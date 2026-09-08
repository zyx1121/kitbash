// Drives the workflow kit over stdio against a fake MCP surface: graphs are
// served by a fake fs_read, steps call fake tools, and the run span lands on a
// fake OTLP receiver.
//
//   bun run deploy/org/workflow/test/run.test.mjs
//
// Both fakes bind an ephemeral port, so nothing here collides with another
// driver. Exit status is 0 when every check passes.

import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";

import { parse as parseYaml } from "yaml";

const kitDir = path.join(import.meta.dirname, "..");
const kit = path.join(kitDir, "index.js");
const manifest = parseYaml(readFileSync(path.join(kitDir, "kitbash.yaml"), "utf8"));
const runTool = manifest.provides.tools.find((tool) => tool.name === "run");
const TOKEN = "tok-workflow";

const results = [];
const check = (name, ok, extra = "") => results.push(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` ${extra}` : ""}`);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// ---------------------------------------------------------------- the fakes

// Graph files the fake fs_read serves.
const files = new Map();
const put = (at, body) => files.set(at, typeof body === "string" ? body : JSON.stringify(body));

put("/org/flows/flat.yaml", `
name: flat
description: Adds a pair and says the total.
input:
  type: object
  required: [a, b]
  properties:
    a: { type: number }
    b: { type: number }
steps:
  - id: total
    tool: server-everything_get-sum
    input:
      a: \${input.a}
      b: \${input.b}
  - id: say
    tool: echo_echo
    input:
      message: "the total is \${steps.total.output.sum}"
    output:
      said: \${result.content.0.text}
output:
  sum: \${steps.total.output.sum}
  said: \${steps.say.output.said}
`);

put("/org/flows/outer.yaml", `
name: outer
steps:
  - id: inner
    workflow: /org/flows/flat.yaml
    input:
      a: \${input.a}
      b: \${input.b}
  - id: report
    tool: echo_echo
    input:
      message: "sum \${steps.inner.output.sum}"
`);

// The same graph written as JSON, which the parser takes as readily as YAML.
put("/org/flows/flat.json", {
  name: "flat json",
  steps: [{ id: "say", tool: "echo_echo", input: { message: "${input.subject}" } }],
  output: { said: "${steps.say.output.text}" },
});

put("/org/flows/later.yaml", `
name: refers to a later step
steps:
  - id: first
    tool: echo_echo
    input:
      message: \${steps.second.output.text}
  - id: second
    tool: echo_echo
    input:
      message: hello
`);

put("/org/flows/loop-a.yaml", `
name: loop a
steps:
  - id: b
    workflow: /org/flows/loop-b.yaml
`);
put("/org/flows/loop-b.yaml", `
name: loop b
steps:
  - id: a
    workflow: /org/flows/loop-a.yaml
`);

// Nine distinct graphs, each running the next: the tenth is past the depth cap.
for (let i = 0; i < 9; i += 1) {
  put(`/org/flows/deep-${i}.yaml`, `name: deep ${i}\nsteps:\n  - id: down\n    workflow: /org/flows/deep-${i + 1}.yaml\n`);
}
put("/org/flows/deep-9.yaml", "name: deep 9\nsteps:\n  - id: done\n    tool: echo_echo\n    input:\n      message: bottom\n");

put("/org/flows/boom.yaml", `
name: a step that fails
steps:
  - id: boom
    tool: boom_boom
    input:
      message: hello
`);

put("/org/flows/missing-tool.yaml", `
name: a step that names a tool nobody runs
steps:
  - id: nope
    tool: ghost_vanish
`);

// Larger than the 256 KiB the engine reads.
put("/org/flows/huge.yaml", `name: huge\ndescription: ${"d".repeat(300 * 1024)}\nsteps:\n  - id: a\n    tool: echo_echo\n`);

put("/org/flows/not-yaml.yaml", "name: broken\nsteps: [ - }\n");

const problem = (slug, status, title, detail) => ({
  isError: true,
  content: [{ type: "text", text: JSON.stringify({ type: `https://kitbash.zyx.tw/errors/${slug}`, title, status, detail, fix: "Do the other thing." }) }],
});

const surfaceTools = [
  { name: "fs_read", description: "Read one file.", inputSchema: { type: "object", required: ["path"], properties: { path: { type: "string" } } } },
  { name: "echo_echo", description: "Echo a message.", inputSchema: { type: "object", required: ["message"], properties: { message: { type: "string" } } } },
  {
    name: "server-everything_get-sum",
    description: "Add two numbers.",
    inputSchema: { type: "object", required: ["a", "b"], properties: { a: { type: "number" }, b: { type: "number" } } },
    outputSchema: { type: "object", required: ["sum"], properties: { sum: { type: "number" } } },
  },
  { name: "boom_boom", description: "Always fails.", inputSchema: { type: "object", properties: { message: { type: "string" } } } },
];

