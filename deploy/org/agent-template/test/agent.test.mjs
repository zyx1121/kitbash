// Drives agent-template's run against fakes: a fake MCP surface in place of the
// one this Process would reach, and a fake Anthropic client whose tool runner
// calls the tools the kit built from that surface.
//
//   cd deploy/org/agent-template && npm test
//   cd deploy/org/agent-template/test && node --test
//
// No key, no network and no container: the kit takes both clients as factories
// for exactly this. Every guard in agent.js has a test here that fails if the
// guard is removed, and the last group holds the schemas the server publishes to
// the manifest the surface validates against.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";

import { parse as parseYaml } from "yaml";

import {
  AgentError,
  RUN_TOOL,
  clip,
  createAgent,
  readConfig,
  redactor,
  resultText,
  surfaceTools,
  systemBlocks,
} from "../agent.js";

const kitDir = path.join(import.meta.dirname, "..");
const manifest = parseYaml(readFileSync(path.join(kitDir, "kitbash.yaml"), "utf8"));
const manifestTool = manifest.provides.tools.find((tool) => tool.name === "run");

const KEY = "sk-ant-test-0123456789abcdef";
const TOKEN = "kitbash-process-token-0123456789";
const KiB = 1024;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// The environment kitbashd would have given a Process of this Package.
const environment = (extra = {}) => ({
  ANTHROPIC_API_KEY: KEY,
  KITBASH_MCP_ENDPOINT: "http://host.containers.internal:4318/mcp",
  KITBASH_TELEMETRY_TOKEN: TOKEN,
  AGENT_MODEL: "claude-opus-5",
  AGENT_MAX_ITERATIONS: "32",
  AGENT_EFFORT: "high",
  ...extra,
});

// ---------------------------------------------------------------- the fakes

// A surface that publishes the tools it was given and answers tools/call from a
// table, recording every call it received.
function fakeSurface({ tools, answers = {}, listThrows }) {
  const state = { calls: [], opened: 0, closed: 0 };
  const make = async () => {
    state.opened += 1;
    return {
      client: {
        listTools: async () => {
          if (listThrows) throw listThrows;
          return { tools };
        },
        callTool: async ({ name, arguments: args }) => {
          state.calls.push({ name, arguments: args });
          const answer = answers[name];
          if (typeof answer === "function") return answer(args);
          return answer ?? { content: [{ type: "text", text: `${name} answered` }] };
        },
      },
      close: async () => {
        state.closed += 1;
      },
    };
  };
  return { make, state };
}

// A tool runner that replays a plan: each entry yields one message and then runs
// the tool calls that message asked for, which is the order the real runner
// works in. It records the params it was built with, so a test reads what the
// kit asked the API for.
function fakeAnthropic({ plan, throws }) {
  const state = { params: undefined, pushed: [], options: undefined, doneCalled: 0 };
  const toolRunner = (params) => {
    state.params = params;
    const seen = [];
    const runner = {
      pushMessages: (...messages) => state.pushed.push(...messages),
      async *[Symbol.asyncIterator]() {
        if (throws) throw throws;
        for (const step of plan) {
          seen.push(step.message);
          yield step.message;
          for (const call of step.calls ?? []) {
            const tool = params.tools.find((entry) => entry.name === call.name);
            assert.ok(tool, `the kit published no tool named ${call.name}`);
            await tool.run(call.input);
          }
        }
      },
    };
    if (!plan?.noDone) {
      runner.done = async () => {
        state.doneCalled += 1;
        return seen.at(-1);
      };
    }
    return runner;
  };
  return { client: { beta: { messages: { toolRunner } } }, state };
}

const message = (props) => ({
  role: "assistant",
  stop_reason: "end_turn",
  content: [],
  usage: { input_tokens: 10, output_tokens: 5 },
  ...props,
});

const text = (body) => ({ type: "text", text: body });

// The two tools the fake surface publishes in most of these tests.
const LISTED = [
  {
    name: "fs_read",
    description: "Read a file of Files.",
    inputSchema: { type: "object", additionalProperties: false, required: ["path"], properties: { path: { type: "string" } } },
  },
  {
    name: "notes_summarise",
    description: "Summarise a note, a tool of another Process.",
    inputSchema: { type: "object", properties: { body: { type: "string" } } },
  },
];

