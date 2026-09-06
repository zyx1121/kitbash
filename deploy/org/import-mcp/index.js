// import-mcp: the kitbash import kit for npm MCP servers.
//
// One tool, import. Given npm:<pkg>@<version> it installs the package into a
// temporary prefix, runs its executable as a stdio MCP server, lists the tools
// the server declares, and hands back the files that make the wrapped server a
// Package: kitbash.yaml, a Dockerfile and one schema file per tool input and
// output. It writes nothing outside the temporary prefix and never builds.
//
// The tool schemas this server advertises are read from kitbash.yaml next to
// this file, so the manifest kitbashd validates against and the schemas the
// server publishes cannot drift apart.
//
// Everything the wrapped package does happens inside this kit's own container:
// the install runs third party install scripts and the handshake runs third
// party code. Nothing it produces is trusted. Its stdout is capped, its bin
// name is validated before it becomes a path or a Dockerfile line, and the
// version that lands in the generated Dockerfile is the one npm resolved.
//
// stdout carries MCP messages only. Everything else goes to stderr.
//
// IMPORT_MCP_NPM overrides the npm compatible installer used for the temporary
// install step, and only that step. Default "npm". It is a command line split
// on whitespace, so a wrapper script can stand in for npm during development.

import { spawn } from "node:child_process";
import { constants as fsConstants } from "node:fs";
import { access, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { StringDecoder } from "node:string_decoder";
import { fileURLToPath } from "node:url";

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

const INSTALL_TIMEOUT_MS = 5 * 60 * 1000;
const HANDSHAKE_TIMEOUT_MS = 30 * 1000;
const MAX_DESCRIPTION = 280;
const MIN_DESCRIPTION = 10;
const MAX_PACKAGE_NAME = 64;
const MAX_TOOL_NAME = 63;
const TOOL_NAME = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;
const BIN_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
// A wrapped server may print without end. Cap what is read from it, kill it and
// fail rather than grow the kit's heap with a stranger's output.
const MAX_CHILD_OUTPUT = 4 * 1024 * 1024;
const MAX_TOOL_PAGES = 20;
// The revision spec/mcp-surface.yaml pins.
const MCP_PROTOCOL = "2025-06-18";
// A Package whose name equals a built in tool family cannot be run, PLAN.md 2.3.
const RESERVED_NAMES = new Set(["fs", "pkg", "proc", "tel", "users", "approvals"]);
const SCHEMA_NOTE = {
  input: "the tool is called with an empty object.",
  output: "the tool returns content blocks.",
};

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

const notFound = (detail, fix, instance, title = "Package not found") =>
  new ImportError({ slug: "not-found", status: 404, title, detail, fix, instance });

const internal = (detail, fix, instance, title = "Import failed") =>
  new ImportError({ slug: "internal", status: 500, title, detail, fix, instance });

// ---------------------------------------------------------------- helpers

function clip(text, max = MAX_DESCRIPTION) {
  if (typeof text !== "string") return "";
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length <= max ? flat : `${flat.slice(0, max - 3).trimEnd()}...`;
}

function unscoped(packageName) {
  return packageName.startsWith("@") ? packageName.slice(packageName.indexOf("/") + 1) : packageName;
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
      `No manifest name can be derived from the npm package name ${raw}.`,
      "Import a package whose name contains at least one letter or digit.",
    );
  }
  return name;
}

// A tool is published under the name the server gives it: the manifest pattern
// is the MCP one and the bridge forwards the name unchanged. A name the
// manifest cannot carry is refused, never rewritten.
function toolNameIsPublishable(raw) {
  return typeof raw === "string" && raw.length <= MAX_TOOL_NAME && TOOL_NAME.test(raw);
}

// Schema file names are a filesystem concern, not a naming one. The manifest
// keeps the server's spelling; only the file name is folded to lowercase and
// to [a-z0-9_-] so it is safe on any filesystem and unambiguous on a case
// insensitive one.
function schemaBaseName(toolName) {
  return toolName.toLowerCase().replace(/[^a-z0-9_-]/g, "_");
}

function installerCommand() {
  const raw = (process.env.IMPORT_MCP_NPM ?? "npm").trim();
  const parts = raw.split(/\s+/).filter(Boolean);
  return parts.length > 0 ? parts : ["npm"];
}

// ---------------------------------------------------------------- import steps

