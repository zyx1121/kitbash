// agent-template: the loop, and everything the loop is made of.
//
// index.js is the stdio MCP server around this file. Everything here takes the
// Anthropic client and the MCP session as factories, so a test drives the whole
// run against fakes without a key and without a surface.
//
// What a run is: open a session on the surface this Process reaches, turn every
// tool the surface answers with into a tool the model may call, and let the
// model call them until it has an answer. The model is the loop; this file is
// the plumbing around it and the accounting of what it did.

import { betaTool } from "@anthropic-ai/sdk/helpers/beta/json-schema";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { CallToolResultSchema } from "@modelcontextprotocol/sdk/types.js";

export const SELF = "agent-template";

// The default model, and the two knobs beside it. All three are the unit's env
// in kitbash.yaml, so a copy of this folder changes them there rather than here.
const DEFAULT_MODEL = "claude-opus-5";
const DEFAULT_EFFORT = "high";
const EFFORTS = new Set(["low", "medium", "high", "xhigh", "max"]);
const DEFAULT_MAX_ITERATIONS = 32;
// An iteration is one API request, so the ceiling is what one call to run may
// spend before it has to be split into two tasks.
const MAX_ITERATIONS_CEILING = 256;
const MAX_TOKENS = 16000;
// A tool result the model reads whole. Past this the rest is dropped rather
// than carried: what a surface tool answers is not this kit's to bound, and a
// fs_read of a large file would otherwise decide how much memory a run holds.
const MAX_TOOL_RESULT_BYTES = 256 * 1024;
const CLIP_MARKER =
  "\n\n[agent-template clipped this result: the tool answered more than 256 KiB and the rest was dropped. Read it in parts if you need the whole of it.]";
// What the model is told when a tool did not do its work. It is a result rather
// than a thrown error so the model can try something else, which is the point.
const TOOL_FAILED = "This tool call failed and did nothing. What it answered:";
const MAX_TASK_LENGTH = 65536;
const MAX_SYSTEM_LENGTH = 16384;
// Pages of tools/list read before the surface is taken as answered. The surface
// answers every tool in one page today; this is the backstop.
const MAX_TOOL_PAGES = 20;
// Retries the SDK makes of a request it may retry. The timeout is the client's
// own default, because one run is allowed to take minutes.
const MAX_RETRIES = 2;
// Server side fallback: when the model declines for policy reasons the API
// tries its default substitute rather than answering the caller a refusal.
const FALLBACK_BETA = "server-side-fallback-2026-07-01";
const LIST_TIMEOUT_MS = 30 * 1000;
const CALL_TIMEOUT_MS = 5 * 60 * 1000;
// What is repeated of a failure that came from somewhere else, the API or a
// tool, before it is cut.
const MAX_BORROWED_TEXT = 4 * 1024;

// The system prompt every run starts with. AGENT.md follows it and the caller's
// own system text follows that, so a member changes the standing instructions by
// editing AGENT.md in their copy of this folder and never this paragraph.
const SYSTEM_PROMPT = `You are an agent running as a Process on a kitbash machine, whose four objects are Files, Packages, Processes and Telemetry.

Your tools are the MCP surface of the member who started you, narrowed to what this Package's manifest permits. A tool you cannot see is one this Package did not ask for, not one that is broken, and there is no shell: everything you do, you do through a tool.

Files paths are absolute. A member's home is /home/<member> and the shared root is /org, which is read only for everyone. Read before you write: a write replaces the whole file and is a commit, so read the file and list the folder first.

You are answering a caller who is not watching you work and cannot answer a question halfway through. Decide, and if a task is ambiguous say in your answer which reading you took. Your final message is the answer: give the result, not a narration of your steps, which the caller already receives beside it.`;