// One agent wired to fakes, with the plan the fake model follows.
function harness({ env = {}, tools = LISTED, answers, plan = [{ message: message({ content: [text("done")] }) }], throws, listThrows } = {}) {
  const surface = fakeSurface({ tools, answers, listThrows });
  const anthropic = fakeAnthropic({ plan, throws });
  const logged = [];
  const agent = createAgent({
    env: environment(env),
    prompt: "AGENT.md says this.",
    version: "0.1.0",
    makeAnthropic: () => anthropic.client,
    makeSurface: surface.make,
    log: (line) => logged.push(line),
  });
  return { agent, surface, anthropic, logged };
}

// ---------------------------------------------------------------- the surface becomes tools

test("every tool of the surface becomes a tool with the same name and the same schema", () => {
  const built = surfaceTools({ listed: LISTED, callTool: async () => ({}), steps: [] });
  assert.deepEqual(
    built.map((tool) => tool.name),
    ["fs_read", "notes_summarise"],
  );
  assert.deepEqual(built[0].input_schema, LISTED[0].inputSchema);
  assert.deepEqual(built[1].input_schema, LISTED[1].inputSchema);
  assert.equal(built[0].description, "Read a file of Files.");
  assert.equal(built[0].type, "custom");
});

test("a tool the surface published without a schema or a description is still callable", () => {
  const built = surfaceTools({
    listed: [{ name: "odd_tool" }, { name: "stringy_tool", inputSchema: { type: "string" } }],
    callTool: async () => ({}),
    steps: [],
  });
  assert.deepEqual(built[0].input_schema, { type: "object" });
  assert.deepEqual(built[1].input_schema, { type: "object" });
  assert.match(built[0].description, /carries no description/);
});

// ---------------------------------------------------------------- a round trip

test("a tool call reaches the surface and comes back as the result and a step", async () => {
  const { agent, surface, anthropic } = harness({
    answers: {
      fs_read: async () => {
        await sleep(12);
        return { content: [text("the file said hello")] };
      },
    },
    plan: [
      {
        message: message({ stop_reason: "tool_use", content: [{ type: "tool_use", id: "t1", name: "fs_read", input: { path: "/home/you/note.md" } }] }),
        calls: [{ name: "fs_read", input: { path: "/home/you/note.md" } }],
      },
      { message: message({ content: [text("the note says hello")], usage: { input_tokens: 40, output_tokens: 7, cache_read_input_tokens: 100 } }) },
    ],
  });

  const answer = await agent.run({ task: "Read the note." });

  assert.deepEqual(surface.state.calls, [{ name: "fs_read", arguments: { path: "/home/you/note.md" } }]);
  assert.equal(answer.answer, "the note says hello");
  assert.equal(answer.stopReason, "end_turn");
  assert.equal(answer.model, "claude-opus-5");
  assert.equal(answer.steps.length, 1);
  assert.equal(answer.steps[0].tool, "fs_read");
  assert.equal(answer.steps[0].ok, true);
  assert.ok(answer.steps[0].durationMs >= 1, `expected a duration, read ${answer.steps[0].durationMs}`);
  // Two turns summed, with the cache read counted as input.
  assert.deepEqual(answer.usage, { inputTokens: 150, outputTokens: 12 });
  assert.equal(anthropic.state.doneCalled, 1);
  assert.equal(surface.state.closed, 1);
});

test("a tool that answers only structured content is read as JSON", async () => {
  const { agent } = harness({
    answers: { notes_summarise: () => ({ structuredContent: { summary: "short" } }) },
    plan: [
      { message: message({ stop_reason: "tool_use", content: [] }), calls: [{ name: "notes_summarise", input: { body: "long" } }] },
      { message: message({ content: [text("summarised")] }) },
    ],
  });
  const answer = await agent.run({ task: "Summarise it." });
  assert.equal(answer.steps[0].ok, true);
  assert.equal(resultText({ structuredContent: { summary: "short" } }), '{"summary":"short"}');
});

test("the run closes its session even when the loop fails", async () => {
  const { agent, surface } = harness({ throws: Object.assign(new Error("boom"), { status: 500 }) });
  await assert.rejects(() => agent.run({ task: "Anything." }), AgentError);
  assert.equal(surface.state.closed, 1);
});

// ---------------------------------------------------------------- a tool that fails

