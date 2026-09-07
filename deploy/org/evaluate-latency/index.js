// evaluate-latency: the smallest kitbash evaluate kit.
//
// It subscribes to the Telemetry fan out, looks at every surface span, and for
// each one slower than the threshold writes one log record back with
// kitbash.eval true. The judgment names its subject by trace and span id and
// repeats the subject's kitbash.user, kitbash.package, kitbash.process and
// kitbash.path, so the query that asks what a Package did also returns how it
// was judged, PLAN.md 2.4.
//
// What it judges: a span whose attributes carry kitbash.tool, which is the
// surface span of a tools/call and not one of its children, and which is not
// itself an evaluation. Judgments are batched for up to two seconds so a burst
// of slow calls costs one request.
//
// The verdict is a threshold, not a model. The point is that the evaluate hook
// needs nothing more than a subscriber that writes back.
//
// Environment, all supplied by kitbashd: KITBASH_TELEMETRY_ENDPOINT and
// KITBASH_TELEMETRY_TOKEN say where and how to write back, KITBASH_PROCESS
// names this Process. KITBASH_EVAL_THRESHOLD_MS is the threshold in
// milliseconds and defaults to 1000.
//
// Nothing is written to stdout. This Package speaks HTTP, not stdio, and its
// log is stderr.

import { createServer } from "node:http";

const PORT = 8080;
const HOST = "0.0.0.0";
// The same cap kitbashd's own receiver applies, spec/kitbashd-api.yaml.
const MAX_BODY_BYTES = 4 * 1024 * 1024;
const DEFAULT_THRESHOLD_MS = 1000;
const BATCH_WINDOW_MS = 2 * 1000;
// A burst larger than this is written immediately rather than held, so the
// queue cannot grow without bound between windows.
const MAX_BATCH = 512;
const EXPORT_TIMEOUT_MS = 5 * 1000;
// SEVERITY_NUMBER_WARN.
const WARN = 13;
const SELF = "evaluate-latency";

function readThreshold() {
  const raw = process.env.KITBASH_EVAL_THRESHOLD_MS;
  if (raw === undefined || raw.trim() === "") return DEFAULT_THRESHOLD_MS;
  const value = Number(raw);
  if (!Number.isFinite(value) || value < 0) {
    console.error(`[evaluate-latency] KITBASH_EVAL_THRESHOLD_MS is ${JSON.stringify(raw)}, which is not a number of milliseconds; using ${DEFAULT_THRESHOLD_MS}`);
    return DEFAULT_THRESHOLD_MS;
  }
  return value;
}

const thresholdMs = readThreshold();
const selfProcess = (process.env.KITBASH_PROCESS ?? "").trim();

const state = { received: 0, spans: 0, judged: 0, written: 0, batches: 0, failures: 0, failing: false, lastError: null };

// ---------------------------------------------------------------- OTLP JSON

function anyValue(value) {
  if (!value || typeof value !== "object") return undefined;
  if ("stringValue" in value) return value.stringValue;
  if ("boolValue" in value) return value.boolValue;
  if ("intValue" in value) return Number(value.intValue);
  if ("doubleValue" in value) return value.doubleValue;
  return undefined;
}

function attributes(list) {
  const out = {};
  if (!Array.isArray(list)) return out;
  for (const entry of list) {
    if (!entry || typeof entry.key !== "string") continue;
    out[entry.key] = anyValue(entry.value);
  }
  return out;
}

// Resource attributes are merged into every record, the record winning, the
// same rule kitbashd's receiver applies before it stores anything.
function merge(resource, record) {
  return { ...resource, ...attributes(record) };
}

// The fan out sends hex identifiers, as the OTLP JSON encoding requires. A
// protojson based producer sends base64 for the same bytes, and the two never
// collide because the lengths differ, so both are accepted and hex is what the
// judgment carries.
function hexId(value, hexLength) {
  if (typeof value !== "string" || value === "") return undefined;
  if (value.length === hexLength && /^[a-f0-9]+$/i.test(value)) return value.toLowerCase();
  const bytes = hexLength / 2;
  // Padded base64 of 16 bytes is 24 characters, of 8 bytes 12, neither of which
  // is 32 or 16, so the two encodings cannot be confused.
  if (value.length === 4 * Math.ceil(bytes / 3)) {
    const raw = Buffer.from(value, "base64");
    if (raw.length === bytes) return raw.toString("hex");
  }
  return undefined;
}

function nanos(value) {
  if (typeof value === "number" && Number.isFinite(value)) return BigInt(Math.trunc(value));
  if (typeof value === "string" && /^[0-9]+$/.test(value)) return BigInt(value);
  return undefined;
}