// The tool this kit publishes. kitbash.yaml is what the surface validates
// against, so these two have to agree; the test that parses the manifest is
// what says they do.
export const RUN_TOOL = {
  name: "run",
  description:
    "Answer the task by calling the tools of this Process's surface until it is done. Returns the agent's answer, why the loop stopped, one entry per tool call and what the run cost.",
  input: {
    type: "object",
    additionalProperties: false,
    required: ["task"],
    properties: {
      task: {
        type: "string",
        minLength: 1,
        maxLength: MAX_TASK_LENGTH,
        description:
          "What to do, in natural language. Name the Files paths and the Packages involved: the agent reaches only what this kit's permits allow, and it cannot ask you a question halfway through.",
      },
      system: {
        type: "string",
        maxLength: MAX_SYSTEM_LENGTH,
        description:
          "Appended to the system prompt for this run. Standing instructions belong in AGENT.md of your own copy of this folder; this is for what changes call by call.",
      },
    },
  },
  output: {
    type: "object",
    additionalProperties: false,
    required: ["answer", "stopReason", "steps", "usage", "model"],
    properties: {
      answer: {
        type: "string",
        description:
          "The text of the agent's final message, which is its answer to the task. Empty when the model refused.",
      },
      stopReason: {
        type: "string",
        description:
          "Why the loop ended: end_turn when the agent answered, max_iterations when it ran out of turns, max_tokens when the final message was cut short, refusal when the model declined, or whatever else the API reported.",
      },
      steps: {
        type: "array",
        description: "One entry per tool call the agent made, in the order they ran.",
        items: {
          type: "object",
          additionalProperties: false,
          required: ["tool", "durationMs", "ok"],
          properties: {
            tool: { type: "string", description: "The surface tool that was called, by the name the surface publishes." },
            durationMs: { type: "integer", description: "How long the call took." },
            ok: {
              type: "boolean",
              description:
                "False when the tool answered a problem or did not answer at all. The agent was told so and kept going, so a false here is not a failed run.",
            },
          },
        },
      },
      usage: {
        type: "object",
        additionalProperties: false,
        required: ["inputTokens", "outputTokens"],
        properties: {
          inputTokens: { type: "integer", description: "Input tokens across every turn, cache reads and cache writes included." },
          outputTokens: { type: "integer", description: "Output tokens across every turn." },
        },
      },
      model: { type: "string", description: "The model that ran, which is AGENT_MODEL or this kit's default." },
    },
  },
};

// ---------------------------------------------------------------- errors

export class AgentError extends Error {
  constructor({ type, status, title, detail, fix }) {
    super(detail);
    this.type = type;
    this.status = status;
    this.title = title;
    this.detail = detail;
    this.fix = fix;
  }

  toResult() {
    const problem = { type: this.type, title: this.title, status: this.status, detail: this.detail, fix: this.fix };
    return { isError: true, content: [{ type: "text", text: JSON.stringify(problem) }] };
  }

  // The same problem with every secret this Process holds removed from it. A
  // problem is built from text this kit did not write, an API message or a
  // transport failure, so it is passed through the redactor before it leaves.
  redacted(redact) {
    return new AgentError({
      type: this.type,
      status: this.status,
      title: redact(this.title),
      detail: redact(this.detail),
      fix: redact(this.fix),
    });
  }
}

const errorType = (slug) => `https://kitbash.zyx.tw/errors/${slug}`;

const badRequest = (detail, fix, title = "The task cannot be run") =>
  new AgentError({ type: errorType("bad-request"), status: 400, title, detail, fix });

const notPermitted = (detail, fix, title = "The agent may not run") =>
  new AgentError({ type: errorType("not-permitted"), status: 403, title, detail, fix });

const internal = (detail, fix, title = "The run failed") =>
  new AgentError({ type: errorType("internal"), status: 500, title, detail, fix });

// The one fix that answers a missing or refused key, written once because it is
// the answer to three different failures.
const SECRET_FIX = "Call secrets_set ANTHROPIC_API_KEY, then proc_stop and proc_run this Package.";

// ---------------------------------------------------------------- redaction

// Nothing this kit writes, into a problem or into its log, may carry the key or
// this Process's token. The values are known here, so they are removed by value
// rather than by trusting every place a string is built. Short values are left
// alone: a two character secret would blank half of any text it appeared in.
const MIN_REDACTED = 8;

export function redactor(secrets) {
  const held = secrets.filter((secret) => typeof secret === "string" && secret.length >= MIN_REDACTED);
  return (text) => {
    let out = typeof text === "string" ? text : `${text}`;
    for (const secret of held) out = out.split(secret).join("[redacted]");
    return out;
  };
}

// What is said of something thrown: its message, on one line, cut, with nothing
// of a stack and nothing of a request body.
const oneLine = (err) =>
  `${err?.message ?? err}`.replace(/\s+/g, " ").trim().slice(0, MAX_BORROWED_TEXT);

// ---------------------------------------------------------------- the environment

