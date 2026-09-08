// workflow: the kitbash workflow engine kit.
//
// One tool, run. Given the path of a graph file in Files it reads the graph
// through the MCP endpoint every Process reaches, then walks the graph: each
// step calls one tool of the owner's surface, or runs another graph, with an
// input templated from the graph's input and from the outputs of earlier
// steps. The result is the graph's output and one entry per step.
//
// The core knows nothing about any of this, PLAN.md 3. The graph format, the
// templating and the way one graph names another are this kit's contract,
// published in kitbash.yaml and in WORKFLOW.md, nowhere else.
//
// The surface it calls is its owner's, reached over streamable HTTP at
// KITBASH_MCP_ENDPOINT with KITBASH_TELEMETRY_TOKEN as the bearer, PLAN.md 2.3.
// One session is opened per run and closed when the run ends; a nested graph
// runs in the same session. Every call the engine makes is already traced by
// kitbashd with kitbash.caller naming this Process, so the only span this kit
// writes itself is one per run.
//
// The tool schemas this server advertises are read from kitbash.yaml next to
// this file, so the manifest kitbashd validates against and the schemas the
// server publishes cannot drift apart.
//
// stdout carries MCP messages only. Everything else goes to stderr.

import { randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { CallToolRequestSchema, CallToolResultSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import { parse as parseYaml } from "yaml";

const SELF = "workflow";
// A graph is a document an agent writes by hand. Anything larger is not a
// graph, and reading it whole is what keeps the engine's memory bounded.
const MAX_GRAPH_BYTES = 256 * 1024;
// The graph run counts as one, so a graph may run graphs seven deep below it.
const MAX_DEPTH = 8;
const RUN_TIMEOUT_MS = 5 * 60 * 1000;
const CALL_TIMEOUT_MS = 60 * 1000;
const EXPORT_TIMEOUT_MS = 4 * 1000;
const MAX_TOOL_PAGES = 20;
const STEP_ID = /^[a-z][a-z0-9_]*$/;
// MCP tool names, the same pattern spec/manifest.schema.json carries.
const TOOL_NAME = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;
// A string that is exactly one reference keeps the referenced value's type.
const WHOLE_REFERENCE = /^\$\{([^{}]+)\}$/;
// A reference inside a longer string is stringified into it.
const EMBEDDED_REFERENCE = /\$\{([^{}]+)\}/g;
const GRAPH_KEYS = new Set(["name", "description", "input", "steps", "output"]);
const STEP_KEYS = new Set(["id", "tool", "workflow", "input", "output"]);

const here = path.dirname(fileURLToPath(import.meta.url));

const manifest = parseYaml(await readFile(path.join(here, "kitbash.yaml"), "utf8"));
const selfVersion = JSON.parse(await readFile(path.join(here, "package.json"), "utf8")).version;
const runTool = manifest.provides.tools.find((tool) => tool.name === "run");
if (!runTool) {
  console.error("kitbash.yaml declares no tool named run");
  process.exit(1);
}

// ---------------------------------------------------------------- errors

class WorkflowError extends Error {
  constructor({ type, status, title, detail, fix, instance }) {
    super(detail);
    this.type = type;
    this.status = status;
    this.title = title;
    this.detail = detail;
    this.fix = fix;
    this.instance = instance;
  }

  toResult() {
    const problem = { type: this.type, title: this.title, status: this.status, detail: this.detail };
    if (this.instance) problem.instance = this.instance;
    problem.fix = this.fix;
    return { isError: true, content: [{ type: "text", text: JSON.stringify(problem) }] };
  }
}

const errorType = (slug) => `https://kitbash.zyx.tw/errors/${slug}`;

const badRequest = (detail, fix, instance, title = "Graph cannot be run") =>
  new WorkflowError({ type: errorType("bad-request"), status: 400, title, detail, fix, instance });

const notFound = (detail, fix, instance, title = "Not found") =>
  new WorkflowError({ type: errorType("not-found"), status: 404, title, detail, fix, instance });

const tooLarge = (detail, fix, instance, title = "Graph is too large") =>
  new WorkflowError({ type: errorType("too-large"), status: 413, title, detail, fix, instance });

const internal = (detail, fix, instance, title = "Run failed") =>
  new WorkflowError({ type: errorType("internal"), status: 500, title, detail, fix, instance });

// A tool that fails answers its own RFC 9457 problem. The run carries that
// problem's class outward, so an agent sees why the step failed and not that
// something inside this kit went wrong. Anything that does not parse as a
// problem is this kit's problem, and that is internal.
function problemFrom(result, { detail, fix, instance }) {
  const first = Array.isArray(result?.content) ? result.content.find((block) => block?.type === "text") : undefined;
  let parsed;
  if (typeof first?.text === "string") {
    try {
      parsed = JSON.parse(first.text);
    } catch {
      parsed = undefined;
    }
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed) || typeof parsed.type !== "string") {
    const text = typeof first?.text === "string" ? first.text.replace(/\s+/g, " ").trim().slice(0, 280) : "";
    return internal(text === "" ? detail : `${detail} ${text}`, fix, instance);
  }
  return new WorkflowError({
    type: parsed.type,
    status: Number.isInteger(parsed.status) ? parsed.status : 500,
    title: typeof parsed.title === "string" ? parsed.title : "Step failed",
    detail: `${detail} ${typeof parsed.detail === "string" ? parsed.detail : ""}`.trim(),
    fix: typeof parsed.fix === "string" ? parsed.fix : fix,
    instance,
  });
}

// ---------------------------------------------------------------- values

const isPlainObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);

