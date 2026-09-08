// observe-count: the smallest kitbash observe kit.
//
// It subscribes to the Telemetry fan out, counts every span, log record and
// metric data point kitbashd delivers, per member and in total, and every ten
// seconds writes the totals back as three cumulative counters. That closes the
// loop the observe hook describes: records go out of kitbashd, a number about
// them comes back in, and both are queryable through tel_query.
//
// It receives OTLP/HTTP JSON on the three standard paths, which is what the fan
// out POSTs, and answers GET /healthz with the same counters so a Process can
// be inspected without a query.
//
// The three paths take records from kitbashd only: every fan out request
// carries Authorization: Bearer KITBASH_FANOUT_SECRET, and anything else on
// the host that can reach this port is answered 401 and counted nowhere. A kit
// started without that secret, by a kitbashd from before the fan out was
// authenticated, accepts what arrives and says so once at start.
//
// The kit adds no attributes of its own to what it exports. kitbashd stamps
// kitbash.user, kitbash.package and kitbash.process from the Process token, and
// kitbash.producer names this kit's Process, so a count is never mistaken for
// the records it counted.
//
// Environment, all supplied by kitbashd: KITBASH_TELEMETRY_ENDPOINT and
// KITBASH_TELEMETRY_TOKEN say where and how to write back, KITBASH_PROCESS
// names this Process, KITBASH_FANOUT_SECRET is the bearer every fan out request
// carries. KITBASH_OBSERVE_INTERVAL_MS overrides the ten seconds.
//
// Nothing is written to stdout. This Package speaks HTTP, not stdio, and its
// log is stderr.

import { createHash, timingSafeEqual } from "node:crypto";
import { createServer } from "node:http";

const PORT = 8080;
const HOST = "0.0.0.0";
// The same cap kitbashd's own receiver applies, spec/kitbashd-api.yaml.
const MAX_BODY_BYTES = 4 * 1024 * 1024;
const DEFAULT_INTERVAL_MS = 10 * 1000;
const EXPORT_TIMEOUT_MS = 5 * 1000;
// The budget for the last export when the Process is stopping, the same three
// seconds kitbash-mcp gives its own shutdown flush.
const SHUTDOWN_TIMEOUT_MS = 3 * 1000;
// kitbash-mcp's AttributeValueLimit. An attribute arrives from a producer this
// kit does not control, and a member name is a map key here, so it is truncated
// on the way in rather than kept whole.
const ATTRIBUTE_VALUE_LIMIT = 1 << 10;
// How many members the map holds before the rest share one bucket, and how many
// of them /healthz serialises. Without both, one producer sending a fresh
// kitbash.user per record is an unbounded map and an unbounded response.
const MAX_USERS = 1024;
const HEALTHZ_USERS = 50;
const OTHER = "other";
const UNKNOWN = "unknown";
// AGGREGATION_TEMPORALITY_CUMULATIVE. The counters run from process start and
// are never reset, so a restart is visible as the reset it is.
const CUMULATIVE = 2;
const SELF = "observe-count";

const startTimeUnixNano = `${BigInt(Date.now()) * 1_000_000n}`;

function readInterval() {
  const raw = process.env.KITBASH_OBSERVE_INTERVAL_MS;
  if (raw === undefined || raw.trim() === "") return DEFAULT_INTERVAL_MS;
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    console.error(`[observe-count] KITBASH_OBSERVE_INTERVAL_MS is ${JSON.stringify(raw)}, which is not a positive number of milliseconds; using ${DEFAULT_INTERVAL_MS}`);
    return DEFAULT_INTERVAL_MS;
  }
  return value;
}

const intervalMs = readInterval();
// Set when kitbashd registered this Process. A record whose kitbash.producer is
// this Process is one of our own exports coming back around, and counting it
// would make the counters count themselves.
const selfProcess = (process.env.KITBASH_PROCESS ?? "").trim();
// The bearer kitbashd minted for this Process. Only kitbashd has it, so a
// request carrying it came from the fan out and not from a neighbour on the
// host that found this loopback port. An empty one means this kit was started
// by a kitbashd from before the fan out was authenticated: it accepts what
// arrives, the way it always did, and says so once at start.
const fanoutSecret = (process.env.KITBASH_FANOUT_SECRET ?? "").trim();
const expectedAuthorization = fanoutSecret === "" ? "" : `Bearer ${fanoutSecret}`;