// The environment a Process is given, read into what a run needs. Nothing here
// throws and nothing here is logged: a value that is wrong is replaced by the
// default and said so by name, never by content.
export function readConfig(env) {
  const model = (env.AGENT_MODEL ?? "").trim() || DEFAULT_MODEL;

  const askedEffort = (env.AGENT_EFFORT ?? "").trim();
  const effort = EFFORTS.has(askedEffort) ? askedEffort : DEFAULT_EFFORT;

  const askedIterations = Number.parseInt((env.AGENT_MAX_ITERATIONS ?? "").trim(), 10);
  const maxIterations =
    Number.isInteger(askedIterations) && askedIterations >= 1 && askedIterations <= MAX_ITERATIONS_CEILING
      ? askedIterations
      : DEFAULT_MAX_ITERATIONS;

  return {
    apiKey: (env.ANTHROPIC_API_KEY ?? "").trim(),
    endpoint: (env.KITBASH_MCP_ENDPOINT ?? "").trim(),
    token: (env.KITBASH_TELEMETRY_TOKEN ?? "").trim(),
    model,
    effort,
    maxIterations,
    // What was ignored, for one log line at the start of a run. Names only.
    ignored: [
      askedEffort !== "" && effort !== askedEffort ? "AGENT_EFFORT" : undefined,
      (env.AGENT_MAX_ITERATIONS ?? "").trim() !== "" && `${maxIterations}` !== (env.AGENT_MAX_ITERATIONS ?? "").trim()
        ? "AGENT_MAX_ITERATIONS"
        : undefined,
    ].filter(Boolean),
  };
}

// ---------------------------------------------------------------- the surface

// The session this Process opens back onto its owner's surface, PLAN.md 2.3.
// One per run, closed when the run ends.
export async function openSurface({ endpoint, token, version }) {
  let url;
  try {
    url = new URL(endpoint);
  } catch {
    url = undefined;
  }
  // A value with no scheme parses as a URL whose protocol is the host, so the
  // scheme is checked rather than assumed.
  if (!url || (url.protocol !== "http:" && url.protocol !== "https:")) {
    throw internal(
      `KITBASH_MCP_ENDPOINT is ${JSON.stringify(endpoint.slice(0, 200))}, which is not an http URL.`,
      "Run this Package through kitbash: the endpoint comes from kitbashd as http://host.containers.internal:4318/mcp.",
    );
  }

  const client = new Client({ name: SELF, version }, { capabilities: {} });
  const transport = new StreamableHTTPClientTransport(url, {
    requestInit: { headers: { authorization: `Bearer ${token}` } },
  });
  try {
    await client.connect(transport);
  } catch (err) {
    await transport.close().catch(() => {});
    throw internal(
      `The MCP surface this Process reaches did not open a session: ${oneLine(err)}`,
      "Wait for this Process's other sessions to end, then run the task again. If it persists, check that KITBASH_MCP_ENDPOINT and KITBASH_TELEMETRY_TOKEN are the ones kitbashd gave it.",
    );
  }

  return {
    client,
    close: async () => {
      await transport.terminateSession().catch(() => {});
      await client.close().catch(() => {});
    },
  };
}

// What the surface would publish this Package's own tool under. The surface
// publishes every tool of every running Process of the owner, this Process
// included, so a Package that permits packages is handed its own run tool by
// the surface it just asked. The names are read from what this Process was
// given rather than written here, so a copy of this folder under another name
// still finds itself: package.json's name, which a copy renames beside the
// manifest, and the last component of KITBASH_PACKAGE, which is the folder
// kitbashd started.
export function selfToolNames({ self, packagePath }) {
  const names = new Set();
  const candidates = [self, `${packagePath ?? ""}`.split("/").filter(Boolean).at(-1)];
  for (const candidate of candidates) {
    const name = typeof candidate === "string" ? candidate.trim() : "";
    if (name !== "") names.add(`${name}_${RUN_TOOL.name}`);
  }
  return names;
}