function stringify(value) {
  if (typeof value === "string") return value;
  if (value === undefined) return "";
  return JSON.stringify(value);
}

// Dotted, and a segment that is all digits indexes an array.
function splitReference(reference, where) {
  const segments = reference.trim().split(".");
  if (segments.length === 0 || segments.some((segment) => segment.trim() === "")) {
    throw badRequest(
      `${where} carries the reference \${${reference}}, which is not a dotted path.`,
      "Write a reference such as ${input.name} or ${steps.first.output.text}.",
    );
  }
  return segments.map((segment) => segment.trim());
}

function referencesOf(value, where, out = []) {
  if (typeof value === "string") {
    for (const match of value.matchAll(EMBEDDED_REFERENCE)) out.push({ segments: splitReference(match[1], where), where });
    return out;
  }
  if (Array.isArray(value)) {
    for (const item of value) referencesOf(item, where, out);
    return out;
  }
  if (isPlainObject(value)) {
    for (const item of Object.values(value)) referencesOf(item, where, out);
    return out;
  }
  return out;
}

function lookup(segments, scope, where) {
  let current = scope;
  const walked = [];
  for (const segment of segments) {
    walked.push(segment);
    if (Array.isArray(current) && /^[0-9]+$/.test(segment)) {
      current = current[Number(segment)];
    } else if (isPlainObject(current)) {
      current = current[segment];
    } else {
      current = undefined;
    }
    if (current === undefined) {
      throw badRequest(
        `${where} references \${${segments.join(".")}}, and ${walked.join(".")} is not there.`,
        "Reference a field the graph input carries or an earlier step actually returned.",
      );
    }
  }
  return current;
}

function resolve(template, scope, where) {
  if (typeof template === "string") {
    const whole = template.match(WHOLE_REFERENCE);
    if (whole) return lookup(splitReference(whole[1], where), scope, where);
    return template.replace(EMBEDDED_REFERENCE, (_, reference) => stringify(lookup(splitReference(reference, where), scope, where)));
  }
  if (Array.isArray(template)) return template.map((item) => resolve(item, scope, where));
  if (isPlainObject(template)) {
    const out = {};
    for (const [key, value] of Object.entries(template)) out[key] = resolve(value, scope, where);
    return out;
  }
  return template;
}

// ---------------------------------------------------------------- the graph