const calls = [];
const authorizations = new Set();
const spans = [];

function callTool(name, args) {
  calls.push({ name, args });
  if (name === "fs_read") {
    const body = files.get(args?.path);
    if (body === undefined) return problem("not-found", 404, "No such file", `${args?.path} does not exist.`);
    return {
      content: [
        { type: "text", text: body },
        { type: "text", text: JSON.stringify({ path: args.path, mediaType: "text/yaml", size: Buffer.byteLength(body, "utf8") }) },
      ],
    };
  }
  if (name === "echo_echo") return { content: [{ type: "text", text: `Echo: ${args?.message}` }] };
  if (name === "server-everything_get-sum") {
    const sum = Number(args?.a) + Number(args?.b);
    return { structuredContent: { sum }, content: [{ type: "text", text: JSON.stringify({ sum }) }] };
  }
  if (name === "boom_boom") return problem("conflict", 409, "Write raced another commit", "boom_boom always fails.");
  return problem("not-found", 404, "Unknown tool", `This surface has no ${name}.`);
}

// A minimal streamable HTTP MCP server: one JSON response per POSTed request,
// 202 for a notification, 405 on GET so the client does not open an SSE stream.
const surface = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === "/v1/traces") {
      authorizations.add(request.headers.get("authorization"));
      const body = await request.json();
      for (const resourceSpans of body.resourceSpans ?? []) {
        for (const scopeSpans of resourceSpans.scopeSpans ?? []) spans.push(...(scopeSpans.spans ?? []));
      }
      return new Response("{}", { headers: { "content-type": "application/json" } });
    }
    if (url.pathname !== "/mcp") return new Response("no", { status: 404 });
    authorizations.add(request.headers.get("authorization"));
    if (request.method === "GET") return new Response("no sse", { status: 405 });
    if (request.method === "DELETE") return new Response(null, { status: 200 });

    const message = await request.json();
    if (message.id === undefined) return new Response(null, { status: 202 });

    let result;
    if (message.method === "initialize") {
      result = { protocolVersion: "2025-06-18", capabilities: { tools: {} }, serverInfo: { name: "fake-surface", version: "0" } };
    } else if (message.method === "tools/list") {
      result = { tools: surfaceTools };
    } else if (message.method === "tools/call") {
      result = callTool(message.params?.name, message.params?.arguments);
    } else {
      return new Response(JSON.stringify({ jsonrpc: "2.0", id: message.id, error: { code: -32601, message: `no ${message.method}` } }), {
        headers: { "content-type": "application/json" },
      });
    }
    return new Response(JSON.stringify({ jsonrpc: "2.0", id: message.id, result }), {
      headers: { "content-type": "application/json", "mcp-session-id": "fake-session" },
    });
  },
});
const base = `http://127.0.0.1:${surface.port}`;

// ---------------------------------------------------------------- the kit

const child = spawn("bun", ["run", kit], {
  stdio: ["pipe", "pipe", "pipe"],
  env: {
    ...process.env,
    KITBASH_MCP_ENDPOINT: `${base}/mcp`,
    KITBASH_TELEMETRY_ENDPOINT: base,
    KITBASH_TELEMETRY_TOKEN: TOKEN,
    KITBASH_PROCESS: "proc-workflow-1",
    KITBASH_PACKAGE: "/org/workflow",
    KITBASH_USER: "alice",
  },
});

let stderr = "";
child.stderr.on("data", (chunk) => (stderr += chunk));

const stdoutLines = [];
const pending = new Map();
let buffer = "";
child.stdout.on("data", (chunk) => {
  buffer += chunk;
  let cut;
  while ((cut = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, cut);
    buffer = buffer.slice(cut + 1);
    if (line.trim() === "") continue;
    stdoutLines.push(line);
    let message;
    try {
      message = JSON.parse(line);
    } catch {
      continue;
    }
    const waiter = message?.id != null ? pending.get(message.id) : undefined;
    if (!waiter) continue;
    pending.delete(message.id);
    waiter(message);
  }
});

let nextId = 0;
const send = (message) => child.stdin.write(`${JSON.stringify(message)}\n`);
const request = (method, params) =>
  new Promise((resolve, reject) => {
    const id = (nextId += 1);
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`${method} did not answer within 30 s`));
    }, 30000);
    pending.set(id, (message) => {
      clearTimeout(timer);
      resolve(message);
    });
    send({ jsonrpc: "2.0", id, method, params });
  });

const runGraph = async (args) => {
  const message = await request("tools/call", { name: "run", arguments: args });
  const result = message.result ?? {};
  const text = result.content?.find((block) => block.type === "text")?.text;
  let parsed;
  try {
    parsed = JSON.parse(text ?? "");
  } catch {
    parsed = undefined;
  }
  return { result, parsed };
};

