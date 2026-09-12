// Drives import-oci over stdio against a fake skopeo, the way pkg_import drives
// it over MCP. No registry, no container runtime.
//
//   cd deploy/org/import-oci && bun install && bun run test/import.test.mjs
//
// Exit status is 0 when every check passes.

import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { parse as parseYaml } from "yaml";

const here = import.meta.dirname;
const kit = path.join(here, "..");
const workspace = mkdtempSync(path.join(tmpdir(), "import-oci-test-"));

// The one payload laid out byte by byte: a description of three byte
// characters, written by the fake in two pieces split in the middle of the
// first of them. Concatenating the Buffers rather than decoding them as a
// stream turns that character into U+FFFD.
const utf8Path = path.join(workspace, "utf8.json");
const utf8Description = `${"中".repeat(277)}...`;
const utf8Head =
  '{"Name":"docker.io/library/utf8",' +
  '"Digest":"sha256:6666666666666666666666666666666666666666666666666666666666666666",' +
  '"Created":"2026-01-01T00:00:00Z","Architecture":"amd64","Os":"linux",' +
  '"Labels":{"org.opencontainers.image.description":"';
writeFileSync(utf8Path, `${utf8Head}${"中".repeat(1000)}"}}`);
// One byte into the first character of the description.
const utf8Split = Buffer.byteLength(utf8Head) + 1;

const child = spawn("bun", ["run", path.join(kit, "index.js")], {
  stdio: ["pipe", "pipe", "pipe"],
  env: {
    ...process.env,
    IMPORT_OCI_SKOPEO: `sh ${path.join(here, "fake-skopeo")}`,
    IMPORT_OCI_FAKE_UTF8: utf8Path,
    IMPORT_OCI_FAKE_SPLIT: `${utf8Split}`,
  },
});

let stderr = "";
child.stderr.on("data", (chunk) => (stderr += chunk));

const pending = new Map();
let buffer = "";
let stdoutRaw = "";
child.stdout.on("data", (chunk) => {
  stdoutRaw += chunk;
  buffer += chunk;
  let cut;
  while ((cut = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, cut);
    buffer = buffer.slice(cut + 1);
    if (!line.trim()) continue;
    const message = JSON.parse(line);
    const waiter = pending.get(message.id);
    if (waiter) {
      pending.delete(message.id);
      waiter(message);
    }
  }
});

let id = 0;
const request = (method, params) =>
  new Promise((resolve) => {
    const n = (id += 1);
    pending.set(n, resolve);
    child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id: n, method, params })}\n`);
  });

const results = [];
const check = (name, ok, extra = "") => results.push(`${ok ? "PASS" : "FAIL"} ${name}${extra ? ` ${extra}` : ""}`);

await request("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "test", version: "0" } });
child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized", params: {} })}\n`);

const listed = await request("tools/list", {});
const advertised = listed.result.tools[0];
const manifest = parseYaml(readFileSync(path.join(kit, "kitbash.yaml"), "utf8"));
const declared = manifest.provides.tools.find((tool) => tool.name === "import");
check("tools/list advertises one tool named import", listed.result.tools.length === 1 && advertised.name === "import");
check("the advertised input schema equals the manifest's", JSON.stringify(advertised.inputSchema) === JSON.stringify(declared.input));
check("the advertised output schema equals the manifest's", JSON.stringify(advertised.outputSchema) === JSON.stringify(declared.output));

const call = async (source) => (await request("tools/call", { name: "import", arguments: { source } })).result;
const problemOf = (result) => JSON.parse(result.content[0].text);

// 1. The reference a real skopeo inspected on the host.
const alpine = "oci://docker.io/library/alpine@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40";
const good = await call(alpine);
check("a pinned image is not an error", good.isError !== true, JSON.stringify(good).slice(0, 160));
const files = good.structuredContent?.files ?? [];
check("exactly kitbash.yaml and README.md are returned", files.length === 2 && files[0].path === "kitbash.yaml" && files[1].path === "README.md", files.map((f) => f.path).join(","));
writeFileSync(path.join(workspace, "alpine.kitbash.yaml"), files[0].content);
const doc = parseYaml(files[0].content);
check("the name is the last path segment", doc.name === "alpine", doc.name);
check("the description falls back when Labels is null", doc.description === `OCI image ${alpine.slice("oci://".length)} imported from its registry.`, doc.description);
check("the tags are [oci]", JSON.stringify(doc.tags) === '["oci"]');
check("no provides block is generated", doc.provides === undefined);
check(
  "one container unit pinned to the digest, expose none",
  doc.deploy.units.length === 1 &&
    doc.deploy.units[0].type === "container" &&
    doc.deploy.units[0].image === alpine.slice("oci://".length) &&
    doc.deploy.units[0].expose === "none" &&
    doc.deploy.units[0].build === undefined,
  JSON.stringify(doc.deploy.units[0]),
);
check(
  "the README names the architecture and the created time",
  /Architecture: linux\/amd64/.test(files[1].content) && /Created: 2026-06-22T19:20:09\.106506185Z/.test(files[1].content),
);