// ---------------------------------------------------------------- counters

const totals = { spans: 0, logs: 0, metrics: 0 };
const byUser = new Map();
const state = { received: 0, refused: 0, ignored: 0, exported: 0, exportFailures: 0, skipped: 0, overflowUsers: 0, failing: false, lastError: null };

// Truncated the way kitbash-mcp truncates an attribute, and stripped of control
// characters so a member name cannot carry an escape sequence into a log line
// or into the /healthz response.
function clipValue(text) {
  if (typeof text !== "string") return "";
  const flat = text
    .replace(/[\u0000-\u001f\u007f]/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  return flat.length <= ATTRIBUTE_VALUE_LIMIT ? flat : flat.slice(0, ATTRIBUTE_VALUE_LIMIT);
}

// The bucket a record counts towards. Past MAX_USERS distinct members the rest
// share one bucket: the totals stay exact, only the split stops growing.
function bucketFor(user) {
  const name = clipValue(user);
  if (name === "") return UNKNOWN;
  if (byUser.has(name)) return name;
  // One slot of the cap belongs to the overflow bucket, so the map never holds
  // more than MAX_USERS keys in all.
  if (byUser.size >= MAX_USERS - 1) {
    state.overflowUsers += 1;
    return OTHER;
  }
  return name;
}

function count(signal, user, n = 1) {
  totals[signal] += n;
  const name = bucketFor(user);
  let bucket = byUser.get(name);
  if (!bucket) {
    bucket = { spans: 0, logs: 0, metrics: 0 };
    byUser.set(name, bucket);
  }
  bucket[signal] += n;
}

// The members with the most records, which is what an agent asking how much
// Telemetry each person produces wants. The totals above are always exact.
function topUsers() {
  return Object.fromEntries(
    [...byUser.entries()]
      .sort(([, a], [, b]) => b.spans + b.logs + b.metrics - (a.spans + a.logs + a.metrics))
      .slice(0, HEALTHZ_USERS),
  );
}

// ---------------------------------------------------------------- OTLP JSON

// An OTLP/HTTP JSON AnyValue, reduced to the JavaScript value it stands for.
// Only the shapes an attribute actually takes are handled; anything else is
// undefined and simply does not match a comparison.
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

function isOwn(attrs) {
  return selfProcess !== "" && attrs["kitbash.producer"] === selfProcess;
}

// Every level of an OTLP document is a repeated field, and a body that puts a
// string where one belongs would otherwise iterate one character at a time.
function list(value) {
  return Array.isArray(value) ? value : [];
}

function dataPointsOf(metric) {
  for (const kind of ["sum", "gauge", "histogram", "exponentialHistogram", "summary"]) {
    const body = metric?.[kind];
    if (body && Array.isArray(body.dataPoints)) return body.dataPoints;
  }
  return [];
}

function ingestTraces(document) {
  for (const resourceSpans of list(document?.resourceSpans)) {
    const resource = attributes(resourceSpans?.resource?.attributes);
    for (const scopeSpans of list(resourceSpans?.scopeSpans)) {
      for (const span of list(scopeSpans?.spans)) {
        const attrs = merge(resource, span?.attributes);
        if (isOwn(attrs)) {
          state.ignored += 1;
          continue;
        }
        count("spans", attrs["kitbash.user"]);
      }
    }
  }
}

function ingestLogs(document) {
  for (const resourceLogs of list(document?.resourceLogs)) {
    const resource = attributes(resourceLogs?.resource?.attributes);
    for (const scopeLogs of list(resourceLogs?.scopeLogs)) {
      for (const record of list(scopeLogs?.logRecords)) {
        const attrs = merge(resource, record?.attributes);
        if (isOwn(attrs)) {
          state.ignored += 1;
          continue;
        }
        count("logs", attrs["kitbash.user"]);
      }
    }
  }
}

function ingestMetrics(document) {
  for (const resourceMetrics of list(document?.resourceMetrics)) {
    const resource = attributes(resourceMetrics?.resource?.attributes);
    for (const scopeMetrics of list(resourceMetrics?.scopeMetrics)) {
      for (const metric of list(scopeMetrics?.metrics)) {
        for (const point of dataPointsOf(metric)) {
          const attrs = merge(resource, point?.attributes);
          if (isOwn(attrs)) {
            state.ignored += 1;
            continue;
          }
          count("metrics", attrs["kitbash.user"]);
        }
      }
    }
  }
}

// ---------------------------------------------------------------- writing back

function counterMetric(name, value, timeUnixNano) {
  return {
    name,
    unit: "1",
    sum: {
      // No attributes: everything that identifies these numbers is stamped by
      // kitbashd from the Process token.
      dataPoints: [{ startTimeUnixNano, timeUnixNano, asInt: `${value}` }],
      aggregationTemporality: CUMULATIVE,
      isMonotonic: true,
    },
  };
}

// One export at a time. The counters are cumulative, so a tick that arrives
// while a slow receiver is still reading the last one is skipped rather than
// queued: the next export carries the same numbers.
let exporting = false;

async function exportCounts() {
  const endpoint = (process.env.KITBASH_TELEMETRY_ENDPOINT ?? "").trim();
  const token = (process.env.KITBASH_TELEMETRY_TOKEN ?? "").trim();
  // A Process that was given no endpoint is an untraced producer of nothing,
  // PLAN.md 2.4. It still counts, and /healthz still answers.
  if (endpoint === "" || token === "") return;
  if (exporting) {
    state.skipped += 1;
    return;
  }
  exporting = true;

  const timeUnixNano = `${BigInt(Date.now()) * 1_000_000n}`;
  const body = JSON.stringify({
    resourceMetrics: [
      {
        scopeMetrics: [
          {
            scope: { name: SELF },
            metrics: [
              counterMetric("kitbash.observed.spans", totals.spans, timeUnixNano),
              counterMetric("kitbash.observed.logs", totals.logs, timeUnixNano),
              counterMetric("kitbash.observed.metrics", totals.metrics, timeUnixNano),
            ],
          },
        ],
      },
    ],
  });

  try {
    const response = await fetch(`${endpoint.replace(/\/+$/, "")}/v1/metrics`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
      body,
      signal: AbortSignal.timeout(EXPORT_TIMEOUT_MS),
    });
    if (!response.ok) throw new Error(`receiver answered ${response.status}`);
    // Drain, so the connection can be reused rather than reset.
    await response.arrayBuffer().catch(() => {});
    state.exported += 1;
    if (state.failing) {
      state.failing = false;
      state.lastError = null;
      console.error(`[observe-count] writing counters back to ${endpoint} works again`);
    }
  } catch (err) {
    state.exportFailures += 1;
    state.lastError = err?.message ?? String(err);
    if (!state.failing) {
      state.failing = true;
      console.error(`[observe-count] cannot write counters back to ${endpoint}: ${state.lastError}`);
    }
  } finally {
    exporting = false;
  }
}

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

