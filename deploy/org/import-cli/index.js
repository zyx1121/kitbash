// import-cli: the kitbash import kit for a command line tool that an Alpine
// package installs.
//
// Two tools, because drafting a schema from a help text is a loop and not a
// call. `import` is the hook kitbashd routes on: given cli:apk:<pkg>@<version>
// it answers with a Package that builds and runs without anything having been
// read, carrying two tools of its own, `run` and `probe`. The agent builds it,
// runs it, calls `probe`, and brings what the binary said about itself back to
// `refine`, which answers with the same Package rewritten so that every flag
// and every argument is a schema property.
//
// Neither tool reaches the network and neither runs the binary. `import` needs
// only the package name and the version out of the source string, and `refine`
// reads text the caller already holds. That is what lets this kit hold no
// permits and ask for nothing.
//
// Newline delimited JSON-RPC on stdin and stdout with no dependencies, which is
// the stdio transport kitbash execs into the container for every session.
// Nothing but MCP messages goes to stdout; every log line goes to stderr.
//
// The files it answers with are the five generate.js writes plus adapter.js,
// which is this kit's own file copied into every Package it drafts. The adapter
// is the only thing in a generated Package that is not generated: it reads
// tools.json, turns a tool call into argv, runs the binary and answers with the
// exit code, the two streams and any file the run produced.

import { createInterface } from "node:readline";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseHelp } from "./parse.js";
import { draft, refined } from "./generate.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const VERSION = "0.1.0";

// What a caller may hand to refine. A help text is a page, a man page is a
// chapter, and both arrive as text a stranger produced, so both are bounded.
const MAX_HELP = 512 * 1024;
const MAX_MAN = 4 * 1024 * 1024;
const MAX_VERSION = 16 * 1024;
// What of the help text is quoted back in the Package's NOTES.md. A file
// written through fs is capped at 1 MiB, and the notes carry prose besides.
const MAX_NOTES_HELP = 64 * 1024;

const SOURCE_PATTERN = "^cli:apk:[a-z0-9][a-z0-9_.+-]*(@[A-Za-z0-9._+-]+)?$";

const filesOutput = {
  type: "object",
  required: ["files"],
  properties: {
    files: {
      type: "array",
      minItems: 1,
      items: {
        type: "object",
        required: ["path"],
        properties: {
          path: { type: "string", description: "Relative to the target folder, no leading slash, no dot component." },
          content: { type: "string", description: "UTF-8 text." },
          contentBase64: { type: "string", contentEncoding: "base64" },
        },
      },
    },
  },
};

const sourceProperty = {
  type: "string",
  pattern: SOURCE_PATTERN,
  maxLength: 256,
  description:
    "The tool to wrap, as cli:apk:<pkg> or cli:apk:<pkg>@<version>. The package is an Alpine one, installed with apk into the Package's own image, and the version is the release apk names, which is <pkgver>-r<pkgrel>.",
};

// The two tools, declared once. kitbash.yaml carries the same schemas, and the
// test beside this file checks that the two have not drifted apart.
const TOOLS = [
  {
    name: "import",
    description:
      "Draft a Package around the Alpine package named by source. Returns the files pkg_import writes: a Dockerfile that installs it, a manifest with a run tool and a probe tool, an adapter and notes saying how to finish the import with refine.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["source"],
      properties: { source: sourceProperty },
    },
    outputSchema: filesOutput,
  },
  {
    name: "refine",
    description:
      "Rewrite the draft Package from what its probe tool reported. Reads the help text, the version and the man page, and returns the same files with one tool per subcommand, or one for the binary, each carrying a schema built from the flags.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["source", "help"],
      properties: {
        source: sourceProperty,
        help: {
          type: "string",
          maxLength: MAX_HELP,
          description: "What the Package's probe tool reported for help, which is the binary's own --help output.",
        },
        version: {
          type: "string",
          maxLength: MAX_VERSION,
          description: "What probe reported for version. Optional: without it the notes say the binary reported none.",
        },
        man: {
          type: "string",
          maxLength: MAX_MAN,
          description: "What probe reported for man. Optional: with it the synopsis and the options section fill in what --help left out.",
        },
      },
    },
    outputSchema: filesOutput,
  },
];

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

const notFound = (detail, fix, instance, title = "Not found") =>
  new ImportError({ slug: "not-found", status: 404, title, detail, fix, instance });

const internal = (detail, fix, instance, title = "Import failed") =>
  new ImportError({ slug: "internal", status: 500, title, detail, fix, instance });

// ---------------------------------------------------------------- the adapter

// adapter.js is this kit's own file and is the same in every Package it writes,
// so it is read once and handed out as text. A kit image built without it can
// still answer tools/list, which is why the failure is reported per call rather
// than at start.
let adapterSource = null;
async function adapter() {
  if (adapterSource !== null) return adapterSource;
  try {
    adapterSource = await readFile(path.join(here, "adapter.js"), "utf8");
  } catch (error) {
    console.error(`[import-cli] adapter.js could not be read: ${error.message}`);
    throw internal(
      "This kit's image carries no adapter.js, so the Package it would write could not run.",
      "Report this: the kit image is incomplete. Rebuild the kit from its folder.",
    );
  }
  return adapterSource;
}

