// The adapter tests drive adapter.js the way the bridge does: a copy of it in a
// directory of its own with a tools.json beside it, spawned as a child, spoken
// to in newline delimited JSON-RPC over stdin and stdout.
//
// The copy is what keeps this file honest. A generated Package is adapter.js,
// tools.json and a package.json declaring type module, and nothing else, so a
// test that ran the file in place would be testing the kit's directory rather
// than the shape the Package ships in.
//
// The command every tool wraps is fake-cli.mjs next to this file, which prints
// its own argv and stdin as JSON, so what the adapter built is read back rather
// than inferred from a real tool's behaviour.

import { strict as assert } from "node:assert";
import { spawn } from "node:child_process";
import { copyFileSync, existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { after, before, test } from "node:test";

const here = import.meta.dirname;
const adapterSource = path.join(here, "..", "adapter.js");
const fakeCli = path.join(here, "fake-cli.mjs");
const node = process.execPath;
const MEBIBYTE = 1024 * 1024;

// The tools.json the tests serve, in the contract the kit generates: a fixed
// argv prefix per tool, options mapped to flags, positionals in argv order and
// the two reserved inputs stdin and files.
const toolsDocument = () => ({
  binary: fakeCli,
  tools: {
    dump: {
      description: "Print the arguments the adapter built.",
      inputSchema: {
        type: "object",
        properties: {
          compact: { type: "boolean" },
          quiet: { type: "boolean" },
          arg: { type: "string" },
          raw: { type: "array", items: { type: "string" } },
          tag: { type: "array", items: { type: "string" } },
          filter: { type: "string" },
          extra: { type: "string" },
          stdin: { type: "string" },
        },
      },
      argv: [node, fakeCli, "dump"],
      options: {
        compact: { flag: "--compact-output", takesValue: false, type: "boolean" },
        quiet: { flag: "--quiet", takesValue: false, type: "boolean" },
        arg: { flag: "--arg", takesValue: true, type: "string" },
        raw: { flag: "--raw", takesValue: true, type: "array", repeat: true },
        tag: { flag: "--tag", takesValue: true, type: "array", repeat: false },
      },
      positionals: ["filter", "extra"],
      stdin: "stdin",
      outputs: [],
    },
    upper: {
      inputSchema: {
        type: "object",
        properties: {
          source: { type: "string", format: "kitbash-file" },
          output: { type: "string" },
        },
        required: ["source", "output"],
      },
      argv: [node, fakeCli, "upper"],
      options: {},
      positionals: ["source", "output"],
      stdin: null,
      outputs: ["output"],
    },
    // The shape a variadic file positional is generated as, which is what a
    // synopsis such as jq's [file...] becomes: an array whose items carry the
    // format rather than the property itself.
    concat: {
      inputSchema: {
        type: "object",
        properties: {
          filter: { type: "string" },
          sources: { type: "array", items: { type: "string", format: "kitbash-file" } },
        },
      },
      argv: [node, fakeCli, "dump"],
      options: {},
      positionals: ["filter", "sources"],
      stdin: null,
      outputs: [],
    },
    flood: {
      inputSchema: { type: "object", properties: { mib: { type: "number" } } },
      argv: [node, fakeCli, "flood"],
      options: {},
      positionals: ["mib"],
      stdin: null,
      outputs: [],
    },
    hang: {
      inputSchema: { type: "object", properties: {} },
      argv: [node, fakeCli, "hang"],
      options: {},
      positionals: [],
      stdin: null,
      outputs: [],
    },
    fail: {
      inputSchema: { type: "object", properties: {} },
      argv: [node, fakeCli, "fail"],
      options: {},
      positionals: [],
      stdin: null,
      outputs: [],
    },
    fork: {
      inputSchema: { type: "object", properties: {} },
      argv: [node, fakeCli, "fork"],
      options: {},
      positionals: [],
      stdin: null,
      outputs: [],
    },
    link: {
      inputSchema: {
        type: "object",
        properties: { target: { type: "string" }, output: { type: "string" } },
      },
      argv: [node, fakeCli, "symlink"],
      options: {},
      positionals: ["target", "output"],
      stdin: null,
      outputs: ["output"],
    },
    absent: {
      inputSchema: { type: "object", properties: {} },
      argv: ["/nonexistent/kitbash-adapter-binary"],
      options: {},
      positionals: [],
      stdin: null,
      outputs: [],
    },
    run: {
      inputSchema: {
        type: "object",
        properties: { args: { type: "array", items: { type: "string" } }, stdin: { type: "string" } },
      },
      argv: [node, fakeCli, "dump"],
      options: {},
      positionals: ["args"],
      spread: "args",
      stdin: "stdin",
      outputs: [],
    },
    probe: { inputSchema: { type: "object", properties: {} }, probe: true },
  },
});

// startAdapter lays out one generated Package in a temporary directory and
// runs it, and answers with the two calls a client makes and a way to stop it.
function startAdapter(env = {}) {
  const dir = mkdtempSync(path.join(tmpdir(), "kitbash-adapter-test-"));
  copyFileSync(adapterSource, path.join(dir, "adapter.js"));
  writeFileSync(path.join(dir, "tools.json"), JSON.stringify(toolsDocument()));
  // The package.json is what makes node read adapter.js as an ES module, which
  // is the generated Package's own package.json in production.
  writeFileSync(
    path.join(dir, "package.json"),
    JSON.stringify({ name: "adapter-under-test", private: true, type: "module" }),
  );

  const child = spawn(node, ["adapter.js"], {
    cwd: dir,
    env: { ...process.env, ...env },
    stdio: ["pipe", "pipe", "pipe"],
  });
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
  // An adapter that died owes answers it will never send, so every waiting
  // call is failed with what it printed on the way out.
  child.on("exit", (code) => {
    for (const settle of pending.values()) settle.reject(new Error(`adapter exited with ${code}: ${stderr}`));
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
    assert.equal(message.error, undefined, `tools/call answered with a JSON-RPC error: ${JSON.stringify(message.error)}`);
    return message.result;
  };

  const stop = () => {
    child.kill("SIGKILL");
    rmSync(dir, { recursive: true, force: true });
  };

  return { request, call, stop };
}

// The JSON document a successful tool call carries in its one text block.
function payload(result) {
  assert.equal(result.isError, undefined, `expected a result, got ${JSON.stringify(result)}`);
  assert.equal(result.content.length, 1);
  assert.equal(result.content[0].type, "text");
  return JSON.parse(result.content[0].text);
}

// The RFC 9457 document an MCP error carries, checked for the shape the bridge
// passes through unchanged.
function problem(result) {
  assert.equal(result.isError, true, `expected an error, got ${JSON.stringify(result).slice(0, 200)}`);
  assert.equal(result.content.length, 1);
  const document = JSON.parse(result.content[0].text);
  assert.ok(document.type.startsWith("https://kitbash.zyx.tw/errors/"), document.type);
  assert.ok(document.title !== "" && document.detail !== "");
  return document;
}

let adapter;

before(() => {
  adapter = startAdapter();
});

after(() => {
  adapter.stop();
});

test("initialize and tools/list answer with what tools.json declares", async () => {
  const initialised = await adapter.request("initialize", { protocolVersion: "2025-06-18" });
  assert.equal(initialised.result.protocolVersion, "2025-06-18");
  assert.deepEqual(initialised.result.capabilities, { tools: {} });

  const listed = await adapter.request("tools/list", {});
  const names = listed.result.tools.map((tool) => tool.name).sort();
  assert.deepEqual(names, ["absent", "concat", "dump", "fail", "flood", "fork", "hang", "link", "probe", "run", "upper"]);
  const dump = listed.result.tools.find((tool) => tool.name === "dump");
  assert.equal(dump.description, "Print the arguments the adapter built.");
  assert.equal(dump.inputSchema.properties.filter.type, "string");
});

test("options become flags: a boolean, a value and a repeated array", async () => {
  const result = await adapter.call("dump", {
    compact: true,
    quiet: false,
    arg: "value",
    raw: ["one", "two"],
  });
  const body = payload(result);
  assert.equal(body.exitCode, 0);
  // A boolean that is false is the caller asking for the flag not to be
  // there, and repeat true puts --raw before every element.
  assert.deepEqual(JSON.parse(body.stdout).args, [
    "--compact-output",
    "--arg",
    "value",
    "--raw",
    "one",
    "--raw",
    "two",
  ]);
});

test("an array option that does not repeat writes its flag once with the elements after it", async () => {
  const result = await adapter.call("dump", { tag: ["red", "blue"], filter: "." });
  assert.deepEqual(JSON.parse(payload(result).stdout).args, ["--tag", "red", "blue", "."]);
});

test("positionals follow the options in the order tools.json records", async () => {
  const result = await adapter.call("dump", { extra: "second", filter: "first", compact: true, arg: 7 });
  assert.deepEqual(JSON.parse(payload(result).stdout).args, [
    "--compact-output",
    "--arg",
    "7",
    "first",
    "second",
  ]);
});

test("a spread positional becomes the whole argv tail", async () => {
  const result = await adapter.call("run", { args: ["--sort-keys", ".a", "-"] });
  assert.deepEqual(JSON.parse(payload(result).stdout).args, ["--sort-keys", ".a", "-"]);
});

test("the stdin property reaches the command's standard input", async () => {
  const result = await adapter.call("dump", { filter: ".", stdin: '{"a":1}' });
  assert.equal(JSON.parse(payload(result).stdout).stdin, '{"a":1}');
});

test("an input file is written for the command and an output file comes back as base64", async () => {
  const result = await adapter.call("upper", {
    source: "notes.txt",
    output: "shouted.txt",
    files: [{ name: "notes.txt", contentBase64: Buffer.from("hello kitbash").toString("base64") }],
  });
  const body = payload(result);
  assert.equal(body.exitCode, 0);
  assert.equal(body.files.length, 1);
  assert.equal(body.files[0].name, "shouted.txt");
  assert.equal(Buffer.from(body.files[0].contentBase64, "base64").toString(), "HELLO KITBASH");
});

test("a variadic file positional becomes one path per name the caller sent", async () => {
  const result = await adapter.call("concat", {
    filter: ".",
    sources: ["first.json", "second.json"],
    files: [
      { name: "first.json", contentBase64: Buffer.from('{"a":1}').toString("base64") },
      { name: "second.json", contentBase64: Buffer.from('{"b":2}').toString("base64") },
    ],
  });
  const args = JSON.parse(payload(result).stdout).args;
  assert.equal(args.length, 3);
  assert.equal(args[0], ".");
  assert.match(args[1], /first\.json$/);
  assert.match(args[2], /second\.json$/);
});

test("one name of a variadic file positional that nobody sent is a not-found problem", async () => {
  const document = problem(
    await adapter.call("concat", {
      filter: ".",
      sources: ["first.json", "absent.json"],
      files: [{ name: "first.json", contentBase64: Buffer.from('{"a":1}').toString("base64") }],
    }),
  );
  assert.equal(document.status, 404);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/not-found");
  assert.match(document.detail, /absent\.json/);
});

test("the temporary directory a call ran in is gone once the call has answered", async () => {
  const body = payload(await adapter.call("dump", { filter: "." }));
  const cwd = JSON.parse(body.stdout).cwd;
  assert.ok(cwd.includes("kitbash-cli-"), cwd);
  assert.equal(existsSync(cwd), false, `${cwd} outlived the call`);
});

test("a non zero exit code is a normal result rather than an MCP error", async () => {
  const body = payload(await adapter.call("fail", {}));
  assert.equal(body.exitCode, 3);
  assert.equal(body.stderr.trim(), "fake-cli: the command refused.");
  assert.equal(body.truncated, false);
});

test("output past the cap is truncated and flagged", async () => {
  const body = payload(await adapter.call("flood", { mib: 9 }));
  assert.equal(body.truncated, true);
  assert.equal(Buffer.byteLength(body.stdout), 8 * MEBIBYTE);
});

test("a command that runs past the timeout is killed", async () => {
  const shortLived = startAdapter({ IMPORT_CLI_TIMEOUT_MS: "400" });
  try {
    const body = payload(await shortLived.call("hang", {}));
    assert.equal(body.timedOut, true);
    assert.equal(body.exitCode, null);
    assert.equal(body.signal, "SIGKILL");
  } finally {
    shortLived.stop();
  }
});

test("a command that forks a grandchild holding stdout is killed with its group", async () => {
  const shortLived = startAdapter({ IMPORT_CLI_TIMEOUT_MS: "500" });
  try {
    // The command exits at once and its child keeps the pipe open, so a run
    // that waited for the pipe to close would never answer at all.
    const started = Date.now();
    const body = payload(await shortLived.call("fork", {}));
    const elapsed = Date.now() - started;
    assert.equal(body.timedOut, true);
    assert.equal(body.exitCode, null);
    assert.ok(elapsed < 1500, `the call took ${elapsed} ms, which is past the timeout plus a second`);
    // The queue is serialized, so an adapter still waiting on that call would
    // never answer this one either.
    const listed = await shortLived.request("tools/list", {});
    assert.ok(listed.result.tools.length > 0);
  } finally {
    shortLived.stop();
  }
});

test("files past the total input cap are refused before they are decoded", async () => {
  // Four files of three mebibytes each are inside the per file cap and over
  // the cap for one call, which is the shape that had the container killed.
  const megabytes = (count) => "A".repeat(count * 4 * (MEBIBYTE / 3));
  const document = problem(
    await adapter.call("upper", {
      source: "one.bin",
      output: "out.txt",
      files: [
        { name: "one.bin", contentBase64: megabytes(3) },
        { name: "two.bin", contentBase64: megabytes(3) },
        { name: "three.bin", contentBase64: megabytes(3) },
        { name: "four.bin", contentBase64: megabytes(3) },
      ],
    }),
  );
  assert.equal(document.status, 413);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/too-large");
  assert.match(document.detail, /over the 8 MiB limit for one call/);
});

test("stdin counts toward the total input cap", async () => {
  const document = problem(
    await adapter.call("dump", {
      filter: ".",
      stdin: "s".repeat(5 * MEBIBYTE),
      files: [{ name: "one.bin", contentBase64: "A".repeat(4 * 4 * (MEBIBYTE / 3)) }],
    }),
  );
  assert.equal(document.status, 413);
  assert.match(document.detail, /and stdin/);
});

test("an output the command left as a symbolic link is not read back", async () => {
  const body = payload(await adapter.call("link", { target: "/etc/hosts", output: "stolen.txt" }));
  assert.equal(body.exitCode, 0);
  assert.deepEqual(body.files, []);
  assert.deepEqual(body.notes, ["stolen.txt is a symbolic link and was not read back."]);
});

test("an unknown tool is a not-found problem", async () => {
  const document = problem(await adapter.call("transcode", {}));
  assert.equal(document.status, 404);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/not-found");
  assert.match(document.detail, /transcode/);
});

test("a positional naming a file nobody sent is a not-found problem", async () => {
  const document = problem(await adapter.call("upper", { source: "absent.txt", output: "out.txt", files: [] }));
  assert.equal(document.status, 404);
  assert.match(document.detail, /absent\.txt/);
});

test("a file past the cap is a too-large problem", async () => {
  const document = problem(
    await adapter.call("upper", {
      source: "big.bin",
      output: "out.txt",
      // Four base64 characters carry three bytes, so twelve mebibytes of
      // them are nine mebibytes of file and over the eight mebibyte cap.
      files: [{ name: "big.bin", contentBase64: "A".repeat(12 * MEBIBYTE) }],
    }),
  );
  assert.equal(document.status, 413);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/too-large");
});

test("input that is not a JSON object is a bad request", async () => {
  const document = problem(await adapter.call("dump", ["not", "an", "object"]));
  assert.equal(document.status, 400);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/bad-request");
});

test("a binary that is not in the image is an internal problem", async () => {
  const document = problem(await adapter.call("absent", {}));
  assert.equal(document.status, 500);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/internal");
});

test("probe reports the binary's help text and an empty version when it has none", async () => {
  const body = payload(await adapter.call("probe", {}));
  assert.match(body.help, /Usage: fake-cli/);
  assert.equal(body.version, "");
  assert.equal(typeof body.man, "string");
});