test("a tool that answers a problem is a failed step and an answer the model reads", async () => {
  const steps = [];
  const built = surfaceTools({
    listed: LISTED,
    steps,
    callTool: async () => ({ isError: true, content: [text('{"type":"not-found","detail":"no such file"}')] }),
  });
  const result = await built[0].run({ path: "/home/you/missing.md" });
  assert.match(result, /^This tool call failed and did nothing\./);
  assert.match(result, /no such file/);
  assert.equal(steps[0].ok, false);
  assert.equal(steps[0].tool, "fs_read");
});

test("a tool that throws is a failed step rather than a failed run", async () => {
  const steps = [];
  const built = surfaceTools({
    listed: LISTED,
    steps,
    callTool: async () => {
      throw new Error("the session went away");
    },
  });
  const result = await built[0].run({ path: "/home/you/note.md" });
  assert.match(result, /^This tool call failed and did nothing\./);
  assert.match(result, /the session went away/);
  assert.equal(steps[0].ok, false);
});

// ---------------------------------------------------------------- clipping

test("a result over 256 KiB is clipped and says that it was", async () => {
  const huge = "x".repeat(300 * KiB);
  const steps = [];
  const built = surfaceTools({ listed: LISTED, steps, callTool: async () => ({ content: [text(huge)] }) });
  const result = await built[0].run({ path: "/home/you/big.log" });
  assert.match(result, /agent-template clipped this result/);
  assert.ok(result.length < huge.length, "the result was not clipped");
  assert.equal(result.slice(0, 256 * KiB), "x".repeat(256 * KiB));
  assert.equal(steps[0].ok, true);
});

test("a result inside the cap is passed through untouched", () => {
  const body = "y".repeat(256 * KiB);
  assert.equal(clip(body), body);
});

// ---------------------------------------------------------------- how a run ends

test("a refusal is an empty answer and the refusal stop reason", async () => {
  const { agent, logged } = harness({
    plan: [{ message: message({ stop_reason: "refusal", content: [text("ignored")], stop_details: { category: "cyber", explanation: "no" } }) }],
  });
  const answer = await agent.run({ task: "Something the model declines." });
  assert.equal(answer.answer, "");
  assert.equal(answer.stopReason, "refusal");
  assert.ok(logged.some((line) => line.includes("declined the task for cyber")), logged.join("\n"));
});

test("running out of iterations is max_iterations and keeps what was said", async () => {
  const plan = [
    { message: message({ stop_reason: "tool_use", content: [text("still working")] }) },
    { message: message({ stop_reason: "tool_use", content: [text("still working")] }) },
  ];
  const { agent } = harness({ env: { AGENT_MAX_ITERATIONS: "2" }, plan });
  const answer = await agent.run({ task: "More than two turns of work." });
  assert.equal(answer.stopReason, "max_iterations");
  assert.equal(answer.answer, "still working");
});

test("a final message cut by max_tokens says so", async () => {
  const { agent } = harness({ plan: [{ message: message({ stop_reason: "max_tokens", content: [text("half an ans")] }) }] });
  const answer = await agent.run({ task: "Write something long." });
  assert.equal(answer.stopReason, "max_tokens");
  assert.equal(answer.answer, "half an ans");
});

test("a paused turn is left to the runner, which resumes it itself", async () => {
  const plan = [
    { message: message({ stop_reason: "pause_turn", content: [text("paused")] }) },
    { message: message({ content: [text("finished")] }) },
  ];
  const { agent, anthropic, logged } = harness({ plan });
  const answer = await agent.run({ task: "A long turn." });
  assert.equal(answer.answer, "finished");
  assert.equal(answer.stopReason, "end_turn");
  // The installed SDK pushes the paused message back itself; pushing it here as
  // well would send the turn twice.
  assert.deepEqual(anthropic.state.pushed, []);
  assert.ok(logged.some((line) => line.includes("paused")), logged.join("\n"));
});

test("a runner that exposes no done() falls back to the last message it yielded", async () => {
  const plan = [{ message: message({ content: [text("from the iterator")] }) }];
  plan.noDone = true;
  const { agent, anthropic } = harness({ plan });
  const answer = await agent.run({ task: "Anything." });
  assert.equal(answer.answer, "from the iterator");
  assert.equal(anthropic.state.doneCalled, 0);
});

// ---------------------------------------------------------------- what the API is asked for

