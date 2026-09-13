// adapter: the CLI to MCP bridge that a generated import-cli Package runs as
// its CMD, PLAN.md 3 and issue #107.
//
// It is a stdio MCP server with no dependency beyond Node 22 itself, because
// the image it ships in is an Alpine that carries the imported CLI and a node
// runtime and nothing else. It speaks the same newline delimited JSON-RPC as
// e2e/fixtures/echo/server.js, which is the transport kitbash execs into every
// container with expose: mcp. Nothing but MCP messages goes to stdout.
//
// The tools it serves come from tools.json next to this file, written by the
// import-cli kit when it drafts the Package and rewritten by the agent after
// refine. This file never reads kitbash.yaml, so a manifest change needs no
// adapter change and the adapter needs no YAML parser.
//
// The tools.json contract, which the kit generates and this file consumes:
//
//   {
//     "binary": "jq",
//     "tools": {
//       "<toolName>": {
//         "inputSchema": {...},                 // JSON schema for tools/list
//         "argv": ["jq"],                       // fixed prefix, binary then subcommand words
//         "options": {                          // input property -> flag
//           "<prop>": {"flag": "--compact-output", "takesValue": false, "type": "boolean"},
//           "<prop2>": {"flag": "--arg", "takesValue": true, "type": "string", "repeat": false}
//         },
//         "positionals": ["filter", "input"],  // input property names, in argv order; a property whose schema has "format": "kitbash-file" names an entry of the reserved input `files` by name, the adapter writes it to a tmp dir and passes the path
//         "stdin": "stdin",                     // input property whose string goes to stdin, or null
//         "outputs": ["output"]                 // positional property names that name files the adapter reads back after the run (base64 in result.files)
//       },
//       "run":   {"inputSchema": {...}, "argv": ["jq"], "options": {}, "positionals": ["args"], "spread": "args", "stdin": "stdin", "outputs": []},
//       "probe": {"inputSchema": {"type":"object","properties":{}}, "probe": true}
//     }
//   }
//
// Two input property names are reserved on every tool. `stdin` is a string fed
// to the command's standard input, and `files` is an array of
// {name, contentBase64} that the adapter decodes into a fresh temporary
// directory which is also the command's working directory. The directory is
// removed after every call, so one call never sees another call's files.
//
// A command that exits non zero is a normal result carrying its exit code,
// because a CLI reporting a failure is an answer and not a transport fault.
// An MCP error is returned only when the call itself is wrong: bad input, an
// unknown tool, a file entry that was never sent, or a size cap exceeded.

import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";

// The protocol version the rest of kitbash speaks, and the one the bridge
// negotiates with when a client asks for nothing else.
const PROTOCOL_VERSION = "2025-06-18";
// What one file, one stdin and one captured stream may weigh, PLAN.md 3 and
// issue #107: 8 MiB each way, inline as base64.
const MAX_BYTES = 8 * 1024 * 1024;
// Base64 is four characters per three bytes, so an encoded string longer than
// this cannot decode to something inside the cap and is rejected before it is
// turned into a Buffer.
const MAX_BASE64 = Math.ceil((MAX_BYTES / 3) * 4) + 4;
// How long one tool call may run before the child is killed.
// IMPORT_CLI_TIMEOUT_MS shortens it, which is how the tests watch a command
// that never exits being killed without waiting two minutes for it.
const RUN_TIMEOUT_MS = Number(process.env.IMPORT_CLI_TIMEOUT_MS) > 0
  ? Number(process.env.IMPORT_CLI_TIMEOUT_MS)
  : 120 * 1000;
// How long each of the three probe commands may run.
const PROBE_TIMEOUT_MS = 20 * 1000;
// The characters a file name may keep once it has been reduced to a basename.
const UNSAFE_NAME = /[^A-Za-z0-9._-]+/g;
const BASE64_ONLY = /^[A-Za-z0-9+/\r\n=]*$/;

const here = import.meta.dirname;