function parseSource(source) {
  if (typeof source !== "string" || !sourcePattern.test(source)) {
    throw badRequest(
      "source is not npm:<pkg>@<version>.",
      "Call import with a source such as npm:@modelcontextprotocol/server-everything@2026.8.31.",
      typeof source === "string" ? source : undefined,
    );
  }
  const spec = source.slice("npm:".length);
  const at = spec.lastIndexOf("@");
  if (at <= 0) {
    throw badRequest("source carries no version.", "Pin an exact version, as in npm:some-server@1.2.3.", source);
  }
  return { name: spec.slice(0, at), version: spec.slice(at + 1), spec };
}

function install(prefix, spec) {
  const [command, ...leading] = installerCommand();
  const args = [...leading, "install", "--no-audit", "--no-fund", "--prefix", prefix, spec];
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: ["ignore", "pipe", "pipe"], env: process.env });
    let output = "";
    const collect = (chunk) => {
      output += chunk;
      if (output.length > 64 * 1024) output = output.slice(-64 * 1024);
    };
    child.stdout.on("data", collect);
    child.stderr.on("data", collect);

    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(internal(`Installing ${spec} did not finish within ${INSTALL_TIMEOUT_MS / 1000} seconds.`, "Retry, or import a smaller package."));
    }, INSTALL_TIMEOUT_MS);

    child.on("error", (err) => {
      clearTimeout(timer);
      console.error(`[import-mcp] installer ${command} failed to start: ${err.message}`);
      reject(internal("The installer could not be started.", "Report this: the kit image is missing its npm."));
    });

    child.on("close", (code) => {
      clearTimeout(timer);
      if (code === 0) {
        resolve();
        return;
      }
      console.error(`[import-mcp] ${command} exited ${code} for ${spec}:\n${output}`);
      // npm says E404 or "No matching version found for"; other npm compatible
      // installers report the registry status instead.
      if (/E404|404 Not Found|\s-\s404\b|No matching version|No version matching|failed to resolve|is not in this registry/i.test(output)) {
        reject(notFound(`npm has no ${spec}.`, "Check the package name and pin a version npm actually publishes.", spec));
      } else {
        reject(internal(`Installing ${spec} failed.`, "The installer output is in this kit's stderr. Retry, or import another version."));
      }
    });
  });
}

async function readInstalledPackage(prefix, name) {
  const file = path.join(prefix, "node_modules", ...name.split("/"), "package.json");
  try {
    return JSON.parse(await readFile(file, "utf8"));
  } catch (err) {
    console.error(`[import-mcp] cannot read ${file}: ${err.message}`);
    throw internal("The package installed but its package.json could not be read.", "Retry; if it persists the package is malformed.");
  }
}

// The bin name comes out of a stranger's package.json and becomes both a path
// and a line in the generated Dockerfile, so it is validated before either.
function pickBin(packageJson, name) {
  const short = unscoped(name);
  const bin = packageJson.bin;
  let chosen;
  if (typeof bin === "string") {
    chosen = short;
  } else if (bin && typeof bin === "object" && !Array.isArray(bin)) {
    const keys = Object.keys(bin);
    if (keys.length === 1) chosen = keys[0];
    else if (keys.includes(short)) chosen = short;
    else if (keys.length > 0) chosen = keys[0];
  }
  if (!chosen) {
    throw notFound(
      `${name} declares no executable, so it cannot be a stdio MCP server.`,
      "Import a package that ships a bin entry, or wrap this one by hand.",
      name,
    );
  }
  if (!BIN_NAME.test(chosen) || chosen.length > 128) {
    console.error(`[import-mcp] ${name} declares the bin name ${JSON.stringify(chosen)}`);
    throw notFound(
      `${name} names its executable in a way this kit refuses to turn into a path.`,
      `Import a package whose bin name matches ${BIN_NAME.source}.`,
      name,
    );
  }
  return chosen;
}

// Belt and braces on top of BIN_NAME: the resolved path must still be the one
// file inside the temporary prefix that we expect.
function resolveBinPath(prefix, binName) {
  const root = path.resolve(prefix, "node_modules", ".bin");
  const binPath = path.resolve(root, binName);
  if (binPath !== path.join(root, binName) || !binPath.startsWith(`${root}${path.sep}`)) {
    throw notFound(
      "The executable of this package does not resolve inside the import workspace.",
      "Import a package whose bin name is a plain file name.",
      binName,
    );
  }
  return binPath;
}

