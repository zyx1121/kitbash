// Drives evaluate-latency against a fake kitbashd receiver: spans go in on the
// fan out path, judgments are caught on the receiver.
//
//   bun run deploy/org/evaluate-latency/test/judge.test.mjs
//
// The kit binds 8080, so this and the observe-count driver run one at a time.
// Exit status is 0 when every check passes.

import { spawn } from "node:child_process";
import path from "node:path";

const kit = path.join(import.meta.dirname, "..", "index.js");
const KIT_URL = "http://127.0.0.1:8080";
const TOKEN = "tok-evaluate";
const PROC = "proc-evaluate-1";
// What kitbashd would have minted for this Process, and what the fan out puts
// on every delivery.
const SECRET = "fanout-secret-evaluate";

const received = [];
const receiver = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const body = await request.text();
    received.push({ path: new URL(request.url).pathname, auth: request.headers.get("authorization"), bytes: body.length, body: JSON.parse(body) });
    return new Response("{}", { headers: { "content-type": "application/json" } });
  },
});
const endpoint = `http://127.0.0.1:${receiver.port}`;

const child = spawn("bun", ["run", kit], {
  stdio: ["ignore", "pipe", "pipe"],
  env: {
    ...process.env,
    KITBASH_TELEMETRY_ENDPOINT: endpoint,
    KITBASH_TELEMETRY_TOKEN: TOKEN,
    KITBASH_PROCESS: PROC,
    KITBASH_FANOUT_SECRET: SECRET,
    KITBASH_EVAL_THRESHOLD_MS: "1000",
  },
});
let stdout = "";
let stderr = "";
child.stdout.on("data", (chunk) => (stdout += chunk));
child.stderr.on("data", (chunk) => (stderr += chunk));