test("the request carries adaptive thinking, the effort, the fallbacks and no forced tool choice", async () => {
  const { agent, anthropic } = harness();
  await agent.run({ task: "Anything.", system: "Answer in one line." });
  const params = anthropic.state.params;
  assert.deepEqual(params.thinking, { type: "adaptive" });
  assert.deepEqual(params.output_config, { effort: "high" });
  assert.equal(params.max_tokens, 16000);
  assert.equal(params.max_iterations, 32);
  assert.equal(params.model, "claude-opus-5");
  assert.deepEqual(params.betas, ["server-side-fallback-2026-07-01"]);
  assert.equal(params.fallbacks, "default");
  assert.equal(params.tool_choice, undefined);
  assert.equal(params.thinking.budget_tokens, undefined);
  assert.deepEqual(params.messages, [{ role: "user", content: "Anything." }]);
  assert.equal(params.tools.length, LISTED.length);
});

test("the system prompt is this kit's paragraph, then AGENT.md, then the caller's text, and only the last block is cached", () => {
  const blocks = systemBlocks({ prompt: "AGENT.md says this.", extra: "Answer in one line." });
  assert.equal(blocks.length, 3);
  assert.match(blocks[0].text, /agent running as a Process on a kitbash machine/);
  assert.match(blocks[0].text, /Files paths are absolute/);
  assert.match(blocks[0].text, /Read before you write/);
  assert.match(blocks[0].text, /not a narration/);
  assert.equal(blocks[1].text, "AGENT.md says this.");
  assert.equal(blocks[2].text, "Answer in one line.");
  assert.equal(blocks[0].cache_control, undefined);
  assert.equal(blocks[1].cache_control, undefined);
  assert.deepEqual(blocks[2].cache_control, { type: "ephemeral" });
});

test("a run with no caller system text still caches its last block", () => {
  const blocks = systemBlocks({ prompt: "AGENT.md says this." });
  assert.equal(blocks.length, 2);
  assert.deepEqual(blocks[1].cache_control, { type: "ephemeral" });
});

// ---------------------------------------------------------------- the environment

test("a Process with no ANTHROPIC_API_KEY answers a problem and opens no session", async () => {
  const { agent, surface } = harness({ env: { ANTHROPIC_API_KEY: "" } });
  const err = await agent.run({ task: "Anything." }).then(
    () => undefined,
    (thrown) => thrown,
  );
  assert.ok(err instanceof AgentError, `expected a problem, got ${err}`);
  assert.equal(err.status, 403);
  assert.match(err.type, /not-permitted$/);
  assert.equal(err.fix, "Call secrets_set ANTHROPIC_API_KEY, then proc_stop and proc_run this Package.");
  assert.equal(surface.state.opened, 0);
  // It is a result of the tool, not a crash: the problem carries a fix and the
  // process is still here to answer the next call.
  const asResult = err.toResult();
  assert.equal(asResult.isError, true);
  assert.ok(JSON.parse(asResult.content[0].text).fix.includes("secrets_set"));
});

test("a Process with no MCP endpoint says so rather than calling the model", async () => {
  const { agent, anthropic } = harness({ env: { KITBASH_MCP_ENDPOINT: "" } });
  await assert.rejects(() => agent.run({ task: "Anything." }), (err) => err instanceof AgentError && /internal$/.test(err.type));
  assert.equal(anthropic.state.params, undefined);
});

test("a surface that publishes nothing is a problem naming the permits", async () => {
  const { agent } = harness({ tools: [] });
  await assert.rejects(
    () => agent.run({ task: "Anything." }),
    (err) => err instanceof AgentError && /permits/.test(err.fix),
  );
});

test("an environment value this kit cannot use is ignored for the default", () => {
  const config = readConfig({ AGENT_MODEL: "", AGENT_EFFORT: "enormous", AGENT_MAX_ITERATIONS: "nine" });
  assert.equal(config.model, "claude-opus-5");
  assert.equal(config.effort, "high");
  assert.equal(config.maxIterations, 32);
  assert.deepEqual(config.ignored, ["AGENT_EFFORT", "AGENT_MAX_ITERATIONS"]);
});

test("an environment value this kit can use is the one that runs", () => {
  const config = readConfig({ AGENT_MODEL: "claude-sonnet-5", AGENT_EFFORT: "low", AGENT_MAX_ITERATIONS: "8" });
  assert.equal(config.model, "claude-sonnet-5");
  assert.equal(config.effort, "low");
  assert.equal(config.maxIterations, 8);
  assert.deepEqual(config.ignored, []);
});