function validateGraph(doc, graphPath) {
  const where = graphPath;
  if (!isPlainObject(doc)) {
    throw badRequest(`${where} does not hold a graph object.`, "Write a YAML or JSON mapping with a steps list. Read WORKFLOW.md.", where);
  }
  for (const key of Object.keys(doc)) {
    if (!GRAPH_KEYS.has(key)) {
      throw badRequest(`${where} carries the unknown key ${key}.`, `A graph has ${[...GRAPH_KEYS].join(", ")} and nothing else.`, where);
    }
  }
  for (const key of ["name", "description"]) {
    if (doc[key] !== undefined && typeof doc[key] !== "string") {
      throw badRequest(`${where} carries a ${key} that is not a string.`, `Write ${key} as one line of text, or leave it out.`, where);
    }
  }
  if (doc.input !== undefined && !isPlainObject(doc.input)) {
    throw badRequest(`${where} carries an input that is not a JSON schema object.`, "Write input as a JSON Schema object, or leave it out.", where);
  }
  if (!Array.isArray(doc.steps) || doc.steps.length === 0) {
    throw badRequest(`${where} declares no steps.`, "A graph is a steps list with at least one step. Read WORKFLOW.md.", where);
  }

  const ids = new Set();
  for (const [index, step] of doc.steps.entries()) {
    const at = `Step ${index + 1} of ${where}`;
    if (!isPlainObject(step)) throw badRequest(`${at} is not a step object.`, "Write each step as a mapping with an id and a tool or a workflow.", where);
    for (const key of Object.keys(step)) {
      if (!STEP_KEYS.has(key)) throw badRequest(`${at} carries the unknown key ${key}.`, `A step has ${[...STEP_KEYS].join(", ")} and nothing else.`, where);
    }
    if (typeof step.id !== "string" || !STEP_ID.test(step.id)) {
      throw badRequest(`${at} has the id ${JSON.stringify(step.id)}.`, `A step id matches ${STEP_ID.source}.`, where);
    }
    if (ids.has(step.id)) throw badRequest(`${where} has two steps with the id ${step.id}.`, "Give every step in one graph its own id.", where);

    const named = `Step ${JSON.stringify(step.id)} of ${where}`;
    const hasTool = step.tool !== undefined;
    const hasWorkflow = step.workflow !== undefined;
    if (hasTool === hasWorkflow) {
      throw badRequest(`${named} names ${hasTool ? "both a tool and a workflow" : "neither a tool nor a workflow"}.`, "Give every step exactly one of tool and workflow.", where);
    }
    if (hasTool && (typeof step.tool !== "string" || !TOOL_NAME.test(step.tool))) {
      throw badRequest(`${named} names the tool ${JSON.stringify(step.tool)}.`, "Name a tool on your surface, such as echo_echo.", where);
    }
    if (hasWorkflow && (typeof step.workflow !== "string" || !step.workflow.startsWith("/"))) {
      throw badRequest(`${named} names the workflow ${JSON.stringify(step.workflow)}.`, "Name another graph by its absolute Files path.", where);
    }
    if (step.input !== undefined && !isPlainObject(step.input)) {
      throw badRequest(`${named} carries an input that is not a mapping.`, "Write the step input as a mapping of names to values or references.", where);
    }
    if (step.output !== undefined && !isPlainObject(step.output)) {
      throw badRequest(`${named} carries an output that is not a mapping.`, "Write the step output as a mapping of names to ${result...} references, or leave it out.", where);
    }

    for (const reference of referencesOf(step.input, named)) {
      const [root, id, block] = reference.segments;
      if (root === "input") continue;
      if (root !== "steps") {
        throw badRequest(`${named} references \${${reference.segments.join(".")}}.`, "A step input references ${input...} or ${steps.<id>.output...}.", where);
      }
      if (!ids.has(id)) {
        throw badRequest(
          `${named} references the step ${JSON.stringify(id ?? "")}, which does not run before it.`,
          "Reference a step listed earlier in this graph. Steps run in the order they are written.",
          where,
        );
      }
      if (block !== "output") {
        throw badRequest(`${named} references \${${reference.segments.join(".")}}.`, "A step is referenced as ${steps.<id>.output...}, its output and nothing else.", where);
      }
    }
    for (const reference of referencesOf(step.output, `The output of ${named}`)) {
      if (reference.segments[0] !== "result") {
        throw badRequest(
          `The output of ${named} references \${${reference.segments.join(".")}}.`,
          "A step output maps ${result...} references into the tool's raw result.",
          where,
        );
      }
    }

    ids.add(step.id);
  }

  if (doc.output !== undefined) {
    if (!isPlainObject(doc.output)) throw badRequest(`${where} carries an output that is not a mapping.`, "Write the graph output as a mapping of names to references, or leave it out.", where);
    for (const reference of referencesOf(doc.output, `The output of ${where}`)) {
      const [root, id, block] = reference.segments;
      if (root === "input") continue;
      if (root !== "steps" || !ids.has(id) || block !== "output") {
        throw badRequest(
          `The output of ${where} references \${${reference.segments.join(".")}}.`,
          "A graph output references ${input...} or ${steps.<id>.output...} of a step this graph declares.",
          where,
        );
      }
    }
  }

  return doc;
}