// The two headers are compared as fixed width digests, so the comparison takes
// the same time whatever arrives: neither the length of the secret nor how much
// of it a caller guessed right is readable from how long the answer took.
function sameSecret(given) {
  const digest = (value) => createHash("sha256").update(value, "utf8").digest();
  return timingSafeEqual(digest(given), digest(expectedAuthorization));
}

// Whether one request may feed this kit. A kit that was given no secret takes
// what it is given, which is what an upgrade of kitbashd under a running
// Process looks like.
function authorized(request) {
  if (expectedAuthorization === "") return true;
  return sameSecret(request.headers.authorization ?? "");
}

// RFC 9110 wants a challenge with every 401, and the only 401 this kit answers
// is a fan out request that did not come from kitbashd.
function unauthorized(response) {
  response.setHeader("www-authenticate", `Bearer realm="${SELF}"`);
  // The body of the refused request is never read, so this connection has an
  // unread request on it and cannot carry another. Saying so is what keeps a
  // client from reusing it and reading a reset instead of an answer.
  response.setHeader("connection", "close");
  problem(
    response,
    401,
    "not-permitted",
    "Not permitted",
    "This kit takes records from the kitbashd fan out only, and this request did not carry its secret.",
    "Nothing to fix from the outside: kitbashd sends the secret it minted for this Process.",
  );
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
  "/v1/logs": ingestLogs,
  "/v1/metrics": ingestMetrics,
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
      intervalMs,
      totals,
      // The busiest members only, so this response has a bounded size whatever
      // a producer sends. userCount says how many there are in all.
      users: topUsers(),
      userCount: byUser.size,
      usersTruncated: byUser.size > HEALTHZ_USERS,
      overflowUsers: state.overflowUsers,
      // Whether the fan out is authenticated, and how many requests were
      // refused because they carried no secret of kitbashd's or the wrong one.
      authenticated: expectedAuthorization !== "",
      received: state.received,
      refused: state.refused,
      ignored: state.ignored,
      exported: state.exported,
      exportFailures: state.exportFailures,
      skipped: state.skipped,
      exporting: !state.failing,
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
  // Before the body is read, so an unauthenticated caller cannot make this kit
  // buffer 4 MiB, and nothing it sent is counted.
  if (!authorized(request)) {
    state.refused += 1;
    unauthorized(response);
    return;
  }

  const body = await readBody(request, response);
  if (body === null) return;

  let document;
  try {
    document = JSON.parse(body.toString("utf8"));
  } catch (err) {
    console.error(`[observe-count] ${route} received a body that is not JSON: ${err.message}`);
    problem(response, 400, "bad-request", "Body is not OTLP JSON", "The request body did not parse as JSON.", "POST OTLP/HTTP JSON, the encoding the fan out uses.");
    return;
  }

  try {
    ingest(document);
    state.received += 1;
  } catch (err) {
    console.error(`[observe-count] ${route} could not be counted: ${err?.stack ?? err}`);
    problem(response, 400, "bad-request", "Body is not an OTLP export", "The request body is JSON but not an OTLP export for this signal.", "POST the export message the standard path expects.");
    return;
  }

  // The OTLP success response is an empty Export*ServiceResponse.
  json(response, 200, {});
});