function childEnvironment() {
  const env = {};
  for (const key of ["PATH", "HOME", "LANG", "SHELL", "TERM", "TMPDIR", "USER", "LOGNAME"]) {
    if (process.env[key]) env[key] = process.env[key];
  }
  return env;
}

// A stdio JSON-RPC client, just enough for initialize, notifications/initialized
// and tools/list. Hand rolled rather than the SDK client so that every byte the
// wrapped server writes is counted and the process dies at the cap instead of
// growing this kit's heap. It also means a malformed tool entry cannot make the
// whole listing fail: the generator decides what to do with each entry.
function listToolsOf(binPath, prefix, spec) {
  return new Promise((resolve, reject) => {
    const child = spawn(binPath, [], { cwd: prefix, stdio: ["pipe", "pipe", "inherit"], env: childEnvironment() });
    const decoder = new StringDecoder("utf8");
    const pending = new Map();
    let buffer = "";
    let read = 0;
    let nextId = 0;
    let settled = false;

    const finish = (err, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        child.kill("SIGKILL");
      } catch {
        // already gone
      }
      // Nothing will answer the requests still in flight.
      const failure = err ?? internal(`${spec} stopped answering the MCP handshake.`, "Retry the import.", spec);
      for (const waiter of pending.values()) waiter.reject(failure);
      pending.clear();
      if (err) reject(err);
      else resolve(value);
    };

    const timer = setTimeout(
      () =>
        finish(
          internal(
            `${spec} did not finish the MCP handshake within ${HANDSHAKE_TIMEOUT_MS / 1000} seconds.`,
            "Check that the package is a stdio MCP server and needs no configuration to start.",
            spec,
          ),
        ),
      HANDSHAKE_TIMEOUT_MS,
    );

    child.stdin.on("error", () => {
      // the server closed its end; the exit handler reports it
    });

    child.on("error", (err) => {
      console.error(`[import-mcp] cannot run the executable of ${spec}: ${err.message}`);
      finish(internal(`The executable of ${spec} could not be started.`, "Check that the package ships a runnable bin entry.", spec));
    });

    child.on("exit", (code, signal) => {
      finish(
        internal(
          `${spec} exited (code ${code}, signal ${signal}) before it answered the MCP handshake.`,
          "Check that the package starts an MCP server on stdio with no arguments.",
          spec,
        ),
      );
    });

    child.stdout.on("end", () => {
      finish(
        internal(
          `${spec} closed its stdout before it answered the MCP handshake.`,
          "Check that the package starts an MCP server on stdio with no arguments.",
          spec,
        ),
      );
    });

    child.stdout.on("data", (chunk) => {
      read += chunk.length;
      if (read > MAX_CHILD_OUTPUT) {
        console.error(`[import-mcp] ${spec} wrote more than ${MAX_CHILD_OUTPUT} bytes on stdout during the handshake`);
        finish(
          internal(
            `${spec} wrote more than ${MAX_CHILD_OUTPUT} bytes before its tool list arrived.`,
            "Import a server that speaks MCP on stdout and logs on stderr.",
            spec,
          ),
        );
        return;
      }
      // chunk is a Buffer, so read counts bytes. The decoder holds back the tail
      // of a multi-byte character that straddles two chunks instead of turning
      // it into U+FFFD; setting an encoding on the stream would silently make
      // the cap above a character count.
      buffer += decoder.write(chunk);
      let cut;
      while ((cut = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, cut);
        buffer = buffer.slice(cut + 1);
        if (!line.trim()) continue;
        let message;
        try {
          message = JSON.parse(line);
        } catch {
          // a banner or a log line on stdout, not a message we asked for
          continue;
        }
        const waiter = message && message.id != null ? pending.get(message.id) : undefined;
        if (!waiter) continue;
        pending.delete(message.id);
        if (message.error) {
          console.error(`[import-mcp] ${spec} answered ${waiter.method} with ${JSON.stringify(message.error).slice(0, 500)}`);
          waiter.reject(internal(`${spec} answered ${waiter.method} with an error.`, "Check that the package needs no configuration this kit cannot supply.", spec));
        } else {
          waiter.resolve(message.result);
        }
      }
    });

    const send = (message) => {
      try {
        child.stdin.write(`${JSON.stringify(message)}\n`);
      } catch (err) {
        console.error(`[import-mcp] cannot write to ${spec}: ${err.message}`);
      }
    };

    const request = (method, params) =>
      new Promise((resolveRequest, rejectRequest) => {
        const id = (nextId += 1);
        pending.set(id, { method, resolve: resolveRequest, reject: rejectRequest });
        send({ jsonrpc: "2.0", id, method, params });
      });

    (async () => {
      await request("initialize", {
        protocolVersion: MCP_PROTOCOL,
        capabilities: {},
        clientInfo: { name: "import-mcp", version: selfVersion },
      });
      send({ jsonrpc: "2.0", method: "notifications/initialized", params: {} });

      const tools = [];
      let cursor;
      for (let page = 0; page < MAX_TOOL_PAGES; page += 1) {
        const result = await request("tools/list", cursor ? { cursor } : {});
        if (!result || !Array.isArray(result.tools)) {
          throw internal(`${spec} answered tools/list with something that is not a tool list.`, "Import a server that implements the 2025-06-18 MCP revision.", spec);
        }
        tools.push(...result.tools);
        cursor = typeof result.nextCursor === "string" ? result.nextCursor : undefined;
        if (!cursor) break;
        if (page === MAX_TOOL_PAGES - 1) {
          console.error(`[import-mcp] ${spec} still had more tool pages after ${MAX_TOOL_PAGES}; the rest is not imported`);
        }
      }
      return tools;
    })().then(
      (tools) => finish(null, tools),
      (err) => {
        if (err instanceof ImportError) {
          finish(err);
          return;
        }
        console.error(`[import-mcp] handshake with ${spec} failed: ${err?.stack ?? err}`);
        finish(internal(`${spec} did not behave as a stdio MCP server.`, "Check that the package starts an MCP server on stdio with no arguments.", spec));
      },
    );
  });
}