// ---------------------------------------------------------------- errors

// AdapterError is one of the four error classes a Package may claim on the
// surface, carried as the RFC 9457 document the bridge passes through when the
// class and the status agree, internal/bridge/passthrough.go.
class AdapterError extends Error {
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
    if (this.fix) problem.fix = this.fix;
    return { isError: true, content: [{ type: "text", text: JSON.stringify(problem) }] };
  }
}

const badRequest = (detail, fix, instance) =>
  new AdapterError({ slug: "bad-request", status: 400, title: "Bad request", detail, fix, instance });

const notFound = (detail, fix, instance) =>
  new AdapterError({ slug: "not-found", status: 404, title: "Not found", detail, fix, instance });

const tooLarge = (detail, fix, instance) =>
  new AdapterError({ slug: "too-large", status: 413, title: "Too large", detail, fix, instance });

const internal = (detail, fix, instance) =>
  new AdapterError({ slug: "internal", status: 500, title: "Internal error", detail, fix, instance });

// ---------------------------------------------------------------- tools.json

// The manifest of tools this adapter serves. A Package whose tools.json is
// missing or malformed cannot serve anything, so it fails at start where the
// build and the first run will show it rather than on the first call.
let doc;
try {
  doc = JSON.parse(await readFile(path.join(here, "tools.json"), "utf8"));
} catch (err) {
  console.error(`adapter: tools.json is missing or not JSON: ${err.message}`);
  process.exit(1);
}
if (!doc || typeof doc !== "object" || !doc.tools || typeof doc.tools !== "object") {
  console.error("adapter: tools.json must be an object with a tools object");
  process.exit(1);
}
if (typeof doc.binary !== "string" || doc.binary === "") {
  console.error("adapter: tools.json must name the binary it wraps");
  process.exit(1);
}

// ---------------------------------------------------------------- helpers

// A name that came from a caller is reduced to one path component, so a file
// sent as ../../etc/passwd lands in the temporary directory under a harmless
// name and nothing the caller writes escapes it.
function safeName(raw) {
  const base = path.basename(String(raw)).replace(UNSAFE_NAME, "_").replace(/^\.+/, "");
  return base === "" ? "file" : base.slice(0, 128);
}

// One argv word from one JSON scalar. Anything that is not a string, a finite
// number or a boolean has no obvious spelling on a command line, so it is a
// bad request rather than a guess.
function word(prop, value) {
  if (typeof value === "string") return value;
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  if (typeof value === "boolean") return String(value);
  throw badRequest(
    `${prop} must be a string, a number or a boolean, got ${value === null ? "null" : typeof value}.`,
    "Send a scalar for this property.",
  );
}

// Whether a property names one of the entries of the reserved `files` input
// rather than carrying its own value. The kit marks those with the
// kitbash-file format in the tool's own schema.
function namesAFile(spec, prop) {
  return spec.inputSchema?.properties?.[prop]?.format === "kitbash-file";
}

// Cap collects one output stream and stops at the cap rather than growing the
// adapter's heap with whatever the command decided to print.
class Cap {
  constructor(limit = MAX_BYTES) {
    this.limit = limit;
    this.size = 0;
    this.chunks = [];
    this.truncated = false;
  }

  push(chunk) {
    const room = this.limit - this.size;
    if (room <= 0) {
      this.truncated = true;
      return;
    }
    if (chunk.length > room) {
      this.chunks.push(chunk.subarray(0, room));
      this.size = this.limit;
      this.truncated = true;
      return;
    }
    this.chunks.push(chunk);
    this.size += chunk.length;
  }

  text() {
    return Buffer.concat(this.chunks).toString("utf8");
  }
}

