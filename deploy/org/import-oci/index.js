// import-oci: the kitbash import kit for images that already exist in a registry.
//
// One tool, import. Given oci://<registry>/<repository>@sha256:<digest> it asks
// skopeo what that manifest says, checks that the registry answers with the
// digest that was asked for, and hands back the files that make the image a
// Package: kitbash.yaml with one container unit pinned to the digest, and a
// README describing what the registry reported. It pulls no layer, builds
// nothing and writes nothing outside the returned files.
//
// The tool schemas this server advertises are read from kitbash.yaml next to
// this file, so the manifest kitbashd validates against and the schemas the
// server publishes cannot drift apart.
//
// An image declares no tools, so the generated manifest carries no provides
// block and its unit is expose: none. Turning the image into something that
// answers on the surface is the importer's next decision, not this kit's.
//
// stdout carries MCP messages only. Everything else goes to stderr.
//
// IMPORT_OCI_SKOPEO overrides the skopeo command, which is a command line split
// on whitespace so a wrapper script can stand in for skopeo during development.

import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { StringDecoder } from "node:string_decoder";
import { fileURLToPath } from "node:url";

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

const INSPECT_TIMEOUT_MS = 2 * 60 * 1000;
const MAX_DESCRIPTION = 280;
const MIN_DESCRIPTION = 10;
const MAX_PACKAGE_NAME = 64;
// A registry answers an inspect with metadata, not with a layer, but the answer
// still comes from a stranger. Cap it, kill skopeo and fail rather than grow
// this kit's heap.
const MAX_INSPECT_OUTPUT = 4 * 1024 * 1024;
// A Package whose name equals a built in tool family cannot be run, PLAN.md 2.3.
const RESERVED_NAMES = new Set(["fs", "pkg", "proc", "tel", "users", "approvals"]);
const DESCRIPTION_LABELS = ["org.opencontainers.image.description", "org.opencontainers.image.title"];
// What one field of the inspect may contribute to the README or to a log line.
const MAX_FIELD = 128;

const here = path.dirname(fileURLToPath(import.meta.url));

const manifest = parseYaml(await readFile(path.join(here, "kitbash.yaml"), "utf8"));
const selfVersion = JSON.parse(await readFile(path.join(here, "package.json"), "utf8")).version;
const importTool = manifest.provides.tools.find((tool) => tool.name === "import");
if (!importTool) {
  console.error("kitbash.yaml declares no tool named import");
  process.exit(1);
}
const sourcePattern = new RegExp(importTool.input.properties.source.pattern);

// ---------------------------------------------------------------- errors

class ImportError extends Error {
  constructor({ slug, status, title, detail, fix, instance }) {
    super(detail);
    this.slug = slug;
    this.status = status;
    this.title = title;
    this.detail = detail;
    this.fix = fix;
    this.instance = instance;
  }

  toResult() {
    const problem = {
      type: `https://kitbash.zyx.tw/errors/${this.slug}`,
      title: this.title,
      status: this.status,
      detail: this.detail,
    };
    if (this.instance) problem.instance = this.instance;
    problem.fix = this.fix;
    return { isError: true, content: [{ type: "text", text: JSON.stringify(problem) }] };
  }
}

const badRequest = (detail, fix, instance, title = "Source cannot be imported") =>
  new ImportError({ slug: "bad-request", status: 400, title, detail, fix, instance });

const notFound = (detail, fix, instance, title = "Image not found") =>
  new ImportError({ slug: "not-found", status: 404, title, detail, fix, instance });

const internal = (detail, fix, instance, title = "Import failed") =>
  new ImportError({ slug: "internal", status: 500, title, detail, fix, instance });

// ---------------------------------------------------------------- helpers

// Everything a registry reports is a stranger's text on its way into a
// manifest, a README or a log line. Control characters are removed rather than
// escaped, because none of the three has a use for them and a stray escape or
// terminal sequence in a description is a trap for whoever reads it next.
function clip(text, max = MAX_DESCRIPTION) {
  if (typeof text !== "string") return "";
  const flat = text
    .replace(/[\u0000-\u001f\u007f]/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  return flat.length <= max ? flat : `${flat.slice(0, max - 3).trimEnd()}...`;
}

// One field of the inspect, clipped short enough to sit on a README line.
function field(value, fallback = "unknown") {
  const text = clip(value, MAX_FIELD);
  return text === "" ? fallback : text;
}

// Package names in a manifest are ^[a-z0-9]+(-[a-z0-9]+)*$, at most 64 characters.
function packageNameFrom(raw) {
  const name = raw
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, MAX_PACKAGE_NAME)
    .replace(/-+$/, "");
  if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(name)) {
    throw badRequest(
      `No manifest name can be derived from the repository ${raw}.`,
      "Import an image whose last path segment contains at least one letter or digit.",
    );
  }
  return name;
}