test("an iteration count outside what one run may spend falls back to the default", () => {
  assert.equal(readConfig({ AGENT_MAX_ITERATIONS: "0" }).maxIterations, 32);
  assert.equal(readConfig({ AGENT_MAX_ITERATIONS: "4096" }).maxIterations, 32);
});

// ---------------------------------------------------------------- what the API answers

test("a 401 from the API is not-permitted and carries neither the key nor the token", async () => {
  const { agent } = harness({ throws: Object.assign(new Error(`401 invalid x-api-key ${KEY}`), { status: 401 }) });
  const err = await agent.run({ task: "Anything." }).then(
    () => undefined,
    (thrown) => thrown,
  );
  assert.ok(err instanceof AgentError);
  assert.equal(err.status, 403);
  assert.match(err.type, /not-permitted$/);
  const body = JSON.stringify(err.toResult());
  assert.ok(!body.includes(KEY), body);
  assert.ok(!body.includes(TOKEN), body);
  assert.ok(body.includes("secrets_set"));
});

test("a 400 repeats the API's reading of the request with every secret removed", async () => {
  const throws = Object.assign(new Error("400 bad request"), {
    status: 400,
    error: { error: { message: `max_tokens is too large, and the key ${KEY} with the token ${TOKEN} should never be here` } },
  });
  const { agent } = harness({ throws });
  const err = await agent.run({ task: "Anything." }).then(
    () => undefined,
    (caught) => caught,
  );
  assert.ok(err instanceof AgentError);
  assert.equal(err.status, 400);
  assert.match(err.detail, /max_tokens is too large/);
  const body = JSON.stringify(err.toResult());
  assert.ok(!body.includes(KEY), body);
  assert.ok(!body.includes(TOKEN), body);
  assert.equal(body.split("[redacted]").length - 1, 2);
});

test("a 429 and a 5xx are internal and say to try again", async () => {
  for (const status of [429, 503]) {
    const { agent } = harness({ throws: Object.assign(new Error(`${status}`), { status }) });
    const err = await agent.run({ task: "Anything." }).then(
      () => undefined,
      (caught) => caught,
    );
    assert.ok(err instanceof AgentError, `${status} did not answer a problem`);
    assert.equal(err.status, 500);
    assert.match(err.fix, /Try again/);
  }
});

test("a connection failure with no status is internal", async () => {
  const { agent } = harness({ throws: new Error("Connection error.") });
  const err = await agent.run({ task: "Anything." }).then(
    () => undefined,
    (caught) => caught,
  );
  assert.ok(err instanceof AgentError);
  assert.equal(err.status, 500);
  assert.match(err.detail, /could not be reached/);
});

test("the redactor leaves a short value alone and removes a real one", () => {
  const redact = redactor([KEY, "ab", ""]);
  assert.equal(redact(`before ${KEY} after`), "before [redacted] after");
  assert.equal(redact("a table of ab"), "a table of ab");
});

// ---------------------------------------------------------------- bad input

test("a task that is not a string with anything in it is bad-request", async () => {
  const { agent } = harness();
  for (const task of [undefined, "", "   ", 7]) {
    await assert.rejects(
      () => agent.run({ task }),
      (err) => err instanceof AgentError && err.status === 400,
      `${JSON.stringify(task)} was accepted`,
    );
  }
});

test("a task or a system text over the cap is bad-request", async () => {
  const { agent } = harness();
  await assert.rejects(
    () => agent.run({ task: "x".repeat(65537) }),
    (err) => err instanceof AgentError && err.status === 400,
  );
  await assert.rejects(
    () => agent.run({ task: "fine", system: "x".repeat(16385) }),
    (err) => err instanceof AgentError && err.status === 400,
  );
});

// ---------------------------------------------------------------- the manifest

test("the schemas the server publishes are the ones the manifest declares", () => {
  assert.equal(RUN_TOOL.name, manifestTool.name);
  assert.equal(RUN_TOOL.description, manifestTool.description);
  assert.deepEqual(RUN_TOOL.input, manifestTool.input);
  assert.deepEqual(RUN_TOOL.output, manifestTool.output);
});