// ---------------------------------------------------------------- validation

function requireString(value, name, max, { optional = false } = {}) {
  if (value === undefined || value === null) {
    if (optional) return "";
    throw badRequest(`${name} is required.`, `Call this tool with a ${name} string.`, undefined, "Argument missing");
  }
  if (typeof value !== "string") {
    throw badRequest(`${name} is not a string.`, `Pass ${name} as a string.`, undefined, "Argument rejected");
  }
  if (value.length > max) {
    throw badRequest(
      `${name} is ${value.length} characters, and at most ${max} are read.`,
      `Pass the first ${max} characters of ${name}, which is where a help text says what it has to say.`,
      undefined,
      "Argument rejected",
    );
  }
  return value;
}

// generate.js refuses a source with a plain Error whose message is the detail.
function asBadSource(error, source) {
  return badRequest(
    error.message,
    "Call this tool with a source such as cli:apk:jq@1.8.2-r0, naming a package the Alpine repositories carry.",
    typeof source === "string" ? source.slice(0, 256) : undefined,
  );
}

// ---------------------------------------------------------------- the tools

async function runImport(args) {
  const source = requireString(args?.source, "source", 256);
  let files;
  try {
    files = draft({ source });
  } catch (error) {
    throw asBadSource(error, source);
  }
  files.push({ path: "adapter.js", content: await adapter() });
  console.error(`[import-cli] drafted ${source} as ${files.length} files`);
  return { files };
}

async function runRefine(args) {
  const source = requireString(args?.source, "source", 256);
  const help = requireString(args?.help, "help", MAX_HELP);
  const version = requireString(args?.version, "version", MAX_VERSION, { optional: true });
  const man = requireString(args?.man, "man", MAX_MAN, { optional: true });

  let files;
  let parsed;
  try {
    // The binary is the apk package name, which is what the draft installed and
    // what its Dockerfile and its argv already name.
    const binary = source.slice("cli:apk:".length).split("@")[0];
    parsed = parseHelp({ binary, help, version, man });
    files = refined({ source, parsed, help: help.slice(0, MAX_NOTES_HELP) });
  } catch (error) {
    if (error instanceof ImportError) throw error;
    throw asBadSource(error, source);
  }
  files.push({ path: "adapter.js", content: await adapter() });
  console.error(
    `[import-cli] refined ${source}: style ${parsed.style}, ${parsed.options.length} flags, ${parsed.subcommands.length} subcommands, ${parsed.positionals.length} positionals`,
  );
  return { files };
}

// ---------------------------------------------------------------- the server

const send = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);
const reply = (id, result) => send({ jsonrpc: "2.0", id, result });
const fail = (id, code, message) => send({ jsonrpc: "2.0", id, error: { code, message } });

async function callTool(params) {
  const name = params?.name;
  const args = params?.arguments ?? {};
  try {
    if (name === "import") {
      const structuredContent = await runImport(args);
      return { content: [{ type: "text", text: JSON.stringify(structuredContent) }], structuredContent };
    }
    if (name === "refine") {
      const structuredContent = await runRefine(args);
      return { content: [{ type: "text", text: JSON.stringify(structuredContent) }], structuredContent };
    }
    return notFound(
      `This kit has no tool named ${String(name).slice(0, 64)}.`,
      "Call import to draft a Package, or refine to rewrite it from what probe reported.",
      typeof name === "string" ? name.slice(0, 64) : undefined,
      "Unknown tool",
    ).toResult();
  } catch (error) {
    if (error instanceof ImportError) return error.toResult();
    console.error(`[import-cli] unhandled failure: ${error?.stack ?? error}`);
    return internal(
      "The call failed for a reason this kit did not anticipate.",
      "Read this kit's stderr in Telemetry, then retry.",
    ).toResult();
  }
}

createInterface({ input: process.stdin }).on("line", (line) => {
  if (line.trim() === "") return;
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    return;
  }
  // A notification carries no id and is answered with nothing.
  if (request.id === undefined || request.id === null) return;

  switch (request.method) {
    case "initialize":
      reply(request.id, {
        protocolVersion: request.params?.protocolVersion ?? "2025-06-18",
        capabilities: { tools: {} },
        serverInfo: { name: "import-cli", version: VERSION },
      });
      break;
    case "ping":
      reply(request.id, {});
      break;
    case "tools/list":
      reply(request.id, { tools: TOOLS });
      break;
    case "tools/call":
      callTool(request.params).then(
        (result) => reply(request.id, result),
        (error) => {
          console.error(`[import-cli] handler rejected: ${error?.stack ?? error}`);
          fail(request.id, -32603, "internal error");
        },
      );
      break;
    default:
      fail(request.id, -32601, `unknown method ${request.method}`);
  }
});

// PID 1 holds stdin open and is never spoken to: the session instances are the
// ones kitbash execs. Resuming the stream is what keeps the container alive.
process.stdin.resume();
console.error(`[import-cli] ready, version ${VERSION}`);
