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
import { copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { after, before, test } from "node:test";

import { draft } from "../generate.js";
import { validate } from "../test/support.mjs";

const here = import.meta.dirname;
const adapterSource = path.join(here, "..", "adapter.js");
const fakeCli = path.join(here, "fake-cli.mjs");
const node = process.execPath;
const MEBIBYTE = 1024 * 1024;

// The two output schemas the kit generates, read out of a drafted Package
// rather than written again here: what the adapter answers has to satisfy the
// schema its own generator declared, and a copy in this file would only prove
// it matches the copy, issue #114.
const generated = JSON.parse(draft({ source: "cli:apk:jq" }).find((file) => file.path === "tools.json").content);
const runOutputSchema = generated.tools.run.outputSchema;
const probeOutputSchema = generated.tools.probe.outputSchema;

// The folders the mounted adapter is told it has, made on this machine because
// the check resolves them: realpath is the whole point of it, so a mount target
// that is not a real directory would be testing a string comparison. They are
// realpath'd here as well, because /tmp is a link to /private/tmp on macOS and a
// target that was not resolved would never match a path that was.
let mountRoot = "";
let docsMount = "";
let outMount = "";
let awayFile = "";
// A folder whose name begins with the mounted one's, which is what the
// separator in the containment check is for.
let siblingFile = "";

// The tools.json the tests serve, in the contract the kit generates: a fixed
// argv prefix per tool, options mapped to flags, positionals in argv order and
// the two reserved inputs stdin and files. With mounts it carries the targets
// the adapter reads a path argument against; without them every path is refused.
//
// Every entry but `plain` carries the generated output schema, which is what
// makes the adapter answer structuredContent; `plain` is the hand edited
// tools.json that dropped it and answers in text alone.
const toolsDocument = ({ mounts = [] } = {}) => ({
  binary: fakeCli,
  ...(mounts.length > 0 ? { mounts } : {}),
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
    // A file argument on its own, which is the shape a path reaches an input
    // through, and a pair of them where the second is an output, which is the
    // shape a path reaches a mount through.
    read: {
      inputSchema: {
        type: "object",
        properties: { source: { type: "string", format: "kitbash-file" } },
      },
      argv: [node, fakeCli, "dump"],
      options: {},
      positionals: ["source"],
      stdin: null,
      outputs: [],
    },
    convert: {
      inputSchema: {
        type: "object",
        properties: {
          source: { type: "string", format: "kitbash-file" },
          dest: { type: "string", format: "kitbash-file" },
        },
        required: ["source", "dest"],
      },
      argv: [node, fakeCli, "upper"],
      options: {},
      positionals: ["source", "dest"],
      stdin: null,
      outputs: ["dest"],
    },
    // A variadic output, which is how a tool that writes several files is
    // generated: the format is on the items and each element takes either form.
    spill: {
      inputSchema: {
        type: "object",
        properties: { dests: { type: "array", items: { type: "string", format: "kitbash-file" } } },
      },
      argv: [node, fakeCli, "spill"],
      options: {},
      positionals: ["dests"],
      stdin: null,
      outputs: ["dests"],
    },
    // An output the command is told to write and does not, which is what a
    // failed run leaves behind.
    skip: {
      inputSchema: {
        type: "object",
        properties: { dest: { type: "string", format: "kitbash-file" } },
      },
      argv: [node, fakeCli, "dump"],
      options: {},
      positionals: ["dest"],
      stdin: null,
      outputs: ["dest"],
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
    // The tool whose entry claims its command prints JSON, which is the one
    // thing about standard output a help text could not tell the generator. It
    // spreads its argv so that one tool can run the fake CLI's JSON command and
    // its prose command, which is the pair the claim has to survive.
    parsed: {
      inputSchema: {
        type: "object",
        properties: { args: { type: "array", items: { type: "string" } }, stdin: { type: "string" } },
      },
      argv: [node, fakeCli],
      options: {},
      positionals: ["args"],
      spread: "args",
      stdin: "stdin",
      outputs: [],
      stdoutJson: true,
    },
    // A hand edited entry that declares no output schema, which is the only way
    // a generated Package answers in text alone.
    plain: {
      inputSchema: { type: "object", properties: {} },
      argv: [node, fakeCli, "dump"],
      options: {},
      positionals: [],
      stdin: null,
      outputs: [],
    },
    probe: { inputSchema: { type: "object", properties: {} }, probe: true },
  },
});

// Every tool of the fixture answers against the schema the kit generates,
// except the one that is there to show what an entry without it does.
const withSchemas = (document) => {
  for (const [name, spec] of Object.entries(document.tools)) {
    if (name === "plain") continue;
    spec.outputSchema = spec.probe ? probeOutputSchema : runOutputSchema;
  }
  return document;
};

// startAdapter lays out one generated Package in a temporary directory and
// runs it, and answers with the two calls a client makes and a way to stop it.
function startAdapter(env = {}, document = withSchemas(toolsDocument())) {
  const dir = mkdtempSync(path.join(tmpdir(), "kitbash-adapter-test-"));
  copyFileSync(adapterSource, path.join(dir, "adapter.js"));
  writeFileSync(path.join(dir, "tools.json"), JSON.stringify(document));
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

// The structured answer a tool with an output schema carries, issue #114: the
// same object as the text block, and one the schema the kit generated accepts.
// A result that lost its structuredContent, or one that drifted from the shape
// the manifest declares, fails here rather than on a host.
function structured(result, schema = runOutputSchema) {
  const body = payload(result);
  assert.ok(result.structuredContent, `the result carries no structuredContent: ${JSON.stringify(result).slice(0, 200)}`);
  assert.deepEqual(result.structuredContent, body, "structuredContent and the text block are not the same answer");
  assert.deepEqual(
    validate(result.structuredContent, schema),
    [],
    `the structured answer does not satisfy the generated schema: ${JSON.stringify(result.structuredContent).slice(0, 200)}`,
  );
  return result.structuredContent;
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
// The second adapter of this file: the same Package with two folders mounted,
// one read only and one read write, which is what a unit with mounts ships as.
let mounted;

before(() => {
  adapter = startAdapter();

  mountRoot = realpathSync(mkdtempSync(path.join(tmpdir(), "kitbash-adapter-mounts-")));
  docsMount = path.join(mountRoot, "docs");
  outMount = path.join(mountRoot, "out");
  mkdirSync(docsMount);
  mkdirSync(outMount);
  writeFileSync(path.join(docsMount, "doc.txt"), "hello kitbash");
  // A folder beside the mounted one whose name starts with the same letters,
  // so a containment check that forgot the separator would let it in.
  mkdirSync(`${docsMount}2`);
  siblingFile = path.join(`${docsMount}2`, "doc.txt");
  writeFileSync(siblingFile, "next door");
  // A file beside the mounts rather than inside either of them, and a link
  // inside one that points at it: the two ways of naming it from within a
  // mount, which realpath is what refuses.
  awayFile = path.join(mountRoot, "away.txt");
  writeFileSync(awayFile, "not in a mount");
  symlinkSync(awayFile, path.join(docsMount, "escape.txt"));

  mounted = startAdapter({}, withSchemas(toolsDocument({
    mounts: [
      { target: docsMount, mode: "ro" },
      { target: outMount, mode: "rw" },
    ],
  })));
});

after(() => {
  adapter.stop();
  mounted.stop();
  rmSync(mountRoot, { recursive: true, force: true });
});

test("initialize and tools/list answer with what tools.json declares", async () => {
  const initialised = await adapter.request("initialize", { protocolVersion: "2025-06-18" });
  assert.equal(initialised.result.protocolVersion, "2025-06-18");
  assert.deepEqual(initialised.result.capabilities, { tools: {} });

  const listed = await adapter.request("tools/list", {});
  const names = listed.result.tools.map((tool) => tool.name).sort();
  assert.deepEqual(names, [
    "absent", "concat", "convert", "dump", "fail", "flood", "fork", "hang", "link", "parsed", "plain", "probe", "read",
    "run", "skip", "spill", "upper",
  ]);
  const dump = listed.result.tools.find((tool) => tool.name === "dump");
  assert.equal(dump.description, "Print the arguments the adapter built.");
  assert.equal(dump.inputSchema.properties.filter.type, "string");
  // An entry with an output schema publishes it, so a client that reaches this
  // Package without the surface in front of it sees the same contract.
  assert.deepEqual(dump.outputSchema, runOutputSchema);
  assert.equal(listed.result.tools.find((tool) => tool.name === "plain").outputSchema, undefined);
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

// ---------------------------------------------------------------------------
// Files through a mount, issue #122. A kitbash-file argument takes a name of
// the files input, as above, or an absolute path under a folder the unit
// mounts, and the second form is what every test below drives.

test("a path under a read only mount reaches the command as it was written", async () => {
  const document = path.join(docsMount, "doc.txt");
  const result = await mounted.call("read", { source: document });
  const body = payload(result);
  assert.equal(body.exitCode, 0);
  // The path the command is given is the caller's, not the resolved one, and
  // no temporary copy of the file was made.
  assert.deepEqual(JSON.parse(body.stdout).args, [document]);
  assert.deepEqual(body.files, []);
});

test("an output path under a read write mount is left in place and reported without content", async () => {
  const source = path.join(docsMount, "doc.txt");
  const dest = path.join(outMount, "shouted.txt");
  const body = payload(await mounted.call("convert", { source, dest }));
  assert.equal(body.exitCode, 0);
  // The result names the file and where it is, and carries none of it: the
  // adapter never reads a file back out of a mount.
  assert.deepEqual(body.files, [{ name: "shouted.txt", path: dest }]);
  assert.equal(body.files[0].contentBase64, undefined);
  // And the file really is there, which is what the owner reads with fs_read.
  assert.equal(readFileSync(dest, "utf8"), "HELLO KITBASH");
});

test("a path outside every mount is an invalid-path problem naming the mounts", async () => {
  const document = problem(await mounted.call("read", { source: awayFile }));
  assert.equal(document.status, 400);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/invalid-path");
  assert.match(document.detail, /outside every folder this unit mounts/);
  assert.ok(document.fix.includes(docsMount) && document.fix.includes(outMount), document.fix);
});

test("a path that climbs out of its mount with .. is an invalid-path problem", async () => {
  const climbing = path.join(docsMount, "..", "away.txt");
  const document = problem(await mounted.call("read", { source: climbing }));
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/invalid-path");
  // The resolved path is what the refusal is about, which is the whole reason
  // the check runs realpath before it compares anything.
  assert.match(document.detail, new RegExp(`resolves to ${awayFile}`));
});

test("a symbolic link inside a mount that points outside it is an invalid-path problem", async () => {
  const link = path.join(docsMount, "escape.txt");
  const document = problem(await mounted.call("read", { source: link }));
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/invalid-path");
  assert.match(document.detail, new RegExp(`resolves to ${awayFile}`));
});

test("an output path under a read only mount is a not-permitted problem", async () => {
  const document = problem(await mounted.call("convert", {
    source: path.join(docsMount, "doc.txt"),
    dest: path.join(docsMount, "written.txt"),
  }));
  assert.equal(document.status, 403);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/not-permitted");
  assert.match(document.detail, /mounts read only/);
  assert.equal(existsSync(path.join(docsMount, "written.txt")), false, "the refused output was written anyway");
});

test("a path form when the unit declares no mounts is an invalid-path problem", async () => {
  // The adapter the rest of this file drives carries no mounts at all, which is
  // every generated Package until one declares some.
  const document = problem(await adapter.call("read", { source: "/etc/hosts" }));
  assert.equal(document.status, 400);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/invalid-path");
  assert.match(document.detail, /this unit declares no mounts/);
  assert.match(document.fix, /declare mounts on the unit/);
});

test("a folder whose name begins with a mount target is not under it", async () => {
  // The containment check compares on a separator and not on a string prefix,
  // so the sibling folder next to the mounted one stays outside it. Without the
  // separator this file would be read.
  for (const outside of [siblingFile, `${docsMount}X/doc.txt`, `${docsMount}2`]) {
    const document = problem(await mounted.call("read", { source: outside }));
    assert.equal(document.type, "https://kitbash.zyx.tw/errors/invalid-path", `${outside} was accepted`);
  }
});

test("a mount inside a mount is read by its own mode, whichever order they are declared", async () => {
  // A unit may mount a folder and a folder inside it. The inner target is the
  // one that governs a path inside it, so the read only folder stays read only
  // even though the read write one contains it.
  const nested = [
    { target: mountRoot, mode: "rw" },
    { target: docsMount, mode: "ro" },
  ];
  for (const order of [nested, [...nested].reverse()]) {
    const adapterWithNesting = startAdapter({}, toolsDocument({ mounts: order }));
    try {
      const refused = problem(await adapterWithNesting.call("convert", {
        source: path.join(docsMount, "doc.txt"),
        dest: path.join(docsMount, "nested.txt"),
      }));
      assert.equal(refused.type, "https://kitbash.zyx.tw/errors/not-permitted",
        `declared as ${JSON.stringify(order)} the inner mount lost its mode`);
      // And the outer folder is still writable, which is the other half of the
      // longest match being the one that decides.
      const body = payload(await adapterWithNesting.call("convert", {
        source: path.join(docsMount, "doc.txt"),
        dest: path.join(outMount, "nested.txt"),
      }));
      assert.equal(body.exitCode, 0);
    } finally {
      adapterWithNesting.stop();
    }
  }
});

test("an input path under a mount that names nothing is a not-found problem", async () => {
  // The same answer a name that was never sent in files gets, because it is the
  // same sentence about the same thing.
  const document = problem(await mounted.call("read", { source: path.join(docsMount, "absent.txt") }));
  assert.equal(document.status, 404);
  assert.equal(document.type, "https://kitbash.zyx.tw/errors/not-found");
  assert.match(document.detail, /is not there/);

  // A path whose folder does not exist at all is the path being wrong rather
  // than the file being missing.
  const noFolder = problem(await mounted.call("read", { source: path.join(docsMount, "nowhere", "absent.txt") }));
  assert.equal(noFolder.type, "https://kitbash.zyx.tw/errors/invalid-path");
});

test("an output path the command did not write is absent from the result", async () => {
  const dest = path.join(outMount, "never.txt");
  const body = payload(await mounted.call("skip", { dest }));
  assert.equal(body.exitCode, 0);
  // The tool ran and wrote nothing, so there is nothing to report: the entry is
  // absent rather than a name pointing at a file that is not there.
  assert.deepEqual(body.files, []);
  assert.equal(existsSync(dest), false);

  // A path that is a folder is not an output either, and says so.
  const folder = payload(await mounted.call("skip", { dest: outMount }));
  assert.deepEqual(folder.files, []);
  assert.deepEqual(folder.notes, [`${outMount} is not a regular file and was not reported as an output.`]);
});

test("a variadic output takes a name and a path in one call", async () => {
  const inMount = path.join(outMount, "spilled.txt");
  const body = payload(await mounted.call("spill", { dests: ["local.txt", inMount] }));
  assert.equal(body.exitCode, 0);
  // The path is reported where it was left and the name is read back as base64,
  // which is one property answering in both forms.
  const byName = Object.fromEntries(body.files.map((file) => [file.name, file]));
  assert.deepEqual(Object.keys(byName).sort(), ["local.txt", "spilled.txt"]);
  assert.equal(byName["spilled.txt"].path, inMount);
  assert.equal(byName["spilled.txt"].contentBase64, undefined);
  assert.equal(byName["local.txt"].path, undefined);
  assert.match(Buffer.from(byName["local.txt"].contentBase64, "base64").toString(), /^spilled into /);
  assert.equal(readFileSync(inMount, "utf8"), `spilled into ${inMount}\n`);
});

test("a variadic file positional takes a name and a path in one call", async () => {
  const document = path.join(docsMount, "doc.txt");
  const body = payload(await mounted.call("concat", {
    filter: ".",
    sources: ["first.json", document],
    files: [{ name: "first.json", contentBase64: Buffer.from('{"a":1}').toString("base64") }],
  }));
  const args = JSON.parse(body.stdout).args;
  assert.equal(args.length, 3);
  assert.equal(args[0], ".");
  // The name became a path in the call's own temporary directory, and the path
  // stayed the path.
  assert.match(args[1], /kitbash-cli-.*first\.json$/);
  assert.equal(args[2], document);
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
    // structured rather than payload: a killed command answers exitCode null,
    // which is the branch the generated schema has to allow.
    const body = structured(await shortLived.call("hang", {}));
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
  const body = structured(await adapter.call("probe", {}), probeOutputSchema);
  assert.match(body.help, /Usage: fake-cli/);
  assert.equal(body.version, "");
  assert.equal(typeof body.man, "string");
});

// ---------------------------------------------------------------------------
// The structured answer, issue #114. A Package whose manifest declares an
// output schema and whose adapter answers one text block is a promise the
// bridge cannot check, because the bridge validates structuredContent and
// passes prose through. Every branch of a call is checked against the schema
// the kit generated rather than against a copy written here.

test("a successful call answers structuredContent the generated schema accepts", async () => {
  const body = structured(await adapter.call("dump", { filter: "." }));
  assert.equal(body.exitCode, 0);
  assert.equal(body.truncated, false);
  assert.deepEqual(body.files, []);
  assert.deepEqual(JSON.parse(body.stdout).args, ["."]);
});

test("a non zero exit answers structuredContent rather than an error", async () => {
  const body = structured(await adapter.call("fail", {}));
  assert.equal(body.exitCode, 3);
  assert.equal(body.stderr.trim(), "fake-cli: the command refused.");
});

test("an output file read back inline is in the structured answer", async () => {
  const body = structured(await adapter.call("upper", {
    source: "notes.txt",
    output: "shouted.txt",
    files: [{ name: "notes.txt", contentBase64: Buffer.from("hello kitbash").toString("base64") }],
  }));
  assert.equal(body.files.length, 1);
  assert.equal(body.files[0].name, "shouted.txt");
  assert.equal(Buffer.from(body.files[0].contentBase64, "base64").toString(), "HELLO KITBASH");
});

test("an output left in a mount is in the structured answer as a path", async () => {
  const source = path.join(docsMount, "doc.txt");
  const dest = path.join(outMount, "structured.txt");
  const body = structured(await mounted.call("convert", { source, dest }));
  // The two forms of an output are one property of one schema, so the mount
  // form has to satisfy the same document the inline form does.
  assert.deepEqual(body.files, [{ name: "structured.txt", path: dest }]);
});

test("a note beside an output is in the structured answer", async () => {
  const body = structured(await adapter.call("link", { target: awayFile, output: "escape.txt" }));
  assert.deepEqual(body.files, []);
  assert.equal(body.notes.length, 1);
  assert.match(body.notes[0], /symbolic link/);
});

test("standard output is parsed into the answer only when the entry asks for it", async () => {
  // fake-cli dump prints JSON, and both tools run it: what differs is the
  // entry, which is where the claim that this command prints JSON lives.
  const parsed = structured(await adapter.call("parsed", { args: ["dump", "."], stdin: "{}" }));
  assert.deepEqual(parsed.stdoutJson.args, ["."]);
  assert.equal(parsed.stdoutJson.stdin, "{}");
  // The text is still the text: the parse is carried beside it, not instead.
  assert.deepEqual(JSON.parse(parsed.stdout), parsed.stdoutJson);

  // The same command through a tool whose entry does not claim JSON, which is
  // every tool the generator drafts.
  const untouched = structured(await adapter.call("dump", { filter: "." }));
  assert.equal(untouched.stdoutJson, undefined);

  // Output that is not JSON leaves the key out rather than reporting a failure
  // the command never had.
  const prose = structured(await adapter.call("parsed", { args: ["spill", "note.txt"] }));
  assert.match(prose.stdout, /wrote 1 file/);
  assert.equal(prose.stdoutJson, undefined);
});

test("a tool whose entry declares no output schema answers in text alone", async () => {
  const result = await adapter.call("plain", {});
  assert.equal(result.structuredContent, undefined);
  assert.equal(payload(result).exitCode, 0);
});

test("a refusal is problem details and carries no structured answer", async () => {
  const result = await adapter.call("upper", { source: "missing.txt", output: "out.txt" });
  assert.equal(problem(result).status, 404);
  assert.equal(result.structuredContent, undefined);
});