// Every tool the surface publishes, in the order it published them, less this
// Package's own. An agent given its own run tool calls itself: one turn of the
// loop starts a whole new loop of up to AGENT_MAX_ITERATIONS turns, inside the
// turn that asked for it, with no bound on the depth and a five minute timeout
// per call that has to cover all of it. Delegating is a Process a member
// started, not this one.
async function listSurfaceTools(client, { exclude = new Set(), say = () => {} } = {}) {
  const tools = [];
  let cursor;
  for (let page = 0; page < MAX_TOOL_PAGES; page += 1) {
    let result;
    try {
      result = await client.listTools(cursor ? { cursor } : {}, { timeout: LIST_TIMEOUT_MS });
    } catch (err) {
      throw internal(
        `The MCP surface did not answer tools/list: ${oneLine(err)}`,
        "Retry; if it persists, check this Process's session limit on the receiver.",
      );
    }
    for (const tool of result?.tools ?? []) {
      if (typeof tool?.name !== "string") continue;
      if (exclude.has(tool.name)) {
        say(`[${SELF}] not giving the model ${tool.name}, which is this Process's own tool`);
        continue;
      }
      tools.push(tool);
    }
    cursor = typeof result?.nextCursor === "string" ? result.nextCursor : undefined;
    if (!cursor) break;
  }
  return tools;
}

// What the model reads of a tool result: the text blocks, or the structured
// content when a tool answered only that. readable is false when the tool
// answered something this kit cannot turn into text, which the model is told
// was a failure rather than an empty answer.
export function resultText(result) {
  const blocks = result?.content ?? [];
  const text = blocks
    .filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("\n");
  if (text !== "") return { text, readable: true };
  if (result?.structuredContent !== undefined) {
    try {
      return { text: JSON.stringify(result.structuredContent), readable: true };
    } catch {
      return { text: "This tool answered structured content this kit could not read as JSON.", readable: false };
    }
  }
  // An image or an embedded resource is a result this kit drops, so the model
  // is told the call did not give it anything rather than left to read an
  // empty answer as a done piece of work.
  if (blocks.length > 0) {
    return { text: "This tool answered content this kit cannot read (image or resource).", readable: false };
  }
  return { text: "The tool answered nothing.", readable: true };
}

// A result the model reads whole, or its first 256 KiB and a marker saying so.
export function clip(text) {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= MAX_TOOL_RESULT_BYTES) return text;
  return bytes.subarray(0, MAX_TOOL_RESULT_BYTES).toString("utf8") + CLIP_MARKER;
}

// Every tool of the surface as a tool the runner can call. Names pass through
// unchanged: the surface already namespaces them as <package>_<tool>. A
// listing's inputSchema is an object schema and needs no check here, because
// the MCP client validates tools/list against ToolSchema before this sees it
// and a listing whose schema is anything else fails the listing itself. A
// description is optional there, so that one is defaulted.
//
// redact is not optional in spirit: everything a tool answers is a stranger's
// text on its way into the model's context, and this Process's own token is
// what a receiver echoes back in a 401 or a 403 body. A model given the token
// is a model that can write it into a file with fs_write.
export function surfaceTools({ listed, callTool, steps, redact = (text) => text }) {
  return listed.map((listing) =>
    betaTool({
      name: listing.name,
      description:
        typeof listing.description === "string" && listing.description !== ""
          ? listing.description
          : `The ${listing.name} tool of this machine's surface. It carries no description.`,
      inputSchema: listing.inputSchema,
      run: async (input) => {
        const startedAt = Date.now();
        let ok = true;
        let text;
        try {
          const result = await callTool(listing.name, input ?? {});
          const read = resultText(result);
          ok = result?.isError !== true && read.readable;
          text = result?.isError === true ? `${TOOL_FAILED} ${read.text}` : read.text;
        } catch (err) {
          // A tool that threw is a tool that failed, and the model is told so
          // in the result rather than by the run ending.
          ok = false;
          text = `${TOOL_FAILED} ${oneLine(err)}`;
        }
        steps.push({ tool: listing.name, durationMs: Date.now() - startedAt, ok });
        return clip(redact(text));
      },
    }),
  );
}

// ---------------------------------------------------------------- the model

// The Anthropic client, made per run so a key rotated between runs is read at
// the start of the next one. The timeout is the SDK's own, because a hard task
// legitimately takes minutes.
function anthropicClient({ apiKey, Anthropic }) {
  return new Anthropic({ apiKey, maxRetries: MAX_RETRIES });
}