// runCommand spawns the child directly, never through a shell, and always
// resolves with what it did: an exit code, a signal when it was killed, and
// both streams capped. A spawn that never started rejects, because that is the
// adapter's own failure and not the command's answer.
function runCommand(command, argv, { cwd, input, timeoutMs, env }) {
  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawn(command, argv, { cwd, env: env ?? process.env, stdio: ["pipe", "pipe", "pipe"] });
    } catch (err) {
      reject(err);
      return;
    }
    const out = new Cap();
    const err = new Cap();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
    }, timeoutMs);
    child.stdout.on("data", (chunk) => out.push(chunk));
    child.stderr.on("data", (chunk) => err.push(chunk));
    // A command that exits before it has read all of stdin closes the pipe,
    // and writing to a closed pipe is that command's choice rather than an
    // error of ours.
    child.stdin.on("error", () => {});
    child.on("error", (spawnError) => {
      clearTimeout(timer);
      reject(spawnError);
    });
    child.on("close", (code, signal) => {
      clearTimeout(timer);
      resolve({
        exitCode: code,
        signal,
        timedOut,
        stdout: out.text(),
        stderr: err.text(),
        truncated: out.truncated || err.truncated,
      });
    });
    child.stdin.end(input ?? "");
  });
}

// ---------------------------------------------------------------- input

// materialiseFiles decodes the reserved `files` input into the call's own
// temporary directory and answers with the map from the name the caller used
// to the path the command will see.
async function materialiseFiles(value, dir) {
  const written = new Map();
  if (value === undefined || value === null) return written;
  if (!Array.isArray(value)) {
    throw badRequest("files must be an array of {name, contentBase64} objects.", "Send files as an array.");
  }
  for (const entry of value) {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
      throw badRequest("every entry of files must be an object with name and contentBase64.", "Fix the files array.");
    }
    const { name, contentBase64 } = entry;
    if (typeof name !== "string" || name === "") {
      throw badRequest("every entry of files must carry a non empty name.", "Name each file.");
    }
    if (typeof contentBase64 !== "string" || !BASE64_ONLY.test(contentBase64)) {
      throw badRequest(`the contents of ${name} must be a base64 string.`, "Encode the file as base64.", name);
    }
    if (contentBase64.length > MAX_BASE64) {
      throw tooLarge(`${name} is larger than the 8 MiB limit for one file.`, "Send a smaller file.", name);
    }
    const bytes = Buffer.from(contentBase64, "base64");
    if (bytes.length > MAX_BYTES) {
      throw tooLarge(`${name} is larger than the 8 MiB limit for one file.`, "Send a smaller file.", name);
    }
    const target = path.join(dir, safeName(name));
    await writeFile(target, bytes);
    written.set(name, target);
  }
  return written;
}

// buildArgv turns one tool call's arguments into the command line the tools.json
// entry describes: the fixed prefix, then every option the input carries, then
// the positionals in the order the kit recorded.
function buildArgv(spec, args, files, outputs) {
  if (!Array.isArray(spec.argv) || spec.argv.length === 0 || spec.argv.some((w) => typeof w !== "string")) {
    throw internal("this tool's argv prefix in tools.json is not a non empty array of strings.", "Fix tools.json.");
  }
  const argv = [...spec.argv];

  for (const [prop, option] of Object.entries(spec.options ?? {})) {
    const value = args[prop];
    if (value === undefined || value === null) continue;
    if (!option || typeof option.flag !== "string") {
      throw internal(`the option ${prop} in tools.json carries no flag.`, "Fix tools.json.", prop);
    }
    // A flag that takes no value is emitted by its presence alone, and a
    // boolean that is false is the caller asking for it not to be there.
    if (!option.takesValue) {
      if (value === false) continue;
      if (option.type === "boolean" && typeof value !== "boolean") {
        throw badRequest(`${prop} must be a boolean.`, "Send true or false.", prop);
      }
      argv.push(option.flag);
      continue;
    }
    if (Array.isArray(value)) {
      const parts = value.map((element) => word(prop, element));
      if (parts.length === 0) continue;
      // A repeatable flag is written once per element, and one that is not
      // repeatable takes the elements as one comma separated value.
      if (option.repeat) {
        for (const part of parts) argv.push(option.flag, part);
      } else {
        argv.push(option.flag, parts.join(","));
      }
      continue;
    }
    argv.push(option.flag, word(prop, value));
  }

  for (const prop of spec.positionals ?? []) {
    const value = args[prop];
    if (value === undefined || value === null) continue;
    // The spread positional is a whole argv tail the caller supplied, which is
    // what the generic run tool is.
    if (spec.spread === prop) {
      if (!Array.isArray(value)) {
        throw badRequest(`${prop} must be an array of strings.`, "Send the arguments as an array.", prop);
      }
      for (const element of value) argv.push(word(prop, element));
      continue;
    }
    // A property that names an input file is replaced by the path that file
    // was written to, and a name nobody sent is a missing file rather than a
    // literal argument.
    if (namesAFile(spec, prop)) {
      const name = word(prop, value);
      const file = files.get(name);
      if (file === undefined) {
        throw notFound(`${prop} names the file ${name}, which was not sent in files.`, "Send that file in files.", name);
      }
      argv.push(file);
      continue;
    }
    // An output is a name the command writes and the adapter reads back, so it
    // is reduced to one component inside the temporary directory.
    if (outputs.has(prop)) {
      argv.push(safeName(word(prop, value)));
      continue;
    }
    argv.push(word(prop, value));
  }

  return argv;
}