// ---------------------------------------------------------------- file generation

// MCP tool input and output are always JSON objects, and kitbashd validates a
// call against this schema before it reaches the container. A schema that is
// missing, malformed or not an object schema is replaced rather than published,
// because publishing it would make the tool impossible to call.
function objectSchema(schema, what, toolName, spec) {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    return { type: "object", description: `This server declares no ${what} schema; ${SCHEMA_NOTE[what]}` };
  }
  if (schema.type === undefined) return { type: "object", ...schema };
  if (schema.type !== "object") {
    console.error(`[import-mcp] ${spec}: replacing the ${what} schema of ${toolName}, which declares type ${JSON.stringify(schema.type)}`);
    return { type: "object", description: `This server declares a ${what} schema that is not an object schema; ${SCHEMA_NOTE[what]}` };
  }
  return schema;
}

function generateFiles({ name, spec, resolvedSpec, packageJson, binName, tools }) {
  const published = [];
  const names = new Set();
  const baseNames = new Set();
  for (const tool of tools) {
    if (!tool || typeof tool !== "object" || Array.isArray(tool)) {
      console.error(`[import-mcp] refusing a tool entry of ${spec} that is not an object`);
      continue;
    }
    if (!toolNameIsPublishable(tool.name)) {
      console.error(`[import-mcp] refusing tool ${JSON.stringify(tool.name)} of ${spec}: a manifest tool name is ${TOOL_NAME.source}, at most ${MAX_TOOL_NAME} characters`);
      continue;
    }
    if (names.has(tool.name)) {
      console.error(`[import-mcp] refusing duplicate tool ${tool.name} of ${spec}`);
      continue;
    }
    names.add(tool.name);
    let base = schemaBaseName(tool.name);
    for (let n = 2; baseNames.has(base); n += 1) base = `${schemaBaseName(tool.name)}_${n}`;
    baseNames.add(base);
    published.push({ tool, base });
  }

  if (published.length === 0) {
    throw badRequest(
      tools.length === 0
        ? `${spec} declares no tools, so there is nothing to publish.`
        : `None of the ${tools.length} tools of ${spec} has a name a manifest can carry.`,
      `Import a server whose tool names match ${TOOL_NAME.source}, or wrap this one by hand.`,
      spec,
    );
  }

  // Everything in package.json is a stranger's input, including its name.
  const declaredName = typeof packageJson.name === "string" && packageJson.name.length > 0 ? packageJson.name : name;
  const packageName = packageNameFrom(unscoped(declaredName));
  if (RESERVED_NAMES.has(packageName)) {
    throw badRequest(
      `${declaredName} would become the Package name ${packageName}, which is a built in tool family.`,
      "Import this package by hand under another folder name.",
      spec,
    );
  }

  let description = clip(packageJson.description);
  if (description.length < MIN_DESCRIPTION) {
    description = clip(`MCP server ${declaredName} imported from npm. Version ${packageJson.version}.`);
  }

  const files = [];
  const manifestDoc = {
    name: packageName,
    description,
    tags: ["mcp", "npm"],
    provides: {
      tools: published.map(({ tool, base }) => {
        let toolDescription = clip(tool.description);
        if (toolDescription.length < MIN_DESCRIPTION) {
          toolDescription = clip(`Tool ${tool.name} of ${declaredName}.`);
        }
        return {
          name: tool.name,
          description: toolDescription,
          input: { $ref: `schemas/${base}.in.json` },
          output: { $ref: `schemas/${base}.out.json` },
        };
      }),
    },
    deploy: { units: [{ type: "container", build: ".", expose: "mcp" }] },
  };

  files.push({
    path: "kitbash.yaml",
    content: `# Generated by import-mcp from npm:${resolvedSpec}.\n${stringifyYaml(manifestDoc, { lineWidth: 0 })}`,
  });

  // resolvedSpec carries the version npm actually installed, and binName has
  // already been checked against BIN_NAME, but it is still quoted as JSON so
  // nothing a package names can break out of the exec form.
  files.push({
    path: "Dockerfile",
    content: [
      `# Generated by import-mcp from npm:${resolvedSpec}.`,
      "# The entrypoint is a stdio MCP server. kitbash runs it as PID 1 with stdin",
      "# held open and execs one more instance per MCP session.",
      "FROM node:22-alpine",
      `RUN npm install -g --no-audit --no-fund ${resolvedSpec}`,
      `ENTRYPOINT [${JSON.stringify(binName)}]`,
      "",
    ].join("\n"),
  });

  for (const { tool, base } of published) {
    const input = objectSchema(tool.inputSchema, "input", tool.name, spec);
    const output = objectSchema(tool.outputSchema, "output", tool.name, spec);
    files.push({ path: `schemas/${base}.in.json`, content: `${JSON.stringify(input, null, 2)}\n` });
    files.push({ path: `schemas/${base}.out.json`, content: `${JSON.stringify(output, null, 2)}\n` });
  }

  return files;
}