function durationMsOf(span) {
  const start = nanos(span?.startTimeUnixNano ?? span?.start_time_unix_nano);
  const end = nanos(span?.endTimeUnixNano ?? span?.end_time_unix_nano);
  if (start === undefined || end === undefined || end < start) return undefined;
  // Microsecond resolution is more than a judgment needs and keeps the number
  // readable in the body of the record.
  return Number((end - start) / 1000n) / 1000;
}

// ---------------------------------------------------------------- judging

const pending = [];
let batchTimer = null;

function judge(span, attrs) {
  const tool = attrs["kitbash.tool"];
  // Not a surface span: a build or run child carries no tool.
  if (typeof tool !== "string" || tool === "") return;
  // A judgment is not judged, whatever kitbash.eval says.
  if (attrs["kitbash.eval"] !== undefined) return;

  const durationMs = durationMsOf(span);
  if (durationMs === undefined || durationMs <= thresholdMs) return;

  const traceId = hexId(span?.traceId ?? span?.trace_id, 32);
  const spanId = hexId(span?.spanId ?? span?.span_id, 16);
  if (!traceId || !spanId) {
    console.error(`[evaluate-latency] a slow ${tool} span carries no usable trace or span id, so it cannot be judged`);
    return;
  }

  const attributesOut = [
    { key: "kitbash.eval", value: { boolValue: true } },
    { key: "kitbash.eval.verdict", value: { stringValue: "slow" } },
    { key: "kitbash.eval.duration_ms", value: { doubleValue: durationMs } },
    { key: "kitbash.subject.trace_id", value: { stringValue: traceId } },
    { key: "kitbash.subject.span_id", value: { stringValue: spanId } },
    { key: "kitbash.tool", value: { stringValue: tool } },
  ];
  // The subject's own four attributes, copied so the judgment answers the same
  // query as the call. kitbashd keeps them only when this kit runs as an admin;
  // for a member's kit they are stamped as the member's own, which is the same
  // value anyway.
  for (const key of ["kitbash.user", "kitbash.package", "kitbash.process", "kitbash.path"]) {
    const value = attrs[key];
    if (typeof value === "string" && value !== "") attributesOut.push({ key, value: { stringValue: value } });
  }

  const timeUnixNano = `${BigInt(Date.now()) * 1_000_000n}`;
  pending.push({
    timeUnixNano,
    observedTimeUnixNano: timeUnixNano,
    severityNumber: WARN,
    severityText: "WARN",
    body: { stringValue: `slow: ${tool} took ${durationMs} ms, threshold ${thresholdMs} ms` },
    attributes: attributesOut,
  });
  state.judged += 1;

  if (pending.length >= MAX_BATCH) {
    flush();
    return;
  }
  if (batchTimer === null) {
    batchTimer = setTimeout(() => {
      batchTimer = null;
      flush();
    }, BATCH_WINDOW_MS);
  }
}

function flush() {
  if (batchTimer !== null) {
    clearTimeout(batchTimer);
    batchTimer = null;
  }
  if (pending.length === 0) return;
  const records = pending.splice(0, pending.length);
  write(records).catch((err) => console.error(`[evaluate-latency] writing judgments failed unexpectedly: ${err?.stack ?? err}`));
}

async function write(records) {
  const endpoint = (process.env.KITBASH_TELEMETRY_ENDPOINT ?? "").trim();
  const token = (process.env.KITBASH_TELEMETRY_TOKEN ?? "").trim();
  // A Process that was given no endpoint is an untraced producer of nothing,
  // PLAN.md 2.4. The judgments are still counted on /healthz.
  if (endpoint === "" || token === "") {
    console.error(`[evaluate-latency] no telemetry endpoint, dropping ${records.length} judgments`);
    return;
  }

  const body = JSON.stringify({
    resourceLogs: [{ scopeLogs: [{ scope: { name: SELF }, logRecords: records }] }],
  });

  try {
    const response = await fetch(`${endpoint.replace(/\/+$/, "")}/v1/logs`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body,
      signal: AbortSignal.timeout(EXPORT_TIMEOUT_MS),
    });
    if (!response.ok) throw new Error(`receiver answered ${response.status}`);
    // Drain, so the connection can be reused rather than reset.
    await response.arrayBuffer().catch(() => {});
    state.batches += 1;
    state.written += records.length;
    if (state.failing) {
      state.failing = false;
      state.lastError = null;
      console.error(`[evaluate-latency] writing judgments to ${endpoint} works again`);
    }
  } catch (err) {
    state.failures += 1;
    state.lastError = err?.message ?? String(err);
    if (!state.failing) {
      state.failing = true;
      console.error(`[evaluate-latency] cannot write ${records.length} judgments to ${endpoint}: ${state.lastError}`);
    }
  }
}