// stdinFor is the string the tool sends to the command's standard input, which
// is the property tools.json named and nothing else.
function stdinFor(spec, args) {
  if (typeof spec.stdin !== "string" || spec.stdin === "") return "";
  const value = args[spec.stdin];
  if (value === undefined || value === null) return "";
  if (typeof value !== "string") {
    throw badRequest(`${spec.stdin} must be a string.`, "Send text for stdin.", spec.stdin);
  }
  if (Buffer.byteLength(value) > MAX_BYTES) {
    throw tooLarge(`${spec.stdin} is larger than the 8 MiB limit.`, "Send less input.", spec.stdin);
  }
  return value;
}

// collectOutputs reads back the files the tool declared as outputs. A file the
// command did not write is simply absent, because a command that failed is a
// normal result and an empty files array is the honest report of it.
async function collectOutputs(spec, args, dir) {
  const files = [];
  for (const prop of spec.outputs ?? []) {
    const value = args[prop];
    if (typeof value !== "string" || value === "") continue;
    const name = safeName(value);
    const target = path.join(dir, name);
    let info;
    try {
      info = await stat(target);
    } catch {
      continue;
    }
    if (!info.isFile()) continue;
    if (info.size > MAX_BYTES) {
      throw tooLarge(`the output file ${name} is larger than the 8 MiB limit.`, "Ask for less output.", name);
    }
    files.push({ name, contentBase64: (await readFile(target)).toString("base64") });
  }
  return files;
}

// ---------------------------------------------------------------- tools