// What the API answered, as a problem the caller can act on. The key is never
// in any of these: a 400's message is the API's own reading of the request, and
// the redactor over it is the belt to that brace.
export function problemFromApi(err) {
  const status = Number.isInteger(err?.status) ? err.status : undefined;
  const said = oneLine(err?.error?.error?.message ?? err?.message ?? err);
  if (status === 401 || status === 403) {
    return notPermitted(`The Anthropic API refused this Process's key with ${status}.`, SECRET_FIX, "The model API refused the key");
  }
  if (status === 429 || (status !== undefined && status >= 500 && status < 600)) {
    return internal(`The Anthropic API answered ${status} and the run did not finish.`, "Try again; this one is the API's side and not the task.");
  }
  if (status === 400) {
    return badRequest(`The Anthropic API refused the request: ${said}`, "Shorten the task, or split it into two runs, and try again.", "The model API refused the request");
  }
  return internal(`The Anthropic API could not be reached: ${said}`, "Try again; if it persists, check that this machine reaches api.anthropic.com.");
}

// The system prompt of one run: this kit's paragraph, then AGENT.md, then the
// caller's own text. The last block is cached, so the whole of it is a prefix
// the next run of the same Process reads from the cache.
export function systemBlocks({ prompt, extra }) {
  const blocks = [{ type: "text", text: SYSTEM_PROMPT }];
  if (typeof prompt === "string" && prompt.trim() !== "") blocks.push({ type: "text", text: prompt });
  if (typeof extra === "string" && extra.trim() !== "") blocks.push({ type: "text", text: extra });
  blocks[blocks.length - 1].cache_control = { type: "ephemeral" };
  return blocks;
}

// Input tokens are what the turn read, whether it read it from the cache or
// wrote it there, so all three counts are one number.
function addUsage(total, usage) {
  if (!usage) return;
  total.inputTokens +=
    (usage.input_tokens ?? 0) + (usage.cache_read_input_tokens ?? 0) + (usage.cache_creation_input_tokens ?? 0);
  total.outputTokens += usage.output_tokens ?? 0;
}

const answerOf = (message) =>
  (message?.content ?? [])
    .filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("");

// ---------------------------------------------------------------- the run

