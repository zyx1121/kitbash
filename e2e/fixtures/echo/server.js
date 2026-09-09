// echo: the smallest MCP server the end to end job can run as a Process.
//
// One tool, echo, which answers with the text it was given. Newline delimited
// JSON-RPC on stdin and stdout, which is the stdio transport kitbash execs into
// the container for every session. Nothing but MCP messages goes to stdout.

import { createInterface } from "node:readline";

const TOOL = {
  name: "echo",
  description: "Answer with the text that was sent.",
  inputSchema: {
    type: "object",
    additionalProperties: false,
    required: ["text"],
    properties: { text: { type: "string", maxLength: 4096 } },
  },
};

const send = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);
const reply = (id, result) => send({ jsonrpc: "2.0", id, result });
const fail = (id, code, message) => send({ jsonrpc: "2.0", id, error: { code, message } });

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
        serverInfo: { name: "echo", version: "1" },
      });
      break;
    case "tools/list":
      reply(request.id, { tools: [TOOL] });
      break;
    case "tools/call": {
      if (request.params?.name !== TOOL.name) {
        fail(request.id, -32602, `unknown tool ${request.params?.name}`);
        break;
      }
      const text = request.params?.arguments?.text;
      if (typeof text !== "string") {
        reply(request.id, { isError: true, content: [{ type: "text", text: "text is required" }] });
        break;
      }
      reply(request.id, {
        content: [{ type: "text", text: JSON.stringify({ text }) }],
        structuredContent: { text },
      });
      break;
    }
    default:
      fail(request.id, -32601, `unknown method ${request.method}`);
  }
});

// PID 1 holds stdin open and is never spoken to: the session instances are the
// ones kitbash execs. Resuming the stream is what keeps the container alive.
process.stdin.resume();