// callTool runs one tool in a temporary directory of its own and removes that
// directory whatever happened, so no call leaves state for the next one.
async function callTool(name, args) {
  const spec = doc.tools[name];
  if (!spec) {
    throw notFound(`this Package serves no tool named ${name}.`, "Call tools/list for the tools it has.", name);
  }
  if (args === undefined || args === null) args = {};
  if (typeof args !== "object" || Array.isArray(args)) {
    throw badRequest("arguments must be a JSON object.", "Send the tool's input as an object.", name);
  }
  if (spec.probe) return probe();

  const dir = await mkdtemp(path.join(tmpdir(), "kitbash-cli-"));
  try {
    const files = await materialiseFiles(args.files, dir);
    const outputs = new Set(spec.outputs ?? []);
    const argv = buildArgv(spec, args, files, outputs);
    const input = stdinFor(spec, args);
    let result;
    try {
      result = await runCommand(argv[0], argv.slice(1), {
        cwd: dir,
        input,
        timeoutMs: RUN_TIMEOUT_MS,
      });
    } catch (err) {
      throw internal(`${argv[0]} could not be started: ${err.message}`, "Check that the binary is in the image.", argv[0]);
    }
    const payload = {
      exitCode: result.exitCode,
      stdout: result.stdout,
      stderr: result.stderr,
      files: await collectOutputs(spec, args, dir),
      truncated: result.truncated,
    };
    // A killed command has no exit code, so the signal and the timeout are how
    // the caller learns why.
    if (result.signal) payload.signal = result.signal;
    if (result.timedOut) payload.timedOut = true;
    return { content: [{ type: "text", text: JSON.stringify(payload) }] };
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

// textOf runs one probe command and answers with what it printed. Standard
// error is taken when standard output is blank, because a CLI that prints its
// usage to stderr is the common case and that text is the point of a probe. A
// command that cannot start, or one that printed nothing at all, is an empty
// string.
async function textOf(command, argv, env) {
  let result;
  try {
    result = await runCommand(command, argv, {
      cwd: tmpdir(),
      input: "",
      timeoutMs: PROBE_TIMEOUT_MS,
      env,
    });
  } catch {
    return "";
  }
  if (result.timedOut) return "";
  return result.stdout.trim() === "" ? result.stderr : result.stdout;
}

// probe is what the agent calls after the first build: the three texts the kit
// refines a manifest from, PLAN.md 3. A binary that has no man page, and an
// image with no man command at all, answer with an empty man rather than an
// error, because the refinement works from whichever of the three exists.
async function probe() {
  const binary = doc.binary;
  const help = await textOf(binary, ["--help"]);
  const version = await textOf(binary, ["--version"]);
  const man = await textOf("man", [binary], {
    ...process.env,
    // man pipes through a pager and formats for a terminal unless it is told
    // otherwise, and neither is readable as JSON.
    PAGER: "cat",
    MANPAGER: "cat",
    MANWIDTH: "80",
    TERM: "dumb",
  });
  return { content: [{ type: "text", text: JSON.stringify({ help, version, man }) }] };
}

// listTools is tools.json in the shape tools/list wants, with the schemas the
// kit wrote so that what the surface validates and what the adapter runs come
// from one file.
function listTools() {
  return Object.entries(doc.tools).map(([name, spec]) => {
    const tool = {
      name,
      inputSchema: spec.inputSchema ?? { type: "object", properties: {} },
    };
    if (typeof spec.description === "string" && spec.description !== "") tool.description = spec.description;
    return tool;
  });
}

// ---------------------------------------------------------------- transport

const send = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);
const reply = (id, result) => send({ jsonrpc: "2.0", id, result });
const fail = (id, code, message) => send({ jsonrpc: "2.0", id, error: { code, message } });

// Calls are answered one at a time. A CLI call spawns a process and writes
// files, and running two of them at once would buy nothing for a session that
// asks one question at a time while making the failure modes harder to read.
let queue = Promise.resolve();

async function handle(request) {
  switch (request.method) {
    case "initialize":
      reply(request.id, {
        protocolVersion: request.params?.protocolVersion ?? PROTOCOL_VERSION,
        capabilities: { tools: {} },
        serverInfo: { name: doc.binary, version: String(doc.version ?? "1") },
      });
      return;
    case "tools/list":
      reply(request.id, { tools: listTools() });
      return;
    case "tools/call":
      try {
        reply(request.id, await callTool(request.params?.name, request.params?.arguments));
      } catch (err) {
        if (err instanceof AdapterError) {
          reply(request.id, err.toResult());
          return;
        }
        console.error(`adapter: ${err.stack ?? err.message}`);
        reply(request.id, internal(`the adapter failed: ${err.message}`, "Read the Process log.").toResult());
      }
      return;
    default:
      fail(request.id, -32601, `unknown method ${request.method}`);
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
  queue = queue.then(() => handle(request)).catch((err) => {
    console.error(`adapter: ${err.stack ?? err.message}`);
  });
});

// PID 1 holds stdin open and keeps the container alive, the same way every
// other Package with expose: mcp does.
process.stdin.resume();