// The graph's input block documents the graph for whoever calls it. This engine
// does not validate against it; it only checks that every name the schema lists
// as required is present, which is the mistake a caller actually makes.
function checkRequired(doc, input, graphPath) {
  const required = doc.input?.required;
  if (!Array.isArray(required)) return;
  const missing = required.filter((name) => typeof name === "string" && !(name in input));
  if (missing.length > 0) {
    throw badRequest(
      `${graphPath} requires the input ${missing.join(", ")}.`,
      `Call run with an input carrying ${missing.join(", ")}.`,
      graphPath,
    );
  }
}

// ---------------------------------------------------------------- the surface

async function openSession(endpoint, token) {
  const client = new Client({ name: SELF, version: selfVersion }, { capabilities: {} });
  const transport = new StreamableHTTPClientTransport(new URL(endpoint), {
    requestInit: { headers: { authorization: `Bearer ${token}` } },
  });
  try {
    await client.connect(transport);
  } catch (err) {
    console.error(`[workflow] cannot open an MCP session at ${endpoint}: ${err?.stack ?? err}`);
    throw internal(
      "The MCP surface this Process reaches did not open a session.",
      "Check that this Process is running and that KITBASH_MCP_ENDPOINT and KITBASH_TELEMETRY_TOKEN are the ones kitbashd gave it.",
    );
  }
  return { client, transport };
}

async function closeSession(session) {
  try {
    await session.transport.terminateSession();
  } catch (err) {
    console.error(`[workflow] the session could not be terminated: ${err?.message ?? err}`);
  }
  try {
    await session.client.close();
  } catch (err) {
    console.error(`[workflow] the session could not be closed: ${err?.message ?? err}`);
  }
}

async function listToolNames(client, deadline) {
  const names = new Set();
  let cursor;
  for (let page = 0; page < MAX_TOOL_PAGES; page += 1) {
    let result;
    try {
      result = await client.listTools(cursor ? { cursor } : {}, { timeout: budget(deadline, "listing the tools of the surface") });
    } catch (err) {
      console.error(`[workflow] tools/list failed: ${err?.stack ?? err}`);
      throw internal("The MCP surface did not answer tools/list.", "Retry; if it persists, check this Process's session limit on the receiver.");
    }
    for (const tool of result.tools ?? []) if (typeof tool?.name === "string") names.add(tool.name);
    cursor = typeof result.nextCursor === "string" ? result.nextCursor : undefined;
    if (!cursor) break;
    if (page === MAX_TOOL_PAGES - 1) console.error(`[workflow] the surface still had tool pages after ${MAX_TOOL_PAGES}; a step naming a tool beyond them is not found`);
  }
  return names;
}

function budget(deadline, what) {
  const remaining = deadline - Date.now();
  if (remaining <= 0) {
    throw internal(
      `The run passed its ${RUN_TIMEOUT_MS / 1000} second budget while ${what}.`,
      "Split the graph, or make the steps that take the time faster.",
    );
  }
  return Math.min(CALL_TIMEOUT_MS, remaining);
}

async function callTool(client, name, args, deadline, { detail, fix, instance }) {
  const timeout = budget(deadline, `calling ${name}`);
  let result;
  try {
    result = await client.callTool({ name, arguments: args }, CallToolResultSchema, { timeout });
  } catch (err) {
    console.error(`[workflow] ${name} failed: ${err?.stack ?? err}`);
    if (err?.code === -32001) {
      throw internal(`${detail} it did not answer within ${Math.round(timeout / 1000)} seconds.`, "Call a faster tool, or split the graph.", instance);
    }
    throw internal(`${detail} the call did not complete: ${(err?.message ?? String(err)).slice(0, 200)}`, fix, instance);
  }
  if (result?.isError) throw problemFrom(result, { detail, fix, instance });
  return result;
}

// What a step returns when it declares no output mapping: the tool's structured
// content, or its text when structured content is what the tool does not have.
function defaultOutput(result) {
  if (isPlainObject(result?.structuredContent)) return result.structuredContent;
  const text = (Array.isArray(result?.content) ? result.content : [])
    .filter((block) => block?.type === "text" && typeof block.text === "string")
    .map((block) => block.text)
    .join("\n");
  return { text };
}

// ---------------------------------------------------------------- the run

