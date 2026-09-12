// runner: the smallest run kit the end to end job can dispatch to.
//
// Three tools: run and stop, which are the run hook of PLAN.md section 3, and
// calls, which hands back what the kit was asked to do. Nothing is started:
// the job is proving that proc_run reached a kit with {package, digest, name,
// unit} and registered what the kit answered, not that a second runtime works.
//
// Newline delimited JSON-RPC on stdin and stdout, the stdio transport kitbash
// execs into the container for every session. The record is a file rather than
// a variable because every session execs one more instance of this server in
// the same container, and the tool that reads the record is not always the
// instance that wrote it.

import { createInterface } from "node:readline";
import { randomBytes } from "node:crypto";
import { appendFileSync, readFileSync } from "node:fs";

const RECORD = "/tmp/runner-calls.jsonl";

const TOOLS = [
  {
    name: "run",
    description: "Start a Process of one Package somewhere this kit owns.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["package", "digest", "name", "unit"],
      properties: {
        package: { type: "string" },
        digest: { type: "string" },
        name: { type: "string" },
        unit: { type: "object" },
      },
    },
  },
  {
    name: "stop",
    description: "Stop a Process this kit owns.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["id"],
      properties: { id: { type: "string" } },
    },
  },
  {
    name: "calls",
    description: "What this kit was asked to do, oldest first.",
    inputSchema: { type: "object", additionalProperties: false, properties: {} },
  },
];

// uuidv7 is the id shape every Process carries: kitbash registers the id a run
// kit answers with, and every other tool addresses the Process by it.
const uuidv7 = () => {
  const b = randomBytes(16);
  const ms = Date.now();
  for (let i = 0; i < 6; i++) b[i] = Math.floor(ms / 2 ** (8 * (5 - i))) & 0xff;
  b[6] = (b[6] & 0x0f) | 0x70;
  b[8] = (b[8] & 0x3f) | 0x80;
  const hex = b.toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
};

const record = (entry) => appendFileSync(RECORD, `${JSON.stringify(entry)}\n`);

const recorded = () => {
  let body = "";
  try {
    body = readFileSync(RECORD, "utf8");
  } catch {
    return [];
  }
  return body
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line));
};

const send = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);
const reply = (id, result) => send({ jsonrpc: "2.0", id, result });
const fail = (id, code, message) => send({ jsonrpc: "2.0", id, error: { code, message } });
const answer = (id, structured) =>
  reply(id, { content: [{ type: "text", text: JSON.stringify(structured) }], structuredContent: structured });

const called = (id, name, args) => {
  switch (name) {
    case "run": {
      const process_id = uuidv7();
      record({ tool: "run", id: process_id, ...args });
      // The endpoint is empty: this kit publishes nothing, and an endpoint it
      // does not serve would be one kitbashd delivers Telemetry to.
      answer(id, { id: process_id, state: "running", endpoint: "" });
      break;
    }
    case "stop":
      record({ tool: "stop", id: args.id });
      answer(id, { id: args.id, state: "stopped" });
      break;
    case "calls":
      answer(id, { calls: recorded() });
      break;
    default:
      fail(id, -32602, `unknown tool ${name}`);
  }
};

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
        serverInfo: { name: "runner", version: "1" },
      });
      break;
    case "tools/list":
      reply(request.id, { tools: TOOLS });
      break;
    case "tools/call":
      called(request.id, request.params?.name, request.params?.arguments ?? {});
      break;
    default:
      fail(request.id, -32601, `unknown method ${request.method}`);
  }
});

// PID 1 holds stdin open and is never spoken to: the session instances are the
// ones kitbash execs. Resuming the stream is what keeps the container alive.
process.stdin.resume();
