// The second MCP client of the end to end job: the TypeScript SDK over
// streamable HTTP, where everything else in the job speaks the surface over
// stdio through kitbash's own Go client.
//
// It runs inside the echo Process's container, which is the only place the
// Process token exists: kitbash-mcp writes the token into the container's
// environment and keeps only its hash, so no step outside the container can
// read one. podman exec hands this script the same environment the container
// got, and it calls the receiver as the Process itself.
//
// What it proves: /mcp answers a client that is not kitbash's own, the surface
// it answers with is the owner's narrowed to the Package's permits block, a
// Package tool answers through it, a built in answers through it, and a tool
// the block does not name is refused rather than served.
//
// Environment, all of it kitbashd's: KITBASH_MCP_ENDPOINT, the receiver;
// KITBASH_TELEMETRY_TOKEN, the Process token this bears; KITBASH_USER, the
// owner whose home fs_list is asked about. E2E_ECHO_TEXT is the job's.

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const endpoint = required("KITBASH_MCP_ENDPOINT");
const token = required("KITBASH_TELEMETRY_TOKEN");
const owner = required("KITBASH_USER");
const text = process.env.E2E_ECHO_TEXT ?? "hello from the second client";

// The whole run, connection included. A client that hangs on the receiver is a
// failure with a message, not a job that waits for its own timeout.
const guard = setTimeout(() => {
  fail("the receiver did not answer within 60 seconds");
}, 60_000);
guard.unref?.();

function required(name) {
  const value = process.env[name];
  if (!value) fail(`${name} is not set in this container's environment`);
  return value;
}

function fail(message) {
  console.error(`mcp.mjs: ${message}`);
  process.exit(1);
}

function problemType(result) {
  const block = result?.content?.find((c) => c.type === "text");
  try {
    return JSON.parse(block?.text ?? "").type ?? "";
  } catch {
    return "";
  }
}

const client = new Client({ name: "kitbash-e2e-client", version: "1" });
const transport = new StreamableHTTPClientTransport(new URL(endpoint), {
  requestInit: { headers: { Authorization: `Bearer ${token}` } },
});
await client.connect(transport);

// 1. The surface a Process is given is its permits block and nothing else.
const listed = (await client.listTools()).tools.map((tool) => tool.name).sort();
const want = ["echo_echo", "fs_list"];
if (listed.join(",") !== want.join(",")) {
  fail(`tools/list answered ${listed.join(",") || "nothing"}, want ${want.join(",")}`);
}

// 2. A Package tool, answered by this same container through kitbash-mcp.
const echoed = await client.callTool({ name: "echo_echo", arguments: { text } });
if (echoed.isError) fail(`echo_echo failed: ${JSON.stringify(echoed.content)}`);
if (echoed.structuredContent?.text !== text) {
  fail(`echo_echo answered ${JSON.stringify(echoed.structuredContent)}, want text ${JSON.stringify(text)}`);
}

// 3. A built in, inside the path prefix the block permits.
const home = `/home/${owner}`;
const listing = await client.callTool({ name: "fs_list", arguments: { path: home } });
if (listing.isError) fail(`fs_list ${home} failed: ${JSON.stringify(listing.content)}`);
const folders = (listing.structuredContent?.folders ?? []).map((folder) => folder.path);
if (!folders.includes(`${home}/echo`)) {
  fail(`fs_list ${home} answered ${folders.join(",") || "no folders"}, want ${home}/echo`);
}

// 4. A tool the block does not name is refused, not served.
const refused = await client.callTool({ name: "users_me", arguments: {} });
if (!refused.isError || !problemType(refused).endsWith("/not-permitted")) {
  fail(`users_me answered ${JSON.stringify(refused)}, want a not-permitted problem`);
}

await client.close();
clearTimeout(guard);

// The line the Go driver reads back.
console.log(JSON.stringify({ ok: true, endpoint, tools: listed, echoed: echoed.structuredContent, folders }));