// 2. Labels present, a nested repository, and control characters in both the
//    description and the fields the README carries.
const labelled = await call("oci://ghcr.io/acme/tools/pdf-render@sha256:1111111111111111111111111111111111111111111111111111111111111111");
const labelledDoc = parseYaml(labelled.structuredContent.files[0].content);
const labelledReadme = labelled.structuredContent.files[1].content;
check("a nested repository takes its last segment", labelledDoc.name === "pdf-render", labelledDoc.name);
check(
  "the description comes from the label, with control characters removed",
  labelledDoc.description === "Renders a PDF page to PNG on stdout. Reads the page number from argv.",
  JSON.stringify(labelledDoc.description),
);
check("no control character reaches the manifest", !/[\u0000-\u001f\u007f]/.test(labelledDoc.description));
check("no control character reaches the README", !/[\u0000-\u001f\u007f]/.test(labelledReadme.replace(/\n/g, "")), JSON.stringify(labelledReadme.match(/Created: .*/)?.[0]));

// 3. A description whose characters straddle a pipe read.
const utf8 = await call("oci://docker.io/library/utf8@sha256:6666666666666666666666666666666666666666666666666666666666666666");
const utf8Doc = parseYaml(utf8.structuredContent.files[0].content);
check("multi-byte characters survive the read", utf8Doc.description === utf8Description, JSON.stringify(utf8Doc.description.slice(0, 12)));
check("the long description is clipped to 280 characters", utf8Doc.description.length === 280, `${utf8Doc.description.length}`);

// 4. The four ways an import is refused.
const mismatch = problemOf(await call("oci://docker.io/library/alpine@sha256:2222222222222222222222222222222222222222222222222222222222222222"));
check("a digest mismatch is bad-request", mismatch.status === 400 && mismatch.type.endsWith("/bad-request"), mismatch.detail?.slice(0, 120));
const unknown = problemOf(await call("oci://docker.io/library/alpine@sha256:4444444444444444444444444444444444444444444444444444444444444444"));
check("manifest unknown is not-found", unknown.status === 404 && unknown.type.endsWith("/not-found"));
const unsupported = problemOf(await call("oci://docker.io/library/alpine@sha256:5555555555555555555555555555555555555555555555555555555555555555"));
check("manifest schema unsupported is bad-request naming the manifest digest", unsupported.status === 400 && /manifest digest/.test(unsupported.fix), unsupported.fix);
const broken = problemOf(await call("oci://registry.example.org/thing@sha256:9999999999999999999999999999999999999999999999999999999999999999"));
check("an unrecognized skopeo failure is internal", broken.status === 500);

// 5. What the source pattern accepts as a reference.
check("a tag is refused before skopeo runs", problemOf(await call("oci://docker.io/library/alpine:3.23")).status === 400);
check("a bare repository with no host is refused", problemOf(await call(`oci://alpine@sha256:${"1".repeat(64)}`)).status === 400);
check("a two segment path with no host is refused", problemOf(await call(`oci://library/alpine@sha256:${"1".repeat(64)}`)).status === 400);
const localhost = await call("oci://localhost:5000/thing@sha256:9999999999999999999999999999999999999999999999999999999999999999");
check("a host with a port reaches skopeo", problemOf(localhost).status === 500, problemOf(localhost).detail?.slice(0, 80));

// 6. The stdio contract.
const stdoutOk = stdoutRaw
  .split("\n")
  .filter((line) => line.trim())
  .every((line) => {
    try {
      return typeof JSON.parse(line) === "object";
    } catch {
      return false;
    }
  });
check("stdout carries JSON-RPC lines only", stdoutOk);
check("skopeo's output is kept on stderr", /skopeo exited 1/.test(stderr) && /manifest unknown/.test(stderr));

child.kill("SIGKILL");
console.log(results.join("\n"));
const failed = results.some((line) => line.startsWith("FAIL"));
console.log(failed ? "RESULT: FAIL" : "RESULT: PASS");
if (failed) console.log(`--- stderr ---\n${stderr.slice(-2000)}`);
process.exit(failed ? 1 : 0);