async function runTask(args, { env, prompt, version, self, makeAnthropic, makeSurface, log }) {
  const task = args?.task;
  if (typeof task !== "string" || task.trim() === "") {
    throw badRequest("task is not a string with anything in it.", "Call run with a task that says what to do, in words.");
  }
  if (task.length > MAX_TASK_LENGTH) {
    throw badRequest(
      `task is ${task.length} characters, over the ${MAX_TASK_LENGTH} this tool takes.`,
      "Write the long part into a file with fs_write and give the agent its path instead.",
    );
  }
  const extra = args?.system;
  if (extra !== undefined && (typeof extra !== "string" || extra.length > MAX_SYSTEM_LENGTH)) {
    throw badRequest(
      `system is not a string of at most ${MAX_SYSTEM_LENGTH} characters.`,
      "Put standing instructions in AGENT.md of your own copy of this folder and leave system for what changes call by call.",
    );
  }

  const config = readConfig(env);
  const redact = redactor([config.apiKey, config.token]);
  const say = (line) => log(redact(line));

  // A Process with no key is a Process that was run before its secret was set.
  // It is this answer rather than a container that refused to start, so the
  // member reads the fix from the tool they called.
  if (config.apiKey === "") {
    throw notPermitted(
      "This Process was given no ANTHROPIC_API_KEY, so it cannot reach the model.",
      SECRET_FIX,
      "The agent has no key",
    );
  }
  if (config.endpoint === "" || config.token === "") {
    throw internal(
      "This Process was given no MCP endpoint or no token, so the agent would have no tools.",
      "Run this Package through kitbash: KITBASH_MCP_ENDPOINT and KITBASH_TELEMETRY_TOKEN come from kitbashd.",
    );
  }
  if (config.ignored.length > 0) say(`[${SELF}] ignoring ${config.ignored.join(" and ")}, which this Process was given a value this kit cannot use`);

  const startedAt = Date.now();
  const steps = [];
  const usage = { inputTokens: 0, outputTokens: 0 };
  const session = await makeSurface({ endpoint: config.endpoint, token: config.token, version }).catch((err) => {
    throw err instanceof AgentError ? err.redacted(redact) : err;
  });

  try {
    const listed = await listSurfaceTools(session.client, {
      exclude: selfToolNames({ self, packagePath: env.KITBASH_PACKAGE }),
      say,
    });
    if (listed.length === 0) {
      throw internal(
        "The surface this Process reaches published no tools, so the agent has nothing to work with.",
        "Declare in provides.permits.tools of this kit's kitbash.yaml what the agent may call: a Process reaches only the tools its Package permits.",
      );
    }
    const tools = surfaceTools({
      listed,
      steps,
      redact,
      callTool: (name, input) =>
        session.client.callTool({ name, arguments: input }, CallToolResultSchema, { timeout: CALL_TIMEOUT_MS }),
    });

    const anthropic = makeAnthropic({ apiKey: config.apiKey });
    const runner = anthropic.beta.messages.toolRunner({
      model: config.model,
      max_tokens: MAX_TOKENS,
      thinking: { type: "adaptive" },
      output_config: { effort: config.effort },
      // A refusal for policy reasons is answered by a substitute model rather
      // than handed to the caller, and the caller still reads which model ran.
      betas: [FALLBACK_BETA],
      fallbacks: "default",
      system: systemBlocks({ prompt, extra }),
      tools,
      messages: [{ role: "user", content: task }],
      max_iterations: config.maxIterations,
    });

    let iterations = 0;
    let last;
    try {
      for await (const message of runner) {
        iterations += 1;
        last = message;
        addUsage(usage, message?.usage);
        // A paused turn is resumed by the runner itself: it pushes the message
        // back and asks again, see determineNextStepFromStopReason in the
        // installed SDK. Pushing it here as well would send the turn twice.
        if (message?.stop_reason === "pause_turn") say(`[${SELF}] turn ${iterations} paused and the runner is resuming it`);
      }
    } catch (err) {
      if (err instanceof AgentError) throw err;
      throw problemFromApi(err);
    }

    // done() is the runner's own final message. The fallback is the last one
    // seen, for a runner that does not expose it.
    const final = typeof runner.done === "function" ? await runner.done() : last;
    if (!final) {
      throw internal(
        "The model answered nothing at all, so there is no result to report.",
        "Try the task again; if it persists, read this Process's stderr with proc_logs.",
      );
    }

    const stop = final.stop_reason ?? null;
    let stopReason;
    let answer = answerOf(final);
    if (stop === "refusal") {
      // A refusal has no text to carry, and the category is what says why.
      stopReason = "refusal";
      answer = "";
      const category = final.stop_details?.category;
      say(`[${SELF}] the model declined the task${category ? ` for ${category}` : ""}`);
    } else if (stop === "max_tokens") {
      // What the last message says of itself comes first: a turn cut by
      // max_tokens on the last allowed iteration was cut by max_tokens, and
      // reading the cap first would report the wrong one of the two.
      stopReason = "max_tokens";
      say(`[${SELF}] the final message hit max_tokens at ${MAX_TOKENS} and is cut`);
    } else if (iterations >= config.maxIterations && stop !== "end_turn" && stop !== "stop_sequence") {
      // The runner stops at the cap whatever the model was in the middle of, so
      // the answer is whatever it had said by then and the caller is told that
      // it was cut rather than finished.
      stopReason = "max_iterations";
      say(`[${SELF}] the loop hit AGENT_MAX_ITERATIONS at ${config.maxIterations} iterations`);
    } else {
      stopReason = stop ?? "end_turn";
    }

    say(
      `[${SELF}] ${stopReason} after ${iterations} iterations and ${steps.length} tool calls in ${Date.now() - startedAt} ms, ${usage.inputTokens} in and ${usage.outputTokens} out`,
    );
    return { answer, stopReason, steps, usage, model: config.model };
  } catch (err) {
    // Every problem that leaves this run passes the redactor, because most of
    // them repeat text this kit did not write.
    throw err instanceof AgentError ? err.redacted(redact) : err;
  } finally {
    await session.close();
  }
}

// ---------------------------------------------------------------- the factory

// The agent, with the two things a test replaces: the Anthropic client and the
// MCP session. The defaults are the real ones, so index.js passes neither.
export function createAgent({
  env = process.env,
  prompt = "",
  version = "0.0.0",
  self = SELF,
  Anthropic,
  makeAnthropic = ({ apiKey }) => anthropicClient({ apiKey, Anthropic }),
  makeSurface = openSurface,
  log = (line) => console.error(line),
} = {}) {
  return {
    run: (args) => runTask(args, { env, prompt, version, self, makeAnthropic, makeSurface, log }),
    // The redactor over this Process's secrets, for the caller that logs
    // something this file did not build. It reads the environment on every call
    // so a rotated value is the one that is removed.
    redact: (text) => {
      const config = readConfig(env);
      return redactor([config.apiKey, config.token])(text);
    },
  };
}
