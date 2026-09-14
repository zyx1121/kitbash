// The kit itself, driven over stdio the way kitbashd drives it: newline
// delimited JSON-RPC into index.js, which is the only place the two tools, the
// schemas they publish and the files they answer with are seen together.
//
//   cd deploy/org/import-cli && node --test "test/*.test.mjs"
//
// generate.test.mjs proves what the generator writes from a parsed help text.
// What this proves is the round trip: what a caller sends reaches the files,
// which is where the mounts of issue #122 could be lost between the two.

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { createInterface } from "node:readline";
import { after, before, test } from "node:test";

import { readYaml } from "./support.mjs";

const here = import.meta.dirname;
const kit = path.join(here, "..", "index.js");
const jqHelp = readFileSync(path.join(here, "fixtures", "jq.help.txt"), "utf8");

// The mounts every call below asks for: one folder of the caller's home read
// only and one read write, which is the pair the end to end job runs.
const mounts = [
  { source: "/home/ada/docs", target: "/files/docs", mode: "ro" },
  { source: "/home/ada/out", target: "/files/out", mode: "rw" },
];

// startKit runs index.js and answers with the two calls a client makes.
function startKit() {
  const child = spawn(process.execPath, [kit], { stdio: ["pipe", "pipe", "pipe"] });
  let stderr = "";
  child.stderr.on("data", (chunk) => {
    stderr += chunk.toString();
  });

  const pending = new Map();
  let nextId = 0;
  createInterface({ input: child.stdout }).on("line", (line) => {
    if (line.trim() === "") return;
    const message = JSON.parse(line);
    const settle = pending.get(message.id);
    if (settle === undefined) return;
    pending.delete(message.id);
    settle.resolve(message);
  });
  child.on("exit", (code) => {
    for (const settle of pending.values()) settle.reject(new Error(`the kit exited with ${code}: ${stderr}`));
    pending.clear();
  });

  const request = (method, params) =>
    new Promise((resolve, reject) => {
      const id = (nextId += 1);
      pending.set(id, { resolve, reject });
      child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
    });

  const call = async (name, args) => {
    const message = await request("tools/call", { name, arguments: args });
    assert.equal(message.error, undefined, `tools/call answered a JSON-RPC error: ${JSON.stringify(message.error)}`);
    return message.result;
  };

  return { request, call, stop: () => child.kill("SIGKILL") };
}

// The files one call answered with, by path.
const fileNamed = (result, name) => {
  assert.equal(result.isError, undefined, `expected files, got ${JSON.stringify(result).slice(0, 300)}`);
  const file = result.structuredContent.files.find((entry) => entry.path === name);
  assert.ok(file, `the kit answered no ${name}`);
  return file.content;
};

// The RFC 9457 document a refused call carries.
const problem = (result) => {
  assert.equal(result.isError, true, `expected a refusal, got ${JSON.stringify(result).slice(0, 300)}`);
  return JSON.parse(result.content[0].text);
};

let kitProcess;

before(() => {
  kitProcess = startKit();
});

after(() => {
  kitProcess.stop();
});

test("both tools publish the mounts input", async () => {
  const listed = await kitProcess.request("tools/list", {});
  const names = listed.result.tools.map((tool) => tool.name);
  assert.deepEqual(names, ["import", "refine"]);
  for (const tool of listed.result.tools) {
    const schema = tool.inputSchema.properties.mounts;
    assert.equal(schema.type, "array", `${tool.name} publishes no mounts input`);
    assert.equal(schema.maxItems, 4);
    assert.equal(schema.items.additionalProperties, false);
    assert.deepEqual(schema.items.required, ["source", "target"]);
    assert.deepEqual(schema.items.properties.mode.enum, ["ro", "rw"]);
  }
});

test("refine with mounts round trips into the manifest and tools.json", async () => {
  const result = await kitProcess.call("refine", {
    source: "cli:apk:jq",
    help: jqHelp,
    version: "jq-1.8.2",
    mounts,
  });

  // The unit carries them as they were given, because whether a member may
  // mount a source is kitbashd's answer at proc_run and not this kit's.
  const document = readYaml(fileNamed(result, "kitbash.yaml"));
  assert.deepEqual(document.deploy.units[0].mounts, mounts);

  // The adapter reads tools.json and nothing else, so the targets are in it.
  const tools = JSON.parse(fileNamed(result, "tools.json"));
  assert.deepEqual(tools.mounts, [
    { target: "/files/docs", mode: "ro" },
    { target: "/files/out", mode: "rw" },
  ]);

  // And the notes a reader of the folder finds say both forms.
  assert.match(fileNamed(result, "NOTES.md"), /## Files through mounts/);
  assert.match(fileNamed(result, "NOTES.md"), /- `\/files\/out` \(`rw`\)/);
  // adapter.js travels with them, which is what makes the folder a Package.
  assert.match(fileNamed(result, "adapter.js"), /tools\.json/);

  // The same mounts through import, which is the hook pkg_import calls.
  const drafted = await kitProcess.call("import", { source: "cli:apk:jq", mounts });
  assert.deepEqual(readYaml(fileNamed(drafted, "kitbash.yaml")).deploy.units[0].mounts, mounts);

  // And a call with no mounts writes a unit with none, which is every import
  // until one asks for a mount.
  const plain = await kitProcess.call("import", { source: "cli:apk:jq" });
  assert.equal(readYaml(fileNamed(plain, "kitbash.yaml")).deploy.units[0].mounts, undefined);
});

test("a mount this kit could not write is refused by the call that sent it", async () => {
  const relative = problem(await kitProcess.call("import", {
    source: "cli:apk:jq",
    mounts: [{ source: "docs", target: "/files/docs" }],
  }));
  assert.equal(relative.status, 400);
  assert.match(relative.detail, /every mount carries a source, as an absolute path/);

  const mode = problem(await kitProcess.call("import", {
    source: "cli:apk:jq",
    mounts: [{ source: "/home/ada/docs", target: "/files/docs", mode: "write" }],
  }));
  assert.equal(mode.status, 400);
  assert.match(mode.detail, /a mount mode is ro or rw/);

  const five = problem(await kitProcess.call("import", {
    source: "cli:apk:jq",
    mounts: [1, 2, 3, 4, 5].map((n) => ({ source: `/home/ada/${n}`, target: `/files/${n}` })),
  }));
  assert.equal(five.status, 400);
  assert.match(five.detail, /a unit may mount four folders/);
});