// ---------------------------------------------------------------- ingest

function ingestTraces(document) {
  for (const resourceSpans of document?.resourceSpans ?? []) {
    const resource = attributes(resourceSpans?.resource?.attributes);
    for (const scopeSpans of resourceSpans?.scopeSpans ?? []) {
      for (const span of scopeSpans?.spans ?? []) {
        state.spans += 1;
        judge(span, merge(resource, span?.attributes));
      }
    }
  }
}

// Logs and metrics are accepted because the fan out delivers all three signals
// to a subscriber, and refusing them would log a delivery failure on kitbashd
// for every record. This kit judges spans only.
function ignore() {}

// ---------------------------------------------------------------- HTTP

const PROBLEM = "application/problem+json";

function problem(response, status, slug, title, detail, fix) {
  const body = JSON.stringify({
    type: `https://kitbash.zyx.tw/errors/${slug}`,
    title,
    status,
    detail,
    fix,
  });
  response.writeHead(status, { "content-type": PROBLEM, "content-length": Buffer.byteLength(body) });
  response.end(body);
}

function json(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(body) });
  response.end(body);
}

// Reads the body with the cap applied as it arrives, so an oversized request is
// refused rather than buffered.
function readBody(request, response) {
  return new Promise((resolve) => {
    const chunks = [];
    let size = 0;
    let done = false;
    request.on("data", (chunk) => {
      if (done) return;
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        done = true;
        problem(
          response,
          413,
          "too-large",
          "Body too large",
          `This kit accepts at most ${MAX_BODY_BYTES} bytes per export.`,
          "Send smaller batches; the fan out already splits its requests.",
        );
        request.destroy();
        resolve(null);
        return;
      }
      chunks.push(chunk);
    });
    request.on("end", () => {
      if (done) return;
      done = true;
      resolve(Buffer.concat(chunks));
    });
    request.on("error", () => {
      if (done) return;
      done = true;
      resolve(null);
    });
  });
}

const INGEST = {
  "/v1/traces": ingestTraces,
  "/v1/logs": ignore,
  "/v1/metrics": ignore,
};

const server = createServer(async (request, response) => {
  const url = new URL(request.url ?? "/", "http://localhost");
  const route = url.pathname;

  if (route === "/healthz") {
    if (request.method !== "GET") {
      problem(response, 405, "bad-request", "Method not allowed", "/healthz answers GET.", "Send GET /healthz.");
      return;
    }
    json(response, 200, {
      kit: SELF,
      process: selfProcess || null,
      thresholdMs,
      received: state.received,
      spans: state.spans,
      judged: state.judged,
      written: state.written,
      batches: state.batches,
      pending: pending.length,
      failures: state.failures,
      writing: !state.failing,
      lastError: state.lastError,
    });
    return;
  }

  const ingest = INGEST[route];
  if (!ingest) {
    problem(response, 404, "not-found", "No such path", `This kit serves ${Object.keys(INGEST).join(", ")} and /healthz.`, "POST OTLP/HTTP JSON to one of the standard paths.");
    return;
  }
  if (request.method !== "POST") {
    problem(response, 405, "bad-request", "Method not allowed", `${route} accepts POST.`, "POST an OTLP/HTTP JSON export.");
    return;
  }

  const body = await readBody(request, response);
  if (body === null) return;

  let document;
  try {
    document = JSON.parse(body.toString("utf8"));
  } catch (err) {
    console.error(`[evaluate-latency] ${route} received a body that is not JSON: ${err.message}`);
    problem(response, 400, "bad-request", "Body is not OTLP JSON", "The request body did not parse as JSON.", "POST OTLP/HTTP JSON, the encoding the fan out uses.");
    return;
  }

  try {
    ingest(document);
    state.received += 1;
  } catch (err) {
    console.error(`[evaluate-latency] ${route} could not be judged: ${err?.stack ?? err}`);
    problem(response, 400, "bad-request", "Body is not an OTLP export", "The request body is JSON but not an OTLP export for this signal.", "POST the export message the standard path expects.");
    return;
  }

  // The OTLP success response is an empty Export*ServiceResponse.
  json(response, 200, {});
});

server.on("clientError", (err, socket) => {
  if (socket.writable) socket.end("HTTP/1.1 400 Bad Request\r\nconnection: close\r\n\r\n");
});

function shutdown(signal) {
  console.error(`[evaluate-latency] ${signal}, stopping`);
  flush();
  server.close(() => process.exit(0));
}
process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));

server.listen(PORT, HOST, () => {
  console.error(`[evaluate-latency] listening on ${HOST}:${PORT}, judging spans slower than ${thresholdMs} ms`);
});
