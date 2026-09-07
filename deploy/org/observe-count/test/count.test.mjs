// Drives observe-count against a fake kitbashd receiver: the fan out is POSTed
// in, the counters are read back on /healthz, and the periodic export is caught
// on the receiver.
//
//   bun run deploy/org/observe-count/test/count.test.mjs
//
// The kit binds 8080, so this and the evaluate-latency driver run one at a
// time. Exit status is 0 when every check passes.

import { spawn } from "node:child_process";
import path from "node:path";

const kit = path.join(import.meta.dirname, "..", "index.js");
const KIT_URL = "http://127.0.0.1:8080";
const TOKEN = "tok-observe";
const PROC = "proc-observe-1";

const received = [];
const serve = (port) =>
  Bun.serve({
    hostname: "127.0.0.1",
    port,
    async fetch(request) {
      received.push({
        path: new URL(request.url).pathname,
        auth: request.headers.get("authorization"),
        contentType: request.headers.get("content-type"),
        body: await request.json(),
      });
      return new Response("{}", { headers: { "content-type": "application/json" } });
    },
  });

let receiver = serve(0);
const receiverPort = receiver.port;
const endpoint = `http://127.0.0.1:${receiverPort}`;

const child = spawn("bun", ["run", kit], {
  stdio: ["ignore", "pipe", "pipe"],
  env: {
    ...process.env,
    KITBASH_TELEMETRY_ENDPOINT: endpoint,
    KITBASH_TELEMETRY_TOKEN: TOKEN,
    KITBASH_PROCESS: PROC,
    KITBASH_USER: "alice",
    KITBASH_PACKAGE: "/org/observe-count",
    KITBASH_OBSERVE_INTERVAL_MS: "300",
  },
});
let stdout = "";
let stderr = "";
child.stdout.on("data", (chunk) => (stdout += chunk));
child.stderr.on("data", (chunk) => (stderr += chunk));

const results = [];
const check = (name, ok, extra = "") => results.push(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` ${extra}` : ""}`);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

for (let i = 0; i < 100; i += 1) {
  try {
    await fetch(`${KIT_URL}/healthz`);
    break;
  } catch {
    await sleep(50);
  }
}

const post = (route, body) =>
  fetch(`${KIT_URL}${route}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });
const healthz = async () => (await fetch(`${KIT_URL}/healthz`)).json();
const attr = (key, value) => ({ key, value: typeof value === "boolean" ? { boolValue: value } : { stringValue: value } });
const span = (user, tool) => ({
  traceId: "11111111111111111111111111111111",
  spanId: "2222222222222222",
  name: tool,
  startTimeUnixNano: "1000000000000000000",
  endTimeUnixNano: "1000000100000000000",
  attributes: [attr("kitbash.user", user), attr("kitbash.tool", tool), attr("kitbash.producer", user)],
});

// Two members, three signals, delivered the way the fan out delivers them.
const traces = await post("/v1/traces", {
  resourceSpans: [{ resource: { attributes: [attr("service.name", "kitbash-mcp")] }, scopeSpans: [{ spans: [span("alice", "fs_list"), span("bob", "pkg_build")] }] }],
});
check("POST /v1/traces answers 200 with an empty response", traces.status === 200 && (await traces.clone().text()) === "{}" && traces.headers.get("content-type") === "application/json");
check(
  "POST /v1/logs answers 200",
  (await post("/v1/logs", { resourceLogs: [{ scopeLogs: [{ logRecords: [{ timeUnixNano: "1", body: { stringValue: "build ok" }, attributes: [attr("kitbash.user", "alice")] }] }] }] })).status === 200,
);
check(
  "POST /v1/metrics answers 200",
  (
    await post("/v1/metrics", {
      resourceMetrics: [
        {
          scopeMetrics: [
            {
              metrics: [
                {
                  name: "kitbash.calls",
                  sum: {
                    dataPoints: [
                      { asInt: "3", timeUnixNano: "1", attributes: [attr("kitbash.user", "alice")] },
                      { asInt: "4", timeUnixNano: "1", attributes: [attr("kitbash.user", "bob")] },
                    ],
                  },
                },
              ],
            },
          ],
        },
      ],
    })
  ).status === 200,
);

// The loop guard: this kit's own export coming back around is not counted.
const loop = await post("/v1/metrics", {
  resourceMetrics: [
    {
      scopeMetrics: [
        {
          metrics: [
            { name: "kitbash.observed.spans", sum: { dataPoints: [{ asInt: "2", timeUnixNano: "1", attributes: [attr("kitbash.user", "alice"), attr("kitbash.producer", PROC)] }] } },
          ],
        },
      ],
    },
  ],
});
check("a record produced by this Process is accepted", loop.status === 200);

let health = await healthz();
check("healthz counts 2 spans, 1 log and 2 metric points", health.totals.spans === 2 && health.totals.logs === 1 && health.totals.metrics === 2, JSON.stringify(health.totals));
check("healthz counts per member", health.users.alice.spans === 1 && health.users.bob.spans === 1, JSON.stringify(health.users));
check("the loop guard ignored exactly one point", health.ignored === 1, `ignored=${health.ignored}`);