test("the manifest declares the permits, the secret and the env this kit is documented with", () => {
  assert.deepEqual(manifest.provides.permits.tools, [
    "fs_list",
    "fs_read",
    "fs_write",
    "fs_history",
    "pkg_list",
    "pkg_inspect",
    "proc_list",
    "proc_logs",
    "tel_query",
    "packages",
  ]);
  assert.deepEqual(manifest.provides.permits.paths, ["/org", "/home/*"]);
  assert.equal(manifest.provides.prompt, "AGENT.md");
  const unit = manifest.deploy.units[0];
  assert.deepEqual(unit.secrets, ["ANTHROPIC_API_KEY"]);
  assert.deepEqual(unit.env, { AGENT_MODEL: "claude-opus-5", AGENT_MAX_ITERATIONS: "32", AGENT_EFFORT: "high" });
  assert.equal(unit.expose, "mcp");
  // No proc_run, no pkg_build and nothing that writes a member or an approval.
  for (const forbidden of ["proc_run", "pkg_build", "secrets_set", "users_add", "approvals_approve"]) {
    assert.ok(!manifest.provides.permits.tools.includes(forbidden), `${forbidden} is permitted`);
  }
});

test("a folder description and a tool description fit what the surface takes", () => {
  assert.ok(manifest.description.length <= 280, `${manifest.description.length} characters`);
  assert.ok(manifestTool.description.length <= 280, `${manifestTool.description.length} characters`);
});

// A JSON Schema 2020-12 subset, enough to run the manifest's output schema over
// what run answered. Unsupported keywords are ignored rather than guessed at, so
// a pass here is never a false pass on the keywords it does read.
function validate(schema, value, at = "output") {
  const fail = (why) => [`${at}: ${why}`];
  if (schema.type === "object") {
    if (value === null || typeof value !== "object" || Array.isArray(value)) return fail("is not an object");
    const problems = [];
    for (const name of schema.required ?? []) if (!Object.hasOwn(value, name)) problems.push(`${at}: has no ${name}`);
    for (const [name, held] of Object.entries(value)) {
      const property = schema.properties?.[name];
      if (!property) {
        if (schema.additionalProperties === false) problems.push(`${at}: carries ${name}, which the schema does not allow`);
        continue;
      }
      problems.push(...validate(property, held, `${at}.${name}`));
    }
    return problems;
  }
  if (schema.type === "array") {
    if (!Array.isArray(value)) return fail("is not an array");
    return value.flatMap((entry, index) => validate(schema.items ?? {}, entry, `${at}[${index}]`));
  }
  if (schema.type === "string") {
    if (typeof value !== "string") return fail("is not a string");
    if (schema.minLength !== undefined && value.length < schema.minLength) return fail("is shorter than minLength");
    if (schema.maxLength !== undefined && value.length > schema.maxLength) return fail("is longer than maxLength");
    return [];
  }
  if (schema.type === "integer") return Number.isInteger(value) ? [] : fail(`is ${JSON.stringify(value)}, not an integer`);
  if (schema.type === "boolean") return typeof value === "boolean" ? [] : fail("is not a boolean");
  return [];
}

test("the validator this test uses reads the keywords it claims to", () => {
  const schema = manifestTool.output;
  assert.deepEqual(validate(schema, { answer: "a", stopReason: "end_turn", steps: [], usage: { inputTokens: 1, outputTokens: 2 }, model: "m" }), []);
  assert.equal(validate(schema, { answer: "a", stopReason: "end_turn", steps: [], usage: { inputTokens: 1, outputTokens: 2 } }).length, 1);
  assert.equal(
    validate(schema, { answer: "a", stopReason: "end_turn", steps: [{ tool: "t", durationMs: 1.5, ok: true }], usage: { inputTokens: 1, outputTokens: 2 }, model: "m" }).length,
    1,
  );
});

test("what run answers validates against the output schema in the manifest", async () => {
  const { agent } = harness({
    plan: [
      { message: message({ stop_reason: "tool_use", content: [] }), calls: [{ name: "fs_read", input: { path: "/org/handbook/kits.md" } }] },
      { message: message({ content: [text("the handbook says so")] }) },
    ],
  });
  const answer = await agent.run({ task: "Read the handbook and say what it says." });
  assert.deepEqual(validate(manifestTool.output, answer), []);
  assert.equal(answer.steps.length, 1);
});