// ---------------------------------------------------------------- the checks

const initialize = await request("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "run.test", version: "0" } });
check("initialize answers with a server", initialize.result?.serverInfo?.name === "workflow", JSON.stringify(initialize.result?.serverInfo));
send({ jsonrpc: "2.0", method: "notifications/initialized", params: {} });

const listed = (await request("tools/list", {})).result?.tools ?? [];
check("one tool named run is published", listed.length === 1 && listed[0]?.name === "run", JSON.stringify(listed.map((tool) => tool.name)));
check("the published input schema is the manifest's", JSON.stringify(listed[0]?.inputSchema) === JSON.stringify(runTool.input));
check("the published output schema is the manifest's", JSON.stringify(listed[0]?.outputSchema) === JSON.stringify(runTool.output));

const flat = await runGraph({ path: "/org/flows/flat.yaml", input: { a: 2, b: 3 } });
check("a flat graph runs", flat.result?.isError !== true, JSON.stringify(flat.parsed).slice(0, 200));
check("the structured content is the graph output", flat.parsed?.outputs?.sum === 5 && flat.parsed?.outputs?.said === "Echo: the total is 5", JSON.stringify(flat.parsed?.outputs));
check("the result carries structuredContent as well as text", JSON.stringify(flat.result?.structuredContent) === JSON.stringify(flat.parsed));
check("every step is reported once, in order", JSON.stringify(flat.parsed?.steps?.map((step) => [step.id, step.tool, step.status])) === JSON.stringify([["total", "server-everything_get-sum", "ok"], ["say", "echo_echo", "ok"]]), JSON.stringify(flat.parsed?.steps));
check("a step reports a duration", flat.parsed?.steps?.every((step) => typeof step.durationMs === "number"));
check("a whole reference keeps the number's type", calls.some((call) => call.name === "server-everything_get-sum" && call.args.a === 2 && call.args.b === 3), JSON.stringify(calls.find((call) => call.name === "server-everything_get-sum")?.args));
check("a reference inside a string is stringified into it", calls.some((call) => call.name === "echo_echo" && call.args.message === "the total is 5"), JSON.stringify(calls.filter((call) => call.name === "echo_echo").map((call) => call.args)));
check("the path is echoed back", flat.parsed?.path === "/org/flows/flat.yaml");

const asJson = await runGraph({ path: "/org/flows/flat.json", input: { subject: "world" } });
check("a graph written as JSON runs", asJson.parsed?.outputs?.said === "Echo: world", JSON.stringify(asJson.parsed?.outputs));

const nested = await runGraph({ path: "/org/flows/outer.yaml", input: { a: 4, b: 6 } });
check("a graph that runs another graph works", nested.result?.isError !== true, JSON.stringify(nested.parsed).slice(0, 300));
check("the nested graph's output is the step's output", nested.parsed?.outputs?.inner?.sum === 10, JSON.stringify(nested.parsed?.outputs?.inner));
check("the parent sees one entry naming the nested graph", JSON.stringify(nested.parsed?.steps?.map((step) => [step.id, step.tool ?? step.workflow])) === JSON.stringify([["inner", "/org/flows/flat.yaml"], ["report", "echo_echo"]]), JSON.stringify(nested.parsed?.steps));
check("a step after the nested graph reads its output", nested.parsed?.outputs?.report?.text === "Echo: sum 10", JSON.stringify(nested.parsed?.outputs?.report));

const later = await runGraph({ path: "/org/flows/later.yaml" });
check("a graph referencing a later step is refused", later.result?.isError === true && later.parsed?.type?.endsWith("/bad-request"), `${later.parsed?.type} ${later.parsed?.detail}`);
check("the refusal names the step it cannot reach", /second/.test(later.parsed?.detail ?? ""), later.parsed?.detail);
check("nothing was called for the refused graph", !calls.some((call) => call.name === "echo_echo" && call.args?.message === "hello"), JSON.stringify(calls.slice(-1)));

const loop = await runGraph({ path: "/org/flows/loop-a.yaml" });
check("a self referencing chain is refused", loop.result?.isError === true && loop.parsed?.type?.endsWith("/bad-request"), `${loop.parsed?.type} ${loop.parsed?.detail}`);
check("the refusal names the graph that closes the loop", /loop-a\.yaml/.test(loop.parsed?.detail ?? ""), loop.parsed?.detail);

const deep = await runGraph({ path: "/org/flows/deep-0.yaml" });
check("a chain deeper than 8 graphs is refused at depth", deep.result?.isError === true && /8/.test(deep.parsed?.detail ?? ""), `${deep.parsed?.type} ${deep.parsed?.detail}`);

const boom = await runGraph({ path: "/org/flows/boom.yaml" });
check("a tool that errors yields the tool's problem type", boom.result?.isError === true && boom.parsed?.type?.endsWith("/conflict"), `${boom.parsed?.type}`);
check("the problem keeps the tool's status", boom.parsed?.status === 409, `${boom.parsed?.status}`);
check("the problem names the step and the tool", /"boom"/.test(boom.parsed?.detail ?? "") && /boom_boom/.test(boom.parsed?.detail ?? ""), boom.parsed?.detail);
check("the problem instance names the step", boom.parsed?.instance === "/org/flows/boom.yaml#boom", boom.parsed?.instance);

const ghost = await runGraph({ path: "/org/flows/missing-tool.yaml" });
check("a missing tool yields not-found", ghost.result?.isError === true && ghost.parsed?.type?.endsWith("/not-found"), `${ghost.parsed?.type}`);
check("the not-found names the step and the tool", /"nope"/.test(ghost.parsed?.detail ?? "") && /ghost_vanish/.test(ghost.parsed?.detail ?? ""), ghost.parsed?.detail);
check("the missing tool was never called", !calls.some((call) => call.name === "ghost_vanish"));

const huge = await runGraph({ path: "/org/flows/huge.yaml" });
check("an oversize graph is refused", huge.result?.isError === true && huge.parsed?.type?.endsWith("/too-large") && huge.parsed?.status === 413, `${huge.parsed?.type} ${huge.parsed?.status}`);

const gone = await runGraph({ path: "/org/flows/nowhere.yaml" });
check("a graph that is not there yields fs_read's own problem", gone.result?.isError === true && gone.parsed?.type?.endsWith("/not-found"), `${gone.parsed?.type} ${gone.parsed?.detail}`);

const broken = await runGraph({ path: "/org/flows/not-yaml.yaml" });
check("a graph that is not YAML is refused", broken.result?.isError === true && broken.parsed?.type?.endsWith("/bad-request"), `${broken.parsed?.type} ${broken.parsed?.detail}`);

const relative = await runGraph({ path: "flows/flat.yaml" });
check("a relative path is refused", relative.result?.isError === true && relative.parsed?.type?.endsWith("/bad-request"), `${relative.parsed?.type}`);

const missingInput = await runGraph({ path: "/org/flows/flat.yaml", input: { a: 1 } });
check("a missing required input is refused", missingInput.result?.isError === true && /\bb\b/.test(missingInput.parsed?.detail ?? ""), missingInput.parsed?.detail);

const unknownTool = await request("tools/call", { name: "walk", arguments: {} });
check("an unknown tool on this kit is not found", unknownTool.result?.isError === true && JSON.parse(unknownTool.result.content[0].text).type.endsWith("/not-found"));

check("every request carried the bearer token", authorizations.size === 1 && authorizations.has(`Bearer ${TOKEN}`), JSON.stringify([...authorizations]));

// The run span, written after the session closes.
for (let i = 0; i < 40 && spans.length < 2; i += 1) await sleep(50);
const attributesOf = (span) => Object.fromEntries((span.attributes ?? []).map((entry) => [entry.key, entry.value.stringValue ?? entry.value.intValue]));
const first = spans[0] ?? {};
check("one span per run is written", spans.length >= 2, `${spans.length} spans`);
check("the run span is named workflow.run", first.name === "workflow.run", first.name);
check("the run span carries the path and the step count", attributesOf(first)["kitbash.path"] === "/org/flows/flat.yaml" && attributesOf(first)["kitbash.workflow.steps"] === "2", JSON.stringify(attributesOf(first)));
check("a successful run carries no error status", first.status === undefined, JSON.stringify(first.status));
const failed = spans.find((span) => span.status?.code === 2);
check("a failed run carries an error status naming the problem", /bad-request|not-found|too-large|conflict/.test(failed?.status?.message ?? ""), failed?.status?.message?.slice(0, 120));
check("the span has a trace id and a span id", /^[0-9a-f]{32}$/.test(first.traceId ?? "") && /^[0-9a-f]{16}$/.test(first.spanId ?? ""));

check("stdout is JSON-RPC only", stdoutLines.length > 0 && stdoutLines.every((line) => {
  try {
    return JSON.parse(line).jsonrpc === "2.0";
  } catch {
    return false;
  }
}), `${stdoutLines.length} lines`);
check("the kit logs to stderr", /\[workflow\] ready/.test(stderr), stderr.split("\n")[0]);

child.kill("SIGKILL");
surface.stop(true);

console.log(results.join("\n"));
const anyFailed = results.some((line) => line.startsWith("FAIL"));
console.log(anyFailed ? "RESULT: FAIL" : "RESULT: PASS");
if (anyFailed) console.log(`--- stderr ---\n${stderr.slice(-3000)}`);
process.exit(anyFailed ? 1 : 0);