// A body that puts a string where a repeated field belongs is not iterated
// one character at a time.
const notAList = await post("/v1/traces", { resourceSpans: "aaaaaaaaaaaaaaaaaaaa" });
health = await healthz();
check("a malformed repeated field counts nothing", notAList.status === 200 && health.totals.spans === 2, `${notAList.status} spans=${health.totals.spans}`);

// The body cap, at it and one byte past it.
const oversized = await post("/v1/traces", "a".repeat(4 * 1024 * 1024 + 1));
check("a body of 4 MiB + 1 is 413", oversized.status === 413, `status=${oversized.status}`);
const problem = await oversized.json();
check("the 413 is an RFC 9457 problem", problem.type.endsWith("/too-large") && problem.status === 413, JSON.stringify(problem).slice(0, 120));
const prefix = '{"resourceSpans":[],"padding":"';
const suffix = '"}';
const atCap = prefix + "b".repeat(4 * 1024 * 1024 - prefix.length - suffix.length) + suffix;
check("a body of exactly 4 MiB is accepted", atCap.length === 4 * 1024 * 1024 && (await post("/v1/traces", atCap)).status === 200);

// The periodic export.
for (let i = 0; i < 40 && received.length === 0; i += 1) await sleep(100);
const exported = received[received.length - 1];
check("the kit exported to /v1/metrics", exported?.path === "/v1/metrics", exported?.path);
check("the export carries the bearer token", exported?.auth === `Bearer ${TOKEN}`, exported?.auth ?? "");
check("the export is JSON", exported?.contentType === "application/json");
const metrics = exported?.body?.resourceMetrics?.[0]?.scopeMetrics?.[0]?.metrics ?? [];
const byName = Object.fromEntries(metrics.map((metric) => [metric.name, metric]));
check(
  "the three counters carry the counted values",
  byName["kitbash.observed.spans"]?.sum.dataPoints[0].asInt === "2" &&
    byName["kitbash.observed.logs"]?.sum.dataPoints[0].asInt === "1" &&
    byName["kitbash.observed.metrics"]?.sum.dataPoints[0].asInt === "2",
  JSON.stringify(metrics.map((metric) => [metric.name, metric.sum.dataPoints[0].asInt])),
);
check(
  "the counters are cumulative monotonic sums of unit 1",
  metrics.every((metric) => metric.unit === "1" && metric.sum.isMonotonic === true && metric.sum.aggregationTemporality === 2),
  JSON.stringify(metrics[0]?.sum ?? {}),
);
check("the data points carry no attributes of the kit's own", metrics.every((metric) => metric.sum.dataPoints.every((point) => point.attributes === undefined)));

// A producer that invents a member per record must not grow the map or the
// response without bound.
const longName = "l".repeat(4096);
for (let batch = 0; batch < 40; batch += 1) {
  const spans = [];
  for (let i = 0; i < 500; i += 1) spans.push(span(`user-${batch}-${i}`, "fs_list"));
  spans.push(span(longName, "fs_list"));
  await post("/v1/traces", { resourceSpans: [{ scopeSpans: [{ spans }] }] });
}
const healthzResponse = await fetch(`${KIT_URL}/healthz`);
const healthzText = await healthzResponse.text();
health = JSON.parse(healthzText);
check("every span is still counted in the totals", health.totals.spans === 2 + 40 * 501, `spans=${health.totals.spans}`);
check("the member map is capped", health.userCount <= 1024, `userCount=${health.userCount}`);
check("the overflow bucket took the rest", health.overflowUsers > 0 && health.users.other !== undefined, `overflowUsers=${health.overflowUsers}`);
check("healthz serialises at most 50 members", Object.keys(health.users).length <= 50, `${Object.keys(health.users).length} members`);
check("healthz says it truncated", health.usersTruncated === true);
check("healthz stays small", healthzText.length < 64 * 1024, `${healthzText.length} bytes`);
check("a member name is clipped to 1 KiB", Object.keys(health.users).every((name) => name.length <= 1024), `${Math.max(...Object.keys(health.users).map((name) => name.length))} characters`);

check("nothing is written to stdout", stdout === "", JSON.stringify(stdout.slice(0, 200)));
check("the kit logs to stderr", /listening on 0\.0\.0\.0:8080/.test(stderr), stderr.split("\n")[0]);

// Failure and recovery are each logged once.
receiver.stop(true);
await sleep(900);
check("a failed export is logged once", stderr.split("\n").filter((line) => line.includes("cannot write counters back")).length === 1);
receiver = serve(receiverPort);
await sleep(900);
check("recovery is logged once", stderr.split("\n").filter((line) => line.includes("works again")).length === 1);

// SIGTERM writes the final counts before the Process goes away.
const before = received.length;
child.kill("SIGTERM");
await sleep(1200);
check("the last counters are written on shutdown", received.length > before, `${received.length - before} exports after SIGTERM`);
check("the shutdown is logged", /SIGTERM, stopping/.test(stderr));

child.kill("SIGKILL");
receiver.stop(true);

console.log(results.join("\n"));
const failed = results.some((line) => line.startsWith("FAIL"));
console.log(failed ? "RESULT: FAIL" : "RESULT: PASS");
if (failed) console.log(`--- stderr ---\n${stderr.slice(-2000)}`);
process.exit(failed ? 1 : 0);
