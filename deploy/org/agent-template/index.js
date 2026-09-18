// agent-template: the kitbash agent kit, and the reference for "every Process
// can be an agent", PLAN.md 2.3.
//
// One tool, run. Given a task in natural language it opens a session back onto
// the MCP surface this Process reaches, gives every tool of that surface to a
// Claude tool use loop, and answers with what the model concluded, one entry per
// tool call it made and what the run cost.
//
// The surface it calls is its owner's, narrowed to the permits in kitbash.yaml,
// reached over streamable HTTP at KITBASH_MCP_ENDPOINT with
// KITBASH_TELEMETRY_TOKEN as the bearer. The model it calls is Anthropic's,
// reached with ANTHROPIC_API_KEY, which is a secret the member sets with
// secrets_set and kitbashd resolves into this container's environment at every
// start: it is in no manifest, in no image and in no request body, and nothing
// this kit writes repeats it.
//
// This folder is a template. A member who wants a different prompt, a different
// model or wider permits copies it into their home, edits AGENT.md, the unit's
// env or provides.permits, and builds their own Package from it; AGENT.md says
// how. That is why the permits here are the narrow ones.
//
// The loop itself is agent.js, which takes the Anthropic client and the MCP
// session as factories so the whole run is driven against fakes in test/.
//
// stdout carries MCP messages only. Everything else goes to stderr.

import { readFileSync } from "node:fs";
import path from "node:path";

import Anthropic from "@anthropic-ai/sdk";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";

import { AgentError, RUN_TOOL, SELF, createAgent } from "./agent.js";

const here = import.meta.dirname;

// The prompt this kit declares in provides.prompt. It is read once: it is part
// of the image and a Process is replaced rather than reloaded.
const prompt = readFileSync(path.join(here, "AGENT.md"), "utf8");
const self = JSON.parse(readFileSync(path.join(here, "package.json"), "utf8"));

// package.json's name is this Package's name, which is what the surface
// publishes this kit's tool under, so a copy of this folder that renames itself
// in both files still recognises its own tool and does not hand it to the
// model. The test that holds package.json and kitbash.yaml to the same name is
// what keeps that true here.
const agent = createAgent({ env: process.env, prompt, version: self.version, self: self.name, Anthropic });

const server = new Server({ name: self.name, version: self.version }, { capabilities: { tools: {} } });

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: RUN_TOOL.name,
      description: RUN_TOOL.description,
      inputSchema: RUN_TOOL.input,
      outputSchema: RUN_TOOL.output,
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  if (request.params.name !== RUN_TOOL.name) {
    return new AgentError({
      type: "https://kitbash.zyx.tw/errors/not-found",
      status: 404,
      title: "Unknown tool",
      detail: `This kit has no tool named ${request.params.name}.`,
      fix: `Call ${RUN_TOOL.name}.`,
    }).toResult();
  }
  try {
    const structuredContent = await agent.run(request.params.arguments);
    return { content: [{ type: "text", text: JSON.stringify(structuredContent) }], structuredContent };
  } catch (err) {
    if (err instanceof AgentError) return err.toResult();
    // Anything else is a bug in this kit rather than an answer, so the caller
    // is told where to read it and nothing of it is repeated here.
    // A stack carries whatever the frame it was thrown from was holding, so it
    // goes through the redactor like everything else this kit writes.
    console.error(agent.redact(`[${SELF}] unhandled failure: ${err?.stack ?? err}`));
    return new AgentError({
      type: "https://kitbash.zyx.tw/errors/internal",
      status: 500,
      title: "The run failed",
      detail: "The run failed for a reason this kit did not anticipate.",
      fix: "Read this kit's stderr with proc_logs, then try the task again.",
    }).toResult();
  }
});

await server.connect(new StdioServerTransport());
console.error(`[${SELF}] ready, version ${self.version}`);