function skopeoCommand() {
  const raw = (process.env.IMPORT_OCI_SKOPEO ?? "skopeo").trim();
  const parts = raw.split(/\s+/).filter(Boolean);
  return parts.length > 0 ? parts : ["skopeo"];
}

// ---------------------------------------------------------------- import steps

function parseSource(source) {
  if (typeof source !== "string" || !sourcePattern.test(source)) {
    throw badRequest(
      "source is not oci://<registry>/<repository>@sha256:<digest>.",
      "Call import with a source such as oci://docker.io/library/alpine@sha256:<64 hex characters>.",
      typeof source === "string" ? source : undefined,
    );
  }
  const ref = source.slice("oci://".length);
  const at = ref.lastIndexOf("@");
  const repository = ref.slice(0, at);
  const digest = ref.slice(at + 1);
  const segments = repository.split("/");
  return { ref, repository, digest, lastSegment: segments[segments.length - 1] };
}

// skopeo speaks to the registry over the network, so its output is a stranger's
// and its failure modes are the registry's. Both streams are captured: stdout
// is the JSON document, stderr is the reason a non zero exit happened.
function inspect(ref) {
  const [command, ...leading] = skopeoCommand();
  const args = [...leading, "inspect", "--no-tags", `docker://${ref}`];
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"], env: process.env });
    // chunk is a Buffer, so read counts bytes. The decoder holds back the tail
    // of a multi-byte character that straddles two chunks instead of turning it
    // into U+FFFD; concatenating the Buffers directly would corrupt every
    // description that is not ASCII, and setting an encoding on the stream
    // would silently make the cap below a character count.
    const outDecoder = new StringDecoder("utf8");
    const errDecoder = new StringDecoder("utf8");
    let out = "";
    let err = "";
    let read = 0;
    let overflowed = false;

    child.stdout.on("data", (chunk) => {
      read += chunk.length;
      if (read > MAX_INSPECT_OUTPUT) {
        if (!overflowed) {
          overflowed = true;
          child.kill("SIGKILL");
        }
        return;
      }
      out += outDecoder.write(chunk);
    });
    child.stderr.on("data", (chunk) => {
      err += errDecoder.write(chunk);
      if (err.length > 64 * 1024) err = err.slice(-64 * 1024);
    });

    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(
        internal(
          `Inspecting ${ref} did not finish within ${INSPECT_TIMEOUT_MS / 1000} seconds.`,
          "Retry; if it persists the registry is unreachable from this machine.",
          ref,
        ),
      );
    }, INSPECT_TIMEOUT_MS);

    child.on("error", (error) => {
      clearTimeout(timer);
      console.error(`[import-oci] ${command} failed to start: ${error.message}`);
      reject(internal("skopeo could not be started.", "Report this: the kit image is missing its skopeo."));
    });

    child.on("close", (code) => {
      clearTimeout(timer);
      if (overflowed) {
        console.error(`[import-oci] ${ref} answered with more than ${MAX_INSPECT_OUTPUT} bytes`);
        reject(
          internal(
            `The registry answered for ${ref} with more than ${MAX_INSPECT_OUTPUT} bytes.`,
            "Import an image whose manifest is of a normal size.",
            ref,
          ),
        );
        return;
      }
      if (code === 0) {
        resolve(out + outDecoder.end());
        return;
      }
      err += errDecoder.end();
      console.error(`[import-oci] skopeo exited ${code} for ${ref}:\n${err}`);
      reject(classify(ref, err));
    });
  });
}

// The registry's refusals, mapped onto the three problems this kit reports.
// Everything unrecognized is internal and its output stays in this kit's stderr.
function classify(ref, output) {
  if (/manifest unknown|manifest for .* not found|name unknown|repository name not known|repository does not exist|was not found|not found: (manifest|name)/i.test(output)) {
    return notFound(
      `The registry has no manifest ${ref}.`,
      "Check the repository and pin a digest the registry actually publishes.",
      ref,
    );
  }
  // A Docker Hub image is addressed by its manifest digest. The config digest,
  // which is what a locally built image is named by, parses as a reference but
  // resolves to something skopeo cannot read as a manifest.
  if (/manifest schema unsupported|unsupported manifest|unsupported media ?type|unknown media ?type|invalid reference|unsupported image/i.test(output)) {
    return badRequest(
      `${ref} is not a manifest this kit can import.`,
      "Use the digest skopeo inspect reports for the image, which is the manifest digest, not the image config digest.",
      ref,
    );
  }
  if (/unauthorized|authentication required|denied|forbidden/i.test(output)) {
    return badRequest(
      `The registry refuses to describe ${ref} without credentials.`,
      "Import an image this machine may read anonymously.",
      ref,
    );
  }
  return internal(
    `skopeo could not inspect ${ref}.`,
    "The skopeo output is in this kit's stderr. Retry, or import another image.",
    ref,
  );
}

// ---------------------------------------------------------------- file generation