async function readGraph(client, graphPath, deadline, at) {
  const result = await callTool(client, "fs_read", { path: graphPath }, deadline, {
    detail: `${at} could not be read:`,
    fix: "Write the graph into Files first, and check the path.",
    instance: graphPath,
  });

  const blocks = Array.isArray(result.content) ? result.content : [];
  const first = blocks[0];
  if (first?.type !== "text" || typeof first.text !== "string") {
    throw badRequest(`${at} is not a text file, so it is not a graph.`, "Write the graph as YAML or JSON.", graphPath);
  }
  // The trailing block is fs_read's metadata, spec/mcp-surface.yaml. Its size is
  // the file's; the text is what actually has to be held and parsed.
  let size = Buffer.byteLength(first.text, "utf8");
  const last = blocks.length > 1 ? blocks[blocks.length - 1] : undefined;
  if (last?.type === "text" && typeof last.text === "string") {
    try {
      const metadata = JSON.parse(last.text);
      if (Number.isInteger(metadata?.size)) size = Math.max(size, metadata.size);
    } catch {
      // not the metadata block; the byte length stands
    }
  }
  if (size > MAX_GRAPH_BYTES) {
    throw tooLarge(`${at} is ${size} bytes, and this engine reads at most ${MAX_GRAPH_BYTES}.`, "Split the graph, or move the data it carries into a file a step reads.", graphPath);
  }

  let doc;
  try {
    doc = parseYaml(first.text);
  } catch (err) {
    throw badRequest(`${at} is not YAML or JSON: ${(err?.message ?? String(err)).split("\n")[0].slice(0, 200)}`, "Fix the syntax and run it again.", graphPath);
  }
  return validateGraph(doc, graphPath);
}

async function runGraph({ client, tools, graphPath, input, chain, deadline }) {
  const at = `The graph ${graphPath}`;
  if (chain.includes(graphPath)) {
    throw badRequest(
      `${graphPath} is already running in this chain, so running it again would not end.`,
      "Break the cycle: a graph may not name itself, directly or through the graphs it runs.",
      graphPath,
    );
  }
  if (chain.length >= MAX_DEPTH) {
    throw badRequest(
      `Running ${graphPath} would nest graphs ${chain.length + 1} deep, and this engine runs at most ${MAX_DEPTH}.`,
      "Flatten the graphs, or run the deeper part as its own run.",
      graphPath,
    );
  }

  const doc = await readGraph(client, graphPath, deadline, at);
  checkRequired(doc, input, graphPath);

  const scope = { input, steps: {} };
  const steps = [];
  for (const step of doc.steps) {
    const named = `Step ${JSON.stringify(step.id)} of ${graphPath}`;
    const instance = `${graphPath}#${step.id}`;
    const args = resolve(step.input ?? {}, scope, named);
    const startedAt = Date.now();

    let raw;
    if (step.tool !== undefined) {
      if (!tools.has(step.tool)) {
        throw notFound(
          `${named} calls ${step.tool}, which is not on this Process's surface.`,
          "Run the Package that provides the tool, then run the graph again. Tool names are <package>_<tool>.",
          instance,
          "Tool not found",
        );
      }
      raw = await callTool(client, step.tool, args, deadline, {
        detail: `${named} called ${step.tool} and it failed:`,
        fix: "Fix the step input, or the tool it calls.",
        instance,
      });
    } else {
      const nested = await runGraph({ client, tools, graphPath: step.workflow, input: args, chain: [...chain, graphPath], deadline });
      raw = nested.outputs;
    }

    const output = step.output === undefined ? (step.tool === undefined ? raw : defaultOutput(raw)) : resolve(step.output, { result: raw }, `The output of ${named}`);
    scope.steps[step.id] = { output };
    steps.push({
      id: step.id,
      ...(step.tool === undefined ? { workflow: step.workflow } : { tool: step.tool }),
      durationMs: Date.now() - startedAt,
      status: "ok",
    });
  }

  const outputs =
    doc.output === undefined
      ? Object.fromEntries(Object.entries(scope.steps).map(([id, entry]) => [id, entry.output]))
      : resolve(doc.output, scope, `The output of ${graphPath}`);

  return { outputs, steps };
}

// ---------------------------------------------------------------- telemetry

// kitbashd traces every call the engine makes, so the only span worth writing
// is the run itself: what ran, how many steps, and whether it failed. Hand
// rolled OTLP/HTTP JSON, the same shape evaluate-latency writes.
const telemetry = { failing: false };