server.on("clientError", (err, socket) => {
  if (socket.writable) socket.end("HTTP/1.1 400 Bad Request\r\nconnection: close\r\n\r\n");
});

const timer = setInterval(() => {
  exportCounts().catch((err) => console.error(`[observe-count] export failed unexpectedly: ${err?.stack ?? err}`));
}, intervalMs);

let stopping = false;

// One last export on the way out, so the counts of the final interval are not
// lost with the Process, bounded by the same three seconds kitbash-mcp gives
// its own flush.
async function shutdown(signal) {
  if (stopping) return;
  stopping = true;
  console.error(`[observe-count] ${signal}, stopping`);
  clearInterval(timer);
  server.close();
  const deadline = new Promise((resolve) => setTimeout(resolve, SHUTDOWN_TIMEOUT_MS));
  await Promise.race([exportCounts().catch(() => {}), deadline]);
  process.exit(0);
}
process.on("SIGTERM", () => shutdown("SIGTERM"));
process.on("SIGINT", () => shutdown("SIGINT"));

server.listen(PORT, HOST, () => {
  console.error(`[observe-count] listening on ${HOST}:${PORT}, reporting every ${intervalMs} ms`);
  if (expectedAuthorization === "") {
    console.error("[observe-count] KITBASH_FANOUT_SECRET is not set: the fan out is unauthenticated and this kit counts records from anything on the host that can reach its port; run the Process again on a kitbashd that mints one");
  }
});