function describe(inspected, ref) {
  const labels = inspected.Labels && typeof inspected.Labels === "object" && !Array.isArray(inspected.Labels) ? inspected.Labels : {};
  for (const label of DESCRIPTION_LABELS) {
    const description = clip(labels[label]);
    if (description.length >= MIN_DESCRIPTION) return description;
  }
  return clip(`OCI image ${ref} imported from its registry.`);
}

function generateFiles({ ref, repository, digest, lastSegment, inspected }) {
  const packageName = packageNameFrom(lastSegment);
  if (RESERVED_NAMES.has(packageName)) {
    throw badRequest(
      `${repository} would become the Package name ${packageName}, which is a built in tool family.`,
      "Import this image by hand under another folder name.",
      ref,
    );
  }

  const image = `${repository}@${digest}`;
  const manifestDoc = {
    name: packageName,
    description: describe(inspected, ref),
    tags: ["oci"],
    // No provides block: an image declares no tools. A unit that exposes
    // nothing is a batch job until whoever imported it says otherwise.
    deploy: { units: [{ type: "container", image, expose: "none" }] },
  };

  // Three more fields out of the same stranger's document. They are clipped for
  // the same reason the description is: a README line is not a place to paste
  // whatever a registry chose to put in a label.
  const architecture = field(inspected.Architecture);
  const os = field(inspected.Os);
  const created = field(inspected.Created);

  const readme = [
    `# ${packageName}`,
    "",
    `Wraps the OCI image \`${image}\`, imported from its registry by import-oci.`,
    "",
    `- Image: \`${image}\``,
    `- Architecture: ${os}/${architecture}`,
    `- Created: ${created}`,
    "",
    "The digest is the version. Rebuilding is not possible and not needed: the",
    "registry serves the same bytes for the same digest, and kitbash runs them",
    "as they are. To follow a newer release, import that release's digest as a",
    "new Package.",
    "",
    "The unit exposes nothing, because an image declares no tools. Change",
    "`expose` in kitbash.yaml to `http` with a `port`, or to `mcp` if the",
    "entrypoint is a stdio MCP server, and declare the tools it offers.",
    "",
  ].join("\n");

  return [
    {
      path: "kitbash.yaml",
      content: `# Generated by import-oci from oci://${image}.\n${stringifyYaml(manifestDoc, { lineWidth: 0 })}`,
    },
    { path: "README.md", content: readme },
  ];
}

// ---------------------------------------------------------------- the tool

async function importSource(source) {
  const { ref, repository, digest, lastSegment } = parseSource(source);
  const raw = await inspect(ref);

  let inspected;
  try {
    inspected = JSON.parse(raw);
  } catch (err) {
    console.error(`[import-oci] skopeo answered for ${ref} with something that is not JSON: ${err.message}`);
    throw internal(
      `The inspect of ${ref} did not return JSON.`,
      "Retry; if it persists, report this with the kit's stderr.",
      ref,
    );
  }
  if (!inspected || typeof inspected !== "object" || Array.isArray(inspected)) {
    throw internal(`The inspect of ${ref} did not return an image description.`, "Retry, or import another image.", ref);
  }

  // The whole point of importing by digest is that the Package names the bytes
  // that were inspected. A registry that answers with another manifest has
  // answered a different question.
  if (inspected.Digest !== digest) {
    const reported = field(inspected.Digest, "no digest at all");
    console.error(`[import-oci] ${ref} reported digest ${JSON.stringify(reported)}`);
    throw badRequest(
      `The registry describes ${repository} at ${reported}, not at ${digest}, so the import would not be reproducible.`,
      "Import the digest skopeo inspect reports for this image.",
      ref,
    );
  }

  console.error(`[import-oci] ${ref} inspected: ${field(inspected.Os, "?")}/${field(inspected.Architecture, "?")}, created ${field(inspected.Created, "?")}`);
  return generateFiles({ ref, repository, digest, lastSegment, inspected });
}

const server = new Server({ name: "import-oci", version: selfVersion }, { capabilities: { tools: {} } });

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: importTool.name,
      description: importTool.description,
      inputSchema: importTool.input,
      outputSchema: importTool.output,
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  if (request.params.name !== importTool.name) {
    return notFound(
      `This kit has no tool named ${request.params.name}.`,
      `Call ${importTool.name}.`,
      request.params.name,
      "Unknown tool",
    ).toResult();
  }
  try {
    const files = await importSource(request.params.arguments?.source);
    const structuredContent = { files };
    return { content: [{ type: "text", text: JSON.stringify(structuredContent) }], structuredContent };
  } catch (err) {
    if (err instanceof ImportError) return err.toResult();
    console.error(`[import-oci] unhandled failure: ${err?.stack ?? err}`);
    return internal("The import failed for a reason this kit did not anticipate.", "Read this kit's stderr in Telemetry, then retry.").toResult();
  }
});

await server.connect(new StdioServerTransport());
console.error(`[import-oci] ready, version ${selfVersion}`);