const results = [];
const check = (name, ok, extra = "") => results.push(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` ${extra}` : ""}`);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// Waits for the kit this driver started rather than for whatever else may
// still hold the port: healthz names the kit, and the second kit below is told
// apart by the fan out it was started with.
async function waitForKit(authenticated) {
  for (let i = 0; i < 100; i += 1) {
    try {
      const health = await (await fetch(`${KIT_URL}/healthz`)).json();
      if (health.kit === "evaluate-latency" && health.authenticated === authenticated) return true;
    } catch {}
    await sleep(50);
  }
  return false;
}
check("the kit came up with an authenticated fan out", await waitForKit(true));

// Every POST carries the secret, the way the fan out does. The requests that
// do not are the ones checked below.
const post = (route, body, headers = { authorization: `Bearer ${SECRET}` }) =>
  fetch(`${KIT_URL}${route}`, {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });
const healthz = async () => (await fetch(`${KIT_URL}/healthz`)).json();
const traces = (spans) => post("/v1/traces", { resourceSpans: [{ resource: { attributes: [attr("service.name", "kitbash-mcp")] }, scopeSpans: [{ spans }] }] });
const attributesOf = (record) => Object.fromEntries((record.attributes ?? []).map((entry) => [entry.key, entry.value]));

function attr(key, value) {
  if (typeof value === "boolean") return { key, value: { boolValue: value } };
  return { key, value: { stringValue: value } };
}

const START = 1000000000000000000n;
const span = (name, ms, attributes, ids = {}) => ({
  traceId: ids.traceId ?? "11111111111111111111111111111111",
  spanId: ids.spanId ?? "2222222222222222",
  name,
  startTimeUnixNano: ids.startTimeUnixNano ?? `${START}`,
  endTimeUnixNano: ids.endTimeUnixNano ?? `${START + BigInt(ms) * 1000000n}`,
  attributes,
});

// One fast call, one slow call, one slow child span with no tool, one slow
// judgment, and two spans whose clocks make no sense. Only the slow call is
// judged.
const first = await traces([
  span("fs_list", 500, [attr("kitbash.user", "alice"), attr("kitbash.tool", "fs_list")], { traceId: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", spanId: "aaaaaaaaaaaaaaaa" }),
  span(
    "pkg_build",
    1500,
    [
      attr("kitbash.user", "alice"),
      attr("kitbash.package", "/org/ffmpeg"),
      attr("kitbash.process", "01J0000000000000000000000A"),
      attr("kitbash.path", "/org/ffmpeg"),
      attr("kitbash.tool", "pkg_build"),
      attr("kitbash.producer", "alice"),
    ],
    { traceId: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", spanId: "bbbbbbbbbbbbbbbb" },
  ),
  span("build", 9000, [attr("kitbash.user", "alice")], { traceId: "cccccccccccccccccccccccccccccccc", spanId: "cccccccccccccccc" }),
  span("judge", 9000, [attr("kitbash.user", "alice"), attr("kitbash.tool", "pkg_build"), attr("kitbash.eval", true)], { traceId: "dddddddddddddddddddddddddddddddd", spanId: "dddddddddddddddd" }),
  // Ends before it starts, and a year long call: neither is a duration.
  span("fs_read", 0, [attr("kitbash.user", "alice"), attr("kitbash.tool", "fs_read")], { traceId: "1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a", spanId: "1a1a1a1a1a1a1a1a", startTimeUnixNano: `${START}`, endTimeUnixNano: `${START - 1000n}` }),
  span("fs_read", 0, [attr("kitbash.user", "alice"), attr("kitbash.tool", "fs_read")], { traceId: "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b", spanId: "2b2b2b2b2b2b2b2b", startTimeUnixNano: "0", endTimeUnixNano: "18446744073709551615" }),
]);
check("POST /v1/traces answers 200 with an empty response", first.status === 200 && (await first.clone().text()) === "{}" && first.headers.get("content-type") === "application/json");
check("POST /v1/logs is accepted and judges nothing", (await post("/v1/logs", { resourceLogs: [] })).status === 200);
check("POST /v1/metrics is accepted and judges nothing", (await post("/v1/metrics", { resourceMetrics: [] })).status === 200);

// A body that puts a string where a repeated field belongs is not iterated
// one character at a time.
const notAList = await post("/v1/traces", { resourceSpans: "aaaaaaaaaaaaaaaaaaaa" });
check("a malformed repeated field judges nothing", notAList.status === 200 && (await healthz()).spans === 6, `${notAList.status}`);

// Anything on the host can reach this port; only kitbashd has the secret. This
// kit is run by an admin, so a slow span from a stranger would be a judgment
// about whatever subject the stranger named. It is refused before the body is
// read and judged nowhere, which the single written request below says.
const strangerSpan = (ids) => span("pkg_build", 9000, [attr("kitbash.user", "mallory"), attr("kitbash.tool", "pkg_build"), attr("kitbash.package", "/org/ffmpeg")], ids);
const strangerBody = { resourceSpans: [{ scopeSpans: [{ spans: [strangerSpan({ traceId: "99999999999999999999999999999999", spanId: "9999999999999999" })] }] }] };
const noHeader = await post("/v1/traces", strangerBody, {});
check("a fan out request without the secret is 401", noHeader.status === 401, `status=${noHeader.status}`);
const refusal = await noHeader.json();
check(
  "the 401 is an RFC 9457 problem with a challenge",
  refusal.type.endsWith("/not-permitted") && refusal.status === 401 && /^Bearer /.test(noHeader.headers.get("www-authenticate") ?? ""),
  `${JSON.stringify(refusal).slice(0, 120)} ${noHeader.headers.get("www-authenticate")}`,
);
const wrongSecret = await post("/v1/traces", strangerBody, { authorization: `Bearer ${SECRET}x` });
check("a fan out request with the wrong secret is 401", wrongSecret.status === 401, `status=${wrongSecret.status}`);
const wrongScheme = await post("/v1/logs", { resourceLogs: [] }, { authorization: SECRET });
check("the secret without the Bearer scheme is 401", wrongScheme.status === 401, `status=${wrongScheme.status}`);
check("/healthz stays open", (await fetch(`${KIT_URL}/healthz`)).status === 200);
const refused = await healthz();
check(
  "a refused request is judged nowhere",
  refused.spans === 6 && refused.judged === 1 && refused.received === 4 && refused.refused === 3 && refused.authenticated === true,
  JSON.stringify({ spans: refused.spans, judged: refused.judged, received: refused.received, refused: refused.refused }),
);

check("judgments are held for the batch window", received.length === 0, `${received.length} requests after the posts`);
for (let i = 0; i < 40 && received.length === 0; i += 1) await sleep(100);
check("exactly one request was written", received.length === 1, `${received.length} requests`);
const written = received[0];
check("the judgment went to /v1/logs with the bearer token", written?.path === "/v1/logs" && written?.auth === `Bearer ${TOKEN}`, `${written?.path} ${written?.auth}`);
const records = written?.body?.resourceLogs?.[0]?.scopeLogs?.[0]?.logRecords ?? [];
check("exactly one log record was written", records.length === 1, `${records.length} records`);

const record = records[0] ?? {};
const attrs = attributesOf(record);
check("the record is a WARN", record.severityText === "WARN" && record.severityNumber === 13, `${record.severityText} ${record.severityNumber}`);
check("the body names the tool, the duration and the threshold", record.body?.stringValue === "slow: pkg_build took 1500 ms, threshold 1000 ms", record.body?.stringValue);
check("kitbash.eval is true", attrs["kitbash.eval"]?.boolValue === true, JSON.stringify(attrs["kitbash.eval"]));
check("the verdict is slow", attrs["kitbash.eval.verdict"]?.stringValue === "slow");
check("the duration is a number", attrs["kitbash.eval.duration_ms"]?.doubleValue === 1500, JSON.stringify(attrs["kitbash.eval.duration_ms"]));
check(
  "the subject is named by trace and span id",
  attrs["kitbash.subject.trace_id"]?.stringValue === "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" && attrs["kitbash.subject.span_id"]?.stringValue === "bbbbbbbbbbbbbbbb",
  `${attrs["kitbash.subject.trace_id"]?.stringValue} ${attrs["kitbash.subject.span_id"]?.stringValue}`,
);
check("the tool is copied", attrs["kitbash.tool"]?.stringValue === "pkg_build");
check(
  "the subject's four attributes are copied",
  attrs["kitbash.user"]?.stringValue === "alice" &&
    attrs["kitbash.package"]?.stringValue === "/org/ffmpeg" &&
    attrs["kitbash.process"]?.stringValue === "01J0000000000000000000000A" &&
    attrs["kitbash.path"]?.stringValue === "/org/ffmpeg",
  JSON.stringify(Object.keys(attrs)),
);
check("the judgment carries no producer claim of its own", attrs["kitbash.producer"] === undefined);

let health = await healthz();
check("healthz reports 6 spans seen and 1 judged", health.spans === 6 && health.judged === 1 && health.written === 1, JSON.stringify({ spans: health.spans, judged: health.judged, written: health.written }));
check("healthz reports the threshold", health.thresholdMs === 1000);

// Two slow calls inside one window are one request, and base64 identifiers are
// written back as hex.
received.length = 0;
await traces([span("fs_read", 3000, [attr("kitbash.user", "bob"), attr("kitbash.tool", "fs_read")], { traceId: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", spanId: "eeeeeeeeeeeeeeee" })]);
await traces([
  span("proc_run", 2000, [attr("kitbash.user", "bob"), attr("kitbash.tool", "proc_run")], {
    // The identifiers a protojson based producer sends for the same bytes.
    traceId: Buffer.from("ffffffffffffffffffffffffffffffff", "hex").toString("base64"),
    spanId: Buffer.from("ffffffffffffffff", "hex").toString("base64"),
  }),
]);
for (let i = 0; i < 40 && received.length === 0; i += 1) await sleep(100);
await sleep(200);
check("two judgments in one window are one request", received.length === 1, `${received.length} requests`);
const batch = received[0]?.body?.resourceLogs?.[0]?.scopeLogs?.[0]?.logRecords ?? [];
check("the batch carries both judgments", batch.length === 2, `${batch.length} records`);
const second = attributesOf(batch[1] ?? {});
check(
  "a base64 identifier is written back as hex",
  second["kitbash.subject.trace_id"]?.stringValue === "ffffffffffffffffffffffffffffffff" && second["kitbash.subject.span_id"]?.stringValue === "ffffffffffffffff",
  JSON.stringify(second["kitbash.subject.trace_id"]),
);

// A subject with enormous attributes must not become a judgment kitbashd
// refuses along with everything batched next to it.
received.length = 0;
// 5 times 700 KiB fits in one delivery, and copying it whole would put more
// than 4 MiB back on the wire once the tool is repeated in the body.
const huge = "h".repeat(700 * 1024);
const delivered = await traces([
  span("huge", 5000, [attr("kitbash.user", huge), attr("kitbash.package", huge), attr("kitbash.process", huge), attr("kitbash.path", huge), attr("kitbash.tool", huge)], {
    traceId: "abababababababababababababababab",
    spanId: "abababababababab",
  }),
]);
check("a delivery of large attributes is accepted", delivered.status === 200, `status=${delivered.status}`);
for (let i = 0; i < 40 && received.length === 0; i += 1) await sleep(100);
const clipped = attributesOf(received[0]?.body?.resourceLogs?.[0]?.scopeLogs?.[0]?.logRecords?.[0] ?? {});
check(
  "every copied attribute is clipped to 1 KiB",
  ["kitbash.user", "kitbash.package", "kitbash.process", "kitbash.path", "kitbash.tool"].every((key) => clipped[key]?.stringValue.length === 1024),
  JSON.stringify(Object.entries(clipped).map(([key, value]) => [key, value.stringValue?.length ?? null])),
);
check("the tool in the body is clipped too", (received[0]?.body?.resourceLogs?.[0]?.scopeLogs?.[0]?.logRecords?.[0]?.body?.stringValue ?? "").length < 1100);
check("the export stays well under the receiver's cap", received[0]?.bytes < 16 * 1024, `${received[0]?.bytes} bytes`);

// The body cap.
const oversized = await post("/v1/traces", "a".repeat(4 * 1024 * 1024 + 1));
check("a body of 4 MiB + 1 is 413", oversized.status === 413, `status=${oversized.status}`);
const problem = await oversized.json();
check("the 413 is an RFC 9457 problem", problem.type.endsWith("/too-large") && problem.status === 413);

check("nothing is written to stdout", stdout === "", JSON.stringify(stdout.slice(0, 200)));
check("the kit logs to stderr", /judging spans slower than 1000 ms/.test(stderr), stderr.split("\n")[0]);

// A judgment made inside the window is still written when the Process stops.
received.length = 0;
await traces([span("pkg_build", 4000, [attr("kitbash.user", "carol"), attr("kitbash.tool", "pkg_build")], { traceId: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", spanId: "cdcdcdcdcdcdcdcd" })]);
child.kill("SIGTERM");
await sleep(1500);
check("the queued judgment is flushed on shutdown", received.length === 1, `${received.length} requests after SIGTERM`);
check("the shutdown is logged", /SIGTERM, stopping with 1 judgments queued/.test(stderr), stderr.split("\n").filter((line) => line.includes("SIGTERM"))[0] ?? "");

child.kill("SIGKILL");
receiver.stop(true);

// A threshold that is not a number falls back to 1000 and says so.
await sleep(200);
const fallback = spawn("bun", ["run", kit], { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, KITBASH_EVAL_THRESHOLD_MS: "soon" } });
let fallbackErr = "";
fallback.stderr.on("data", (chunk) => (fallbackErr += chunk));
check("the second kit came up with an unauthenticated fan out", await waitForKit(false));
check("an unparsable threshold falls back to 1000", (await healthz()).thresholdMs === 1000);
check("the fallback is logged on stderr", /KITBASH_EVAL_THRESHOLD_MS is "soon"/.test(fallbackErr), fallbackErr.split("\n")[0]);
// That kit was started without a fan out secret, which is a kitbashd from
// before the fan out was authenticated: it accepts what arrives and says so
// once. The Process is the same one an upgrade leaves running.
const legacyPost = await post("/v1/traces", { resourceSpans: [{ scopeSpans: [{ spans: [span("fs_list", 100, [attr("kitbash.user", "alice"), attr("kitbash.tool", "fs_list")])] }] }] }, {});
const legacyHealth = await healthz();
check("without a secret a request carrying none is accepted", legacyPost.status === 200 && legacyHealth.spans === 1, `status=${legacyPost.status} spans=${legacyHealth.spans}`);
check("healthz says the fan out is unauthenticated", legacyHealth.authenticated === false && legacyHealth.refused === 0);
check(
  "the unauthenticated fan out is logged once at start",
  fallbackErr.split("\n").filter((line) => line.includes("KITBASH_FANOUT_SECRET is not set")).length === 1,
  fallbackErr.split("\n").filter((line) => line.includes("KITBASH_FANOUT_SECRET")).length.toString(),
);
fallback.kill("SIGKILL");
// The other driver binds this same port, so this one leaves it free.
for (let i = 0; i < 100; i += 1) {
  try {
    await fetch(`${KIT_URL}/healthz`);
    await sleep(50);
  } catch {
    break;
  }
}

console.log(results.join("\n"));
const failed = results.some((line) => line.startsWith("FAIL"));
console.log(failed ? "RESULT: FAIL" : "RESULT: PASS");
if (failed) console.log(`--- stderr ---\n${stderr.slice(-2000)}`);
process.exit(failed ? 1 : 0);
