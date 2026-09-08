// Drives the workflow kit over stdio against a fake MCP surface: graphs are
// served by a fake fs_read, steps call fake tools, and the run span lands on a
// fake OTLP receiver.
//
//   bun run deploy/org/workflow/test/run.test.mjs
//
// The fake surface answers like the acceptance host does: echo_echo and
// server-everything_get-sum return text and no structured content, and
// numbers_add is the one tool here with an output schema.
//
// Everything binds an ephemeral port, so nothing here collides with another
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
const MiB = 1024 * 1024;

const results = [];
const check = (name, ok, extra = "") => results.push(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` ${extra}` : ""}`);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// ---------------------------------------------------------------- the graphs

const files = new Map();
const put = (at, body) => files.set(at, typeof body === "string" ? body : JSON.stringify(body));

put("/org/flows/flat.yaml", `
name: flat
description: Adds a pair and says what came back.
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
      message: "the tool said: \${steps.total.output.text}"
    output:
      said: \${result.content.0.text}
output:
  total: \${steps.total.output.text}
  said: \${steps.say.output.said}
`);

// The one tool here that answers with structured content.
put("/org/flows/structured.yaml", `
name: structured
steps:
  - id: sum
    tool: numbers_add
    input:
      a: \${input.a}
      b: \${input.b}
  - id: say
    tool: echo_echo
    input:
      message: "sum \${steps.sum.output.sum}"
    output:
      said: \${result.structuredContent.missing}
`);

put("/org/flows/structured-default.yaml", `
name: structured default
steps:
  - id: sum
    tool: numbers_add
    input:
      a: \${input.a}
      b: \${input.b}
  - id: say
    tool: echo_echo
    input:
      message: "sum is \${steps.sum.output.sum}"
output:
  sum: \${steps.sum.output.sum}
  said: \${steps.say.output.text}
`);

// The same graph written as JSON, which the parser takes as readily as YAML.
put("/org/flows/flat.json", {
  name: "flat json",
  steps: [{ id: "say", tool: "echo_echo", input: { message: "${input.subject}" } }],
  output: { said: "${steps.say.output.text}" },
});

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
      message: "said \${steps.inner.output.said}"
`);

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

