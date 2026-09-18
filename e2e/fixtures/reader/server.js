// reader: the smallest MCP server that proves what a Process can see of Files.
//
// Two tools, read and write, which read and write a path inside the container.
// The paths that matter are the ones the unit's mounts put there, so a read
// that answers with what a member wrote through fs_write is the whole chain,
// and a write that fails on a read only mount is the kernel saying no rather
// than kitbash checking anything.
//
// Newline delimited JSON-RPC on stdin and stdout, the stdio transport kitbash
// execs into the container for every session. Nothing but MCP messages goes to
// stdout.

import { readFileSync, writeFileSync } from "node:fs";
import { createInterface } from "node:readline";

const TOOLS = [
  {
    name: "read",
    description: "Answer with the content of a file the Process can see.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["path"],
      properties: { path: { type: "string", maxLength: 4096 } },
    },
  },
  {
    name: "env",
    description: "Answer with the value of an environment variable the Process was given.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["name"],
      properties: { name: { type: "string", maxLength: 64 } },
    },
  },
  {
    name: "write",
    description: "Write a file where the Process can see it.",
    inputSchema: {
      type: "object",
      additionalProperties: false,
      required: ["path", "content"],
      properties: {
        path: { type: "string", maxLength: 4096 },
        content: { type: "string", maxLength: 4096 },
      },
    },
  },
];

const send = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);
const reply = (id, result) => send({ jsonrpc: "2.0", id, result });
const fail = (id, code, message) => send({ jsonrpc: "2.0", id, error: { code, message } });

// The error of a refused write is the answer this fixture exists for, so it is
// returned as a tool error with the kernel's own message rather than swallowed.
const toolError = (id, text) =>
  reply(id, { isError: true, content: [{ type: "text", text }] });

const call = (id, name, args) => {
  switch (name) {
    case "read": {
      const content = readFileSync(args.path, "utf8");
      reply(id, {
        content: [{ type: "text", text: JSON.stringify({ content }) }],
        structuredContent: { content },
      });
      return;
    }
    case "env": {
      // The environment of this container is what kitbashd wrote into the file
      // it created the container with, so a declared secret is here and
      // nowhere a member's own process could have read it.
      const value = process.env[args.name] ?? "";
      reply(id, {
        content: [{ type: "text", text: JSON.stringify({ value }) }],
        structuredContent: { value },
      });
      return;
    }
    case "write": {
      writeFileSync(args.path, args.content);
      reply(id, {
        content: [{ type: "text", text: JSON.stringify({ written: true }) }],
        structuredContent: { written: true },
      });
      return;
    }
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
        serverInfo: { name: "reader", version: "1" },
      });
      break;
    case "tools/list":
      reply(request.id, { tools: TOOLS });
      break;
    case "tools/call": {
      const name = request.params?.name;
      const args = request.params?.arguments ?? {};
      try {
        call(request.id, name, args);
      } catch (err) {
        toolError(request.id, String(err?.message ?? err));
      }
      break;
    }
    default:
      fail(request.id, -32601, `unknown method ${request.method}`);
  }
});

// PID 1 holds stdin open and is never spoken to: the session instances are the
// ones kitbash execs. Resuming the stream is what keeps the container alive.
process.stdin.resume();