// ---------------------------------------------------------------- the tool

async function importSource(source) {
  const { name, version, spec } = parseSource(source);
  const prefix = await mkdtemp(path.join(tmpdir(), "import-mcp-"));
  try {
    await install(prefix, spec);
    const packageJson = await readInstalledPackage(prefix, name);

    // The generated Package pins what npm resolved, not what the caller typed,
    // so the import is reproducible however the source pattern later changes.
    const resolved = typeof packageJson.version === "string" ? packageJson.version : "";
    if (resolved !== version) {
      console.error(`[import-mcp] ${spec} resolved to version ${JSON.stringify(resolved)}`);
      throw badRequest(
        `${name} resolved to a version this kit did not ask for, so the import would not be reproducible.`,
        "Pin the exact version npm publishes, as in npm:some-server@1.2.3.",
        spec,
      );
    }
    const resolvedSpec = `${name}@${resolved}`;

    const binName = pickBin(packageJson, name);
    const binPath = resolveBinPath(prefix, binName);
    try {
      await access(binPath, fsConstants.X_OK);
    } catch {
      throw notFound(
        `${spec} declares the executable ${binName} but did not install it.`,
        "Import a package whose bin entry points at a file it ships.",
        spec,
      );
    }
    const tools = await listToolsOf(binPath, prefix, spec);
    console.error(`[import-mcp] ${spec} declared ${tools.length} tools`);
    return generateFiles({ name, spec, resolvedSpec, packageJson, binName, tools });
  } finally {
    await rm(prefix, { recursive: true, force: true }).catch((err) => {
      console.error(`[import-mcp] could not remove ${prefix}: ${err.message}`);
    });
  }
}

const server = new Server({ name: "import-mcp", version: selfVersion }, { capabilities: { tools: {} } });

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
    console.error(`[import-mcp] unhandled failure: ${err?.stack ?? err}`);
    return internal("The import failed for a reason this kit did not anticipate.", "Read this kit's stderr in Telemetry, then retry.").toResult();
  }
});

await server.connect(new StdioServerTransport());
console.error(`[import-mcp] ready, version ${selfVersion}`);