put("/org/flows/prototype.yaml", `
name: reaches for the prototype
steps:
  - id: first
    tool: echo_echo
    input:
      message: \${input.__proto__.polluted}
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

put("/org/flows/shout.yaml", `
name: a step whose failure is enormous
steps:
  - id: shout
    tool: shout_boom
`);

put("/org/flows/liar.yaml", `
name: a step whose problem type is not a URI
steps:
  - id: liar
    tool: liar_boom
`);

// The missing tool is the second step: nothing is called before it is refused.
put("/org/flows/missing-tool.yaml", `
name: a step that names a tool nobody runs
steps:
  - id: before
    tool: echo_echo
    input:
      message: before the ghost
  - id: nope
    tool: ghost_vanish
`);

// Ten steps of 2 MiB: the run's 16 MiB output budget is crossed part way.
put(
  "/org/flows/greedy.yaml",
  `name: greedy\nsteps:\n${Array.from({ length: 10 }, (_, i) => `  - id: blob_${i}\n    tool: fixture_blob\n    input:\n      bytes: ${2 * MiB}\n`).join("")}`,
);

// The two shapes that used to make the engine grow without bound.
put(
  "/org/flows/hundred.yaml",
  `name: a hundred megabytes\nsteps:\n${Array.from({ length: 100 }, (_, i) => `  - id: blob_${i}\n    tool: fixture_blob\n    input:\n      bytes: ${MiB}\n`).join("")}`,
);
put(
  "/org/flows/forty.yaml",
  `name: forty times four megabytes\nsteps:\n${Array.from({ length: 40 }, (_, i) => `  - id: blob_${i}\n    tool: fixture_blob\n    input:\n      bytes: ${4 * MiB}\n`).join("")}`,
);

// One step of 5 MiB: kept clipped at 4 MiB.
put("/org/flows/fat.yaml", `name: fat\nsteps:\n  - id: blob\n    tool: fixture_blob\n    input:\n      bytes: ${5 * MiB}\n`);

// 300 steps in one graph, refused before anything runs.
put("/org/flows/many.yaml", `name: many\nsteps:\n${Array.from({ length: 300 }, (_, i) => `  - id: s${i}\n    tool: echo_echo\n    input:\n      message: "x"\n`).join("")}`);

// Three graphs of a hundred steps each: 256 steps into the chain the run stops.
const hundred = (id) => Array.from({ length: 100 }, (_, i) => `  - id: ${id}${i}\n    tool: echo_echo\n    input:\n      message: "x"\n`).join("");
put("/org/flows/cap-a.yaml", `name: cap a\nsteps:\n${hundred("a")}  - id: down\n    workflow: /org/flows/cap-b.yaml\n`);
put("/org/flows/cap-b.yaml", `name: cap b\nsteps:\n${hundred("b")}  - id: down\n    workflow: /org/flows/cap-c.yaml\n`);
put("/org/flows/cap-c.yaml", `name: cap c\nsteps:\n${hundred("c")}`);

// Larger than the 256 KiB the engine reads.
put("/org/flows/huge.yaml", `name: huge\ndescription: ${"d".repeat(300 * 1024)}\nsteps:\n  - id: a\n    tool: echo_echo\n`);

put("/org/flows/not-yaml.yaml", "name: broken\nsteps: [ - }\n");

// ---------------------------------------------------------------- the surface

const problem = (type, status, title, detail) => ({
  isError: true,
  content: [{ type: "text", text: JSON.stringify({ type, title, status, detail, fix: "Do the other thing." }) }],
});

const kitbashProblem = (slug, status, title, detail) => problem(`https://kitbash.zyx.tw/errors/${slug}`, status, title, detail);

const surfaceTools = [
  { name: "fs_read", description: "Read one file.", inputSchema: { type: "object", required: ["path"], properties: { path: { type: "string" } } } },
  { name: "echo_echo", description: "Echo a message.", inputSchema: { type: "object", required: ["message"], properties: { message: { type: "string" } } } },
  {
    name: "server-everything_get-sum",
    description: "Add two numbers, in words.",
    inputSchema: { type: "object", required: ["a", "b"], properties: { a: { type: "number" }, b: { type: "number" } } },
  },
  {
    name: "numbers_add",
    description: "Add two numbers, with structured content.",
    inputSchema: { type: "object", required: ["a", "b"], properties: { a: { type: "number" }, b: { type: "number" } } },
    outputSchema: { type: "object", required: ["sum"], properties: { sum: { type: "number" } } },
  },
  { name: "fixture_blob", description: "Answer with a blob of text.", inputSchema: { type: "object", required: ["bytes"], properties: { bytes: { type: "number" } } } },
  { name: "boom_boom", description: "Always fails.", inputSchema: { type: "object", properties: { message: { type: "string" } } } },
  { name: "shout_boom", description: "Fails enormously.", inputSchema: { type: "object", properties: {} } },
  { name: "liar_boom", description: "Fails with a type that is not a URI.", inputSchema: { type: "object", properties: {} } },
];

const calls = [];
const authorizations = new Set();
const spans = [];

function callTool(name, args) {
  calls.push({ name, args });
  if (name === "fs_read") {
    const body = files.get(args?.path);
    if (body === undefined) return kitbashProblem("not-found", 404, "No such file", `${args?.path} does not exist.`);
    return {
      content: [
        { type: "text", text: body },
        { type: "text", text: JSON.stringify({ path: args.path, mediaType: "text/yaml", size: Buffer.byteLength(body, "utf8") }) },
      ],
    };
  }
  if (name === "echo_echo") return { content: [{ type: "text", text: `Echo: ${args?.message}` }] };
  // What the real server-everything answers: a sentence, no structured content.
  if (name === "server-everything_get-sum") return { content: [{ type: "text", text: `The sum of ${args?.a} and ${args?.b} is ${Number(args?.a) + Number(args?.b)}.` }] };
  if (name === "numbers_add") {
    const sum = Number(args?.a) + Number(args?.b);
    return { structuredContent: { sum }, content: [{ type: "text", text: JSON.stringify({ sum }) }] };
  }
  if (name === "fixture_blob") return { content: [{ type: "text", text: "b".repeat(Number(args?.bytes) || 0) }] };
  if (name === "boom_boom") return kitbashProblem("conflict", 409, "Write raced another commit", "boom_boom always fails.");
  if (name === "shout_boom") return kitbashProblem("conflict", 409, "T".repeat(5000), "D".repeat(100 * 1024));
  if (name === "liar_boom") return problem("javascript:alert(1)", 400, "Not a problem type", "liar_boom names a type that is not an https URI.");
  return kitbashProblem("not-found", 404, "Unknown tool", `This surface has no ${name}.`);
}

// A minimal streamable HTTP MCP server: one JSON response per POSTed request,
// 202 for a notification, 405 on GET so the client does not open an SSE stream.
// /mcp-429 is the receiver at its session limit, which answers a problem body.
const surface = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  maxRequestBodySize: 64 * MiB,
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
    if (url.pathname === "/mcp-429") {
      return new Response(
        JSON.stringify({
          type: "https://kitbash.zyx.tw/errors/too-many-sessions",
          title: "Too many sessions",
          status: 429,
          detail: "This Process already holds 8 sessions.",
          fix: "End a session and try again.",
        }),
        { status: 429, headers: { "content-type": "application/problem+json" } },
      );
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

function startKit(environment) {
  const child = spawn("bun", ["run", kit], {
    stdio: ["pipe", "pipe", "pipe"],
    env: { ...process.env, KITBASH_TELEMETRY_TOKEN: TOKEN, KITBASH_PROCESS: "proc-workflow-1", KITBASH_PACKAGE: "/org/workflow", KITBASH_USER: "alice", ...environment },
  });

  const state = { stderr: "", stdoutLines: [] };
  child.stderr.on("data", (chunk) => (state.stderr += chunk));

  const pending = new Map();
  let buffer = "";
  child.stdout.on("data", (chunk) => {
    buffer += chunk;
    let cut;
    while ((cut = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, cut);
      buffer = buffer.slice(cut + 1);
      if (line.trim() === "") continue;
      state.stdoutLines.push(line);
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
        reject(new Error(`${method} did not answer within 120 s`));
      }, 120000);
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

  const ready = async () => {
    const initialize = await request("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "run.test", version: "0" } });
    send({ jsonrpc: "2.0", method: "notifications/initialized", params: {} });
    return initialize;
  };

  return { child, state, request, send, runGraph, ready, kill: () => child.kill("SIGKILL") };
}

const kitbash = startKit({ KITBASH_MCP_ENDPOINT: `${base}/mcp`, KITBASH_TELEMETRY_ENDPOINT: base });
const { request, runGraph } = kitbash;

// ---------------------------------------------------------------- the checks

const initialize = await kitbash.ready();
check("initialize answers with a server", initialize.result?.serverInfo?.name === "workflow", JSON.stringify(initialize.result?.serverInfo));

const listed = (await request("tools/list", {})).result?.tools ?? [];
check("one tool named run is published", listed.length === 1 && listed[0]?.name === "run", JSON.stringify(listed.map((tool) => tool.name)));
check("the published input schema is the manifest's", JSON.stringify(listed[0]?.inputSchema) === JSON.stringify(runTool.input));
check("the published output schema is the manifest's", JSON.stringify(listed[0]?.outputSchema) === JSON.stringify(runTool.output));

const flat = await runGraph({ path: "/org/flows/flat.yaml", input: { a: 2, b: 3 } });
check("a flat graph runs", flat.result?.isError !== true, JSON.stringify(flat.parsed).slice(0, 200));
check("a text only tool is read through text", flat.parsed?.outputs?.total === "The sum of 2 and 3 is 5.", JSON.stringify(flat.parsed?.outputs));
check("the next step reads what the first said", flat.parsed?.outputs?.said === "Echo: the tool said: The sum of 2 and 3 is 5.", JSON.stringify(flat.parsed?.outputs?.said));
check("the result carries structuredContent as well as text", JSON.stringify(flat.result?.structuredContent) === JSON.stringify(flat.parsed));
check(
  "every step is reported once, in order",
  JSON.stringify(flat.parsed?.steps?.map((step) => [step.id, step.tool, step.status])) === JSON.stringify([["total", "server-everything_get-sum", "ok"], ["say", "echo_echo", "ok"]]),
  JSON.stringify(flat.parsed?.steps),
);
check("a step reports a duration", flat.parsed?.steps?.every((step) => typeof step.durationMs === "number"));
check("a step that was not clipped says nothing about clipping", flat.parsed?.steps?.every((step) => step.clipped === undefined));
check("a whole reference keeps the number's type", calls.some((call) => call.name === "server-everything_get-sum" && call.args.a === 2 && call.args.b === 3), JSON.stringify(calls.find((call) => call.name === "server-everything_get-sum")?.args));
check("the path is echoed back", flat.parsed?.path === "/org/flows/flat.yaml");

const structured = await runGraph({ path: "/org/flows/structured-default.yaml", input: { a: 40, b: 2 } });
check("a tool with structured content is read through its fields", structured.parsed?.outputs?.sum === 42, JSON.stringify(structured.parsed?.outputs));
check("a reference inside a string is stringified into it", structured.parsed?.outputs?.said === "Echo: sum is 42", structured.parsed?.outputs?.said);

const badMapping = await runGraph({ path: "/org/flows/structured.yaml", input: { a: 1, b: 1 } });
check("an output mapping that references nothing is refused", badMapping.result?.isError === true && /structuredContent\.missing/.test(badMapping.parsed?.detail ?? ""), badMapping.parsed?.detail);

const asJson = await runGraph({ path: "/org/flows/flat.json", input: { subject: "world" } });
check("a graph written as JSON runs", asJson.parsed?.outputs?.said === "Echo: world", JSON.stringify(asJson.parsed?.outputs));

const nested = await runGraph({ path: "/org/flows/outer.yaml", input: { a: 4, b: 6 } });
check("a graph that runs another graph works", nested.result?.isError !== true, JSON.stringify(nested.parsed).slice(0, 300));
check("the nested graph's output is the step's output", nested.parsed?.outputs?.inner?.total === "The sum of 4 and 6 is 10.", JSON.stringify(nested.parsed?.outputs?.inner));
check(
  "the parent sees one entry naming the nested graph",
  JSON.stringify(nested.parsed?.steps?.map((step) => [step.id, step.tool ?? step.workflow])) === JSON.stringify([["inner", "/org/flows/flat.yaml"], ["report", "echo_echo"]]),
  JSON.stringify(nested.parsed?.steps),
);
check("a step after the nested graph reads its output", nested.parsed?.outputs?.report?.text === "Echo: said Echo: the tool said: The sum of 4 and 6 is 10.", JSON.stringify(nested.parsed?.outputs?.report));

const later = await runGraph({ path: "/org/flows/later.yaml" });
check("a graph referencing a later step is refused", later.result?.isError === true && later.parsed?.type?.endsWith("/bad-request"), `${later.parsed?.type} ${later.parsed?.detail}`);
check("the refusal names the step it cannot reach", /second/.test(later.parsed?.detail ?? ""), later.parsed?.detail);
check("nothing was called for the refused graph", !calls.some((call) => call.name === "echo_echo" && call.args?.message === "hello"), JSON.stringify(calls.slice(-1)));

const prototype = await runGraph({ path: "/org/flows/prototype.yaml", input: {} });
check("a reference that reaches for the prototype is refused", prototype.result?.isError === true && /__proto__ is not data/.test(prototype.parsed?.detail ?? ""), prototype.parsed?.detail);

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

const shout = await runGraph({ path: "/org/flows/shout.yaml" });
check("an enormous problem is carried outward clipped", shout.parsed?.title.length === 200 && shout.parsed?.detail.length <= 4096, `${shout.parsed?.title.length} ${shout.parsed?.detail.length}`);
check("the clipped problem keeps its type", shout.parsed?.type?.endsWith("/conflict"), shout.parsed?.type);

const liar = await runGraph({ path: "/org/flows/liar.yaml" });
check("a problem type that is not an https URI is not carried outward", liar.parsed?.type?.endsWith("/internal"), liar.parsed?.type);

const ghost = await runGraph({ path: "/org/flows/missing-tool.yaml" });
check("a missing tool yields not-found", ghost.result?.isError === true && ghost.parsed?.type?.endsWith("/not-found"), `${ghost.parsed?.type}`);
check("the not-found names the step and the tool", /"nope"/.test(ghost.parsed?.detail ?? "") && /ghost_vanish/.test(ghost.parsed?.detail ?? ""), ghost.parsed?.detail);
check("the graph is resolved before its first step runs", !calls.some((call) => call.args?.message === "before the ghost"), JSON.stringify(calls.filter((call) => call.name === "echo_echo").slice(-1)));
check("the missing tool was never called", !calls.some((call) => call.name === "ghost_vanish"));

const fat = await runGraph({ path: "/org/flows/fat.yaml" });
check("a step that returns more than 4 MiB is kept clipped", fat.parsed?.outputs?.blob?.text?.length <= 4 * MiB, `${fat.parsed?.outputs?.blob?.text?.length} characters`);
check("the clipped step says so", fat.parsed?.steps?.[0]?.clipped === true, JSON.stringify(fat.parsed?.steps?.[0]));

const greedy = await runGraph({ path: "/org/flows/greedy.yaml" });
check("a run that keeps more than 16 MiB is refused", greedy.result?.isError === true && greedy.parsed?.type?.endsWith("/too-large"), `${greedy.parsed?.type} ${greedy.parsed?.detail}`);
check("the refusal names the step that crossed the budget", /Step "blob_[0-9]+"/.test(greedy.parsed?.detail ?? ""), greedy.parsed?.detail);

const hundred100 = await runGraph({ path: "/org/flows/hundred.yaml" });
check("a hundred steps of 1 MiB stop at the budget", hundred100.result?.isError === true && hundred100.parsed?.type?.endsWith("/too-large"), hundred100.parsed?.detail);
const forty = await runGraph({ path: "/org/flows/forty.yaml" });
check("forty steps of 4 MiB stop at the budget", forty.result?.isError === true && forty.parsed?.type?.endsWith("/too-large"), forty.parsed?.detail);

const many = await runGraph({ path: "/org/flows/many.yaml" });
check("a graph of 300 steps is refused before it runs", many.result?.isError === true && /256/.test(many.parsed?.detail ?? ""), many.parsed?.detail);

const capped = await runGraph({ path: "/org/flows/cap-a.yaml" });
check("a chain of graphs past 256 steps is refused", capped.result?.isError === true && /256 steps/.test(capped.parsed?.detail ?? ""), capped.parsed?.detail);
check("the run stopped at the 257th step", /would be step 257/.test(capped.parsed?.detail ?? ""), capped.parsed?.detail);

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
check("a nested run counts the nested steps", attributesOf(spans.find((span) => attributesOf(span)["kitbash.path"] === "/org/flows/outer.yaml") ?? {})["kitbash.workflow.steps"] === "4", JSON.stringify(spans.filter((span) => attributesOf(span)["kitbash.path"] === "/org/flows/outer.yaml").map(attributesOf)));
check("a successful run carries no error status", first.status === undefined, JSON.stringify(first.status));
const failed = spans.find((span) => span.status?.code === 2);
check("a failed run carries an error status naming the problem", /bad-request|not-found|too-large|conflict/.test(failed?.status?.message ?? ""), failed?.status?.message?.slice(0, 120));
check("the span has a trace id and a span id", /^[0-9a-f]{32}$/.test(first.traceId ?? "") && /^[0-9a-f]{16}$/.test(first.spanId ?? ""));

check(
  "stdout is JSON-RPC only",
  kitbash.state.stdoutLines.length > 0 &&
    kitbash.state.stdoutLines.every((line) => {
      try {
        return JSON.parse(line).jsonrpc === "2.0";
      } catch {
        return false;
      }
    }),
  `${kitbash.state.stdoutLines.length} lines`,
);
check("the kit logs to stderr", /\[workflow\] ready/.test(kitbash.state.stderr), kitbash.state.stderr.split("\n")[0]);

// The point of the caps above: after runs that returned tens of MiB, the kit
// is nowhere near the 512Mi the manifest declares. VmHWM is the kernel's own
// high water mark, so this reads what actually happened rather than a sample.
let peak;
try {
  peak = Number(readFileSync(`/proc/${kitbash.child.pid}/status`, "utf8").match(/VmHWM:\s+(\d+) kB/)?.[1]) * 1024;
} catch {
  peak = undefined;
}
if (peak) check("peak memory stays well inside the declared 512Mi", peak < 256 * MiB, `${Math.round(peak / MiB)} MiB high water mark`);
kitbash.kill();

// A receiver at its session limit answers a problem, and that problem is what
// the caller is told, not that something is wrong with the environment.
const limited = startKit({ KITBASH_MCP_ENDPOINT: `${base}/mcp-429` });
await limited.ready();
const refused = await limited.runGraph({ path: "/org/flows/flat.yaml" });
check("a refused session carries the receiver's problem", refused.parsed?.type?.endsWith("/too-many-sessions") && refused.parsed?.status === 429, `${refused.parsed?.type} ${refused.parsed?.status}`);
limited.kill();

// An endpoint that is not an http URL is said plainly and not thrown from
// inside. A value with no scheme is the mistake this catches.
const malformed = startKit({ KITBASH_MCP_ENDPOINT: "host.containers.internal:4318/mcp" });
await malformed.ready();
const nowhere = await malformed.runGraph({ path: "/org/flows/flat.yaml" });
check("an endpoint with no scheme is a clear problem", nowhere.parsed?.type?.endsWith("/internal") && /not an http URL/.test(nowhere.parsed?.detail ?? ""), nowhere.parsed?.detail);
malformed.kill();

const notAUrl = startKit({ KITBASH_MCP_ENDPOINT: "not a url at all" });
await notAUrl.ready();
const nonsense = await notAUrl.runGraph({ path: "/org/flows/flat.yaml" });
check("an endpoint that is not a URL is the same clear problem", nonsense.parsed?.type?.endsWith("/internal") && /not an http URL/.test(nonsense.parsed?.detail ?? ""), nonsense.parsed?.detail);
notAUrl.kill();

surface.stop(true);

console.log(results.join("\n"));
const anyFailed = results.some((line) => line.startsWith("FAIL"));
console.log(anyFailed ? "RESULT: FAIL" : "RESULT: PASS");
if (anyFailed) console.log(`--- stderr ---\n${kitbash.state.stderr.slice(-3000)}`);
process.exit(anyFailed ? 1 : 0);