async function exportRun({ graphPath, steps, startedAt, endedAt, failure }) {
  const endpoint = (process.env.KITBASH_TELEMETRY_ENDPOINT ?? "").trim();
  const token = (process.env.KITBASH_TELEMETRY_TOKEN ?? "").trim();
  // A Process given no endpoint is an untraced producer of nothing, PLAN.md 2.4.
  if (endpoint === "" || token === "") return;

  const span = {
    traceId: randomBytes(16).toString("hex"),
    spanId: randomBytes(8).toString("hex"),
    name: "workflow.run",
    kind: 1,
    startTimeUnixNano: `${BigInt(startedAt) * 1_000_000n}`,
    endTimeUnixNano: `${BigInt(endedAt) * 1_000_000n}`,
    attributes: [
      { key: "kitbash.path", value: { stringValue: graphPath } },
      { key: "kitbash.workflow.steps", value: { intValue: `${steps}` } },
    ],
  };
  if (failure) span.status = { code: 2, message: failure.slice(0, 1024) };

  const body = JSON.stringify({ resourceSpans: [{ scopeSpans: [{ scope: { name: SELF }, spans: [span] }] }] });
  try {
    const response = await fetch(`${endpoint.replace(/\/+$/, "")}/v1/traces`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body,
      signal: AbortSignal.timeout(EXPORT_TIMEOUT_MS),
    });
    if (!response.ok) throw new Error(`receiver answered ${response.status}`);
    await response.arrayBuffer().catch(() => {});
    if (telemetry.failing) {
      telemetry.failing = false;
      console.error(`[workflow] writing run spans to ${endpoint} works again`);
    }
  } catch (err) {
    if (!telemetry.failing) {
      telemetry.failing = true;
      console.error(`[workflow] cannot write the run span to ${endpoint}: ${err?.message ?? err}`);
    }
  }
}

// ---------------------------------------------------------------- the tool

async function run(args) {
  const graphPath = args?.path;
  if (typeof graphPath !== "string" || !graphPath.startsWith("/")) {
    throw badRequest(`path is ${JSON.stringify(graphPath)}, which is not an absolute Files path.`, "Call run with the absolute path of a graph file, such as /org/flows/report.yaml.");
  }
  const input = args?.input === undefined ? {} : args.input;
  if (!isPlainObject(input)) {
    throw badRequest("input is not an object.", "Pass the graph's input as an object, or leave it out.", graphPath);
  }

  const endpoint = (process.env.KITBASH_MCP_ENDPOINT ?? "").trim();
  const token = (process.env.KITBASH_TELEMETRY_TOKEN ?? "").trim();
  if (endpoint === "" || token === "") {
    throw internal(
      "This Process was given no MCP endpoint or no token, so it can reach no tools.",
      "Run this Package through kitbash: KITBASH_MCP_ENDPOINT and KITBASH_TELEMETRY_TOKEN come from kitbashd.",
      graphPath,
    );
  }

  const startedAt = Date.now();
  const deadline = startedAt + RUN_TIMEOUT_MS;
  let session;
  let ran = 0;
  let failure;
  try {
    session = await openSession(endpoint, token);
    const tools = await listToolNames(session.client, deadline);
    if (!tools.has("fs_read")) {
      throw internal("The surface this Process reaches has no fs_read, so no graph can be read.", "Report this: a Process session is the owner's whole surface.", graphPath);
    }
    const result = await runGraph({ client: session.client, tools, graphPath, input, chain: [], deadline });
    ran = result.steps.length;
    console.error(`[workflow] ${graphPath} ran ${ran} steps in ${Date.now() - startedAt} ms`);
    return { path: graphPath, outputs: result.outputs, steps: result.steps };
  } catch (err) {
    failure = err instanceof WorkflowError ? `${err.type} ${err.detail}` : `${err?.message ?? err}`;
    throw err;
  } finally {
    if (session) await closeSession(session);
    await exportRun({ graphPath, steps: ran, startedAt, endedAt: Date.now(), failure });
  }
}

const server = new Server({ name: SELF, version: selfVersion }, { capabilities: { tools: {} } });

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: runTool.name,
      description: runTool.description,
      inputSchema: runTool.input,
      outputSchema: runTool.output,
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  if (request.params.name !== runTool.name) {
    return notFound(`This kit has no tool named ${request.params.name}.`, `Call ${runTool.name}.`, request.params.name, "Unknown tool").toResult();
  }
  try {
    const structuredContent = await run(request.params.arguments);
    return { content: [{ type: "text", text: JSON.stringify(structuredContent) }], structuredContent };
  } catch (err) {
    if (err instanceof WorkflowError) return err.toResult();
    console.error(`[workflow] unhandled failure: ${err?.stack ?? err}`);
    return internal("The run failed for a reason this kit did not anticipate.", "Read this kit's stderr in Telemetry, then retry.").toResult();
  }
});

await server.connect(new StdioServerTransport());
console.error(`[workflow] ready, version ${selfVersion}`);
