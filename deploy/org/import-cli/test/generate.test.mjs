// Generator tests: the files a draft and a refined import answer with.
//
//   cd deploy/org/import-cli && npm test
//   node --test "deploy/org/import-cli/test/*.test.mjs"
//
// The manifest checks read the generated YAML back and run it against
// spec/manifest.schema.json, which is the file kitbashd validates against, so a
// manifest that passes here is one a fresh host would show. The reader and the
// validator are the two functions in support.mjs; both are a subset, and both
// are proved against every manifest already in this repository by the last test
// in this file.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";

import { parseHelp } from "../parse.js";
import { apkConstraint, draft, refined, parseSource, toYaml } from "../generate.js";
import { readYaml, validate } from "./support.mjs";

const here = import.meta.dirname;
const fixtures = path.join(here, "fixtures");
const repo = path.join(here, "..", "..", "..", "..");
const schema = JSON.parse(readFileSync(path.join(repo, "spec", "manifest.schema.json"), "utf8"));

const read = (name) => readFileSync(path.join(fixtures, name), "utf8");
const fileNamed = (files, name) => files.find((file) => file.path === name);
const manifestOf = (files) => readYaml(fileNamed(files, "kitbash.yaml").content);
const toolsOf = (files) => JSON.parse(fileNamed(files, "tools.json").content);

// The jq fixture, read the way refine reads what probe reported.
const jqHelp = read("jq.help.txt");
const jqParsed = parseHelp({ binary: "jq", help: jqHelp, version: read("jq.version.txt") });

test("a source is read into its package and its version", () => {
  assert.deepEqual(parseSource("cli:apk:jq@1.7.1"), { pkg: "jq", version: "1.7.1", name: "jq" });
  assert.deepEqual(parseSource("cli:apk:ffmpeg"), { pkg: "ffmpeg", version: "", name: "ffmpeg" });
  assert.deepEqual(parseSource("cli:apk:github-cli@2.83.0-r0"), {
    pkg: "github-cli",
    version: "2.83.0-r0",
    name: "github-cli",
  });
  for (const bad of ["jq", "cli:npm:jq", "cli:apk:", "cli:apk:JQ", "cli:apk:jq@", "cli:apk:../etc", 7, null]) {
    assert.throws(() => parseSource(bad), /is not cli:apk:/, `${JSON.stringify(bad)} should be refused`);
  }
  // A Package named after a built in tool family could never be run.
  assert.throws(() => parseSource("cli:apk:proc"), /built in tool family/);
});

test("the draft for cli:apk:jq@1.7.1 is a Package that builds", () => {
  const files = draft({ source: "cli:apk:jq@1.7.1" });
  assert.deepEqual(
    files.map((file) => file.path).sort(),
    ["Dockerfile", "NOTES.md", "kitbash.yaml", "package.json", "tools.json"],
  );
  for (const file of files) {
    assert.equal(typeof file.content, "string");
    assert.ok(file.content.length > 0, `${file.path} is empty`);
    assert.doesNotMatch(file.path, /^\/|(^|\/)\.\.?(\/|$)/, `${file.path} is not a relative path`);
  }

  const dockerfile = fileNamed(files, "Dockerfile").content;
  assert.match(dockerfile, /^FROM node:22-alpine$/m);
  // 1.7.1 names no apk release, so it is asked for with the fuzzy match.
  assert.match(dockerfile, /^RUN apk add --no-cache jq~=1\.7\.1$/m);
  assert.match(dockerfile, /^COPY package\.json adapter\.js tools\.json \.\/$/m);
  assert.match(dockerfile, /^CMD \["node", "adapter\.js"\]$/m);

  // The generated package.json is what makes adapter.js load as an ES module.
  assert.equal(JSON.parse(fileNamed(files, "package.json").content).type, "module");

  // A source without a version installs whatever the base image carries.
  const loose = draft({ source: "cli:apk:jq" });
  assert.match(fileNamed(loose, "Dockerfile").content, /^RUN apk add --no-cache jq$/m);

  // A version that names an apk release is pinned to exactly that release.
  const pinned = draft({ source: "cli:apk:jq@1.8.2-r0" });
  assert.match(fileNamed(pinned, "Dockerfile").content, /^RUN apk add --no-cache jq=1\.8\.2-r0$/m);
});

test("the three apk constraints a source version can become", () => {
  // apk names a release as <pkgver>-r<pkgrel>, and = wants the whole of it, so
  // an upstream version without the suffix has to be asked for with ~=.
  assert.equal(apkConstraint("jq", ""), "jq");
  assert.equal(apkConstraint("jq", "1.8.2"), "jq~=1.8.2");
  assert.equal(apkConstraint("jq", "1.8.2-r0"), "jq=1.8.2-r0");
  assert.equal(apkConstraint("github-cli", "2.83.0-r12"), "github-cli=2.83.0-r12");
  // A version with a suffix that is not a release number is still a prefix.
  assert.equal(apkConstraint("ffmpeg", "8.0.1_rc1"), "ffmpeg~=8.0.1_rc1");

  // And the notes of each say which of the three happened.
  assert.match(fileNamed(draft({ source: "cli:apk:jq" }), "NOTES.md").content, /The source named none/);
  assert.match(fileNamed(draft({ source: "cli:apk:jq@1.8.2" }), "NOTES.md").content, /Which release `jq~=1\.8\.2` resolves to/);
  assert.match(
    fileNamed(draft({ source: "cli:apk:jq@1.8.2-r0" }), "NOTES.md").content,
    /Whether the branch still carries `jq=1\.8\.2-r0`/,
  );
  // The refined Package is pinned the same way and says so as well.
  const parsed = parseHelp({ binary: "jq", help: jqHelp });
  const notes = fileNamed(refined({ source: "cli:apk:jq@1.8.2", parsed, help: jqHelp }), "NOTES.md").content;
  assert.match(notes, /Which release `jq~=1\.8\.2` resolves to/);
  assert.match(fileNamed(refined({ source: "cli:apk:jq@1.8.2", parsed, help: jqHelp }), "Dockerfile").content, /jq~=1\.8\.2/);
});

test("the draft manifest satisfies spec/manifest.schema.json", () => {
  const document = manifestOf(draft({ source: "cli:apk:jq@1.7.1" }));
  assert.deepEqual(validate(document, schema), []);

  // The keys the schema requires, checked by name as well, because a validator
  // that silently skipped a keyword would pass anything.
  assert.equal(document.name, "jq");
  assert.equal(typeof document.description, "string");
  assert.ok(document.description.length >= 10 && document.description.length <= 280, `description is ${document.description.length} characters`);
  assert.deepEqual(document.deploy.units, [{ type: "container", build: ".", expose: "mcp", limits: { memory: "512Mi" } }]);
  assert.deepEqual(
    document.provides.tools.map((tool) => tool.name),
    ["run", "probe"],
  );
  for (const tool of document.provides.tools) {
    assert.ok(tool.description.length <= 280, `${tool.name} has a description of ${tool.description.length} characters`);
    assert.equal(tool.input.type, "object");
    assert.equal(tool.output.type, "object");
  }
});

test("the draft's tools.json is the contract the adapter reads", () => {
  const tools = toolsOf(draft({ source: "cli:apk:jq@1.7.1" }));
  assert.equal(tools.binary, "jq");
  assert.deepEqual(Object.keys(tools.tools), ["run", "probe"]);
  assert.deepEqual(tools.tools.run, {
    argv: ["jq"],
    options: {},
    positionals: ["args"],
    spread: "args",
    stdin: "stdin",
    outputs: [],
    inputSchema: tools.tools.run.inputSchema,
  });
  assert.equal(tools.tools.probe.probe, true);
  // Every entry carries the schema it is called with, because the adapter
  // answers tools/list from this file and never reads the manifest.
  const declared = manifestOf(draft({ source: "cli:apk:jq@1.7.1" })).provides.tools;
  for (const tool of declared) {
    assert.deepEqual(tools.tools[tool.name].inputSchema, tool.input, `${tool.name} declares two different input schemas`);
  }
});

test("the refined jq Package has a filter positional and one tool per binary", () => {
  const files = refined({ source: "cli:apk:jq@1.8.2", parsed: jqParsed, help: jqHelp });
  const document = manifestOf(files);
  assert.deepEqual(validate(document, schema), []);

  const declared = document.provides.tools.map((tool) => tool.name);
  assert.deepEqual(declared, ["run", "probe", "jq"]);

  const tool = document.provides.tools.find((entry) => entry.name === "jq");
  assert.deepEqual(tool.input.required, ["filter"]);
  assert.equal(tool.input.properties.filter.type, "string");
  assert.equal(tool.input.additionalProperties, false);
  // The second positional is a file the caller supplies, so it is named from
  // the files array rather than by a path on the host.
  assert.equal(tool.input.properties.file.type, "array");
  assert.equal(tool.input.properties.file.items.format, "kitbash-file");
  // The reserved inputs are on every generated tool.
  assert.equal(tool.input.properties.stdin.type, "string");
  assert.equal(tool.input.properties.files.type, "array");
  // A flag became a property of the type its value name implies.
  assert.equal(tool.input.properties.compact_output.type, "boolean");
  assert.equal(tool.input.properties.indent.type, "number");
  assert.equal(tool.input.properties.arg.type, "array");

  const tools = toolsOf(files);
  assert.deepEqual(tools.tools.jq.argv, ["jq"]);
  assert.deepEqual(tools.tools.jq.positionals, ["filter", "file"]);
  assert.equal(tools.tools.jq.stdin, "stdin");
  assert.deepEqual(tools.tools.jq.outputs, []);
  assert.deepEqual(tools.tools.jq.options.compact_output, {
    flag: "--compact-output",
    takesValue: false,
    type: "boolean",
  });
  assert.deepEqual(tools.tools.jq.options.arg, { flag: "--arg", takesValue: true, type: "array", repeat: false });
  // run and probe survive the refinement, because a schema is a guess and the
  // escape hatch is not.
  assert.equal(tools.tools.probe.probe, true);
  assert.deepEqual(tools.tools.run.positionals, ["args"]);
});

test("the refined NOTES.md carries the help text and the doubts", () => {
  const notes = fileNamed(refined({ source: "cli:apk:jq@1.8.2", parsed: jqParsed, help: jqHelp }), "NOTES.md").content;
  assert.match(notes, /reading a help text in the `gnu` style/);
  assert.match(notes, /Flags read: 29\./);
  // The calling contract every Package repeats, so an agent that found the
  // folder can use it without reading this kit.
  assert.match(notes, /"exitCode"/);
  assert.match(notes, /contentBase64/);
  assert.match(notes, /kitbash-file/);
  assert.match(notes, /8388608 bytes/);
  // What the generator could not decide, named rather than implied.
  assert.match(notes, /## What the generator could not decide/);
  assert.match(notes, /`--arg` takes more than one value/);
  // The notes say what repeat means, because the adapter honours both settings.
  assert.match(notes, /`"repeat": false`, so the flag is written once and every element follows it as its own argv word/);
  assert.match(notes, /`file` was taken for a file the caller supplies/);
  // The raw help text is quoted back so the next reader can check the work.
  assert.match(notes, /jq - commandline JSON processor/);
});

test("a binary with subcommands gets one tool per subcommand", () => {
  const help = read("git.help.txt");
  const parsed = parseHelp({ binary: "git", help, version: read("git.version.txt") });
  const files = refined({ source: "cli:apk:git@2.52.0", parsed, help });
  const document = manifestOf(files);
  assert.deepEqual(validate(document, schema), []);

  const declared = document.provides.tools.map((tool) => tool.name);
  assert.equal(declared[0], "run");
  assert.equal(declared[1], "probe");
  assert.ok(declared.includes("git_commit"), "expected a git_commit tool");
  assert.ok(declared.includes("git_push"), "expected a git_push tool");
  assert.equal(declared.length, parsed.subcommands.length + 2);

  const tools = toolsOf(files);
  assert.deepEqual(tools.tools.git_commit.argv, ["git", "commit"]);
  assert.equal(tools.tools.git_commit.stdin, "stdin");
  // Every tool name is one the manifest schema accepts.
  for (const name of declared) assert.match(name, /^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/);
});

test("an output positional is read back into the result", () => {
  const help = read("ffmpeg.help.txt");
  const parsed = parseHelp({ binary: "ffmpeg", help, version: read("ffmpeg.version.txt") });
  const files = refined({ source: "cli:apk:ffmpeg@8.0.1", parsed, help });
  assert.deepEqual(validate(manifestOf(files), schema), []);

  const tools = toolsOf(files);
  assert.deepEqual(tools.tools.ffmpeg.positionals, ["infile", "outfile"]);
  assert.deepEqual(tools.tools.ffmpeg.outputs, ["outfile"]);
  assert.equal(tools.tools.ffmpeg.inputSchema.properties.infile.items.format, "kitbash-file");
  assert.match(fileNamed(files, "NOTES.md").content, /`outfile` was taken for a file the command writes/);
});

test("a help text in no style still yields a Package that runs", () => {
  const parsed = parseHelp({ binary: "mystery", help: "it does things\n" });
  const files = refined({ source: "cli:apk:mystery", parsed, help: "it does things\n" });
  const document = manifestOf(files);
  assert.deepEqual(validate(document, schema), []);
  // One tool for the binary, carrying nothing but the reserved inputs, plus the
  // two that are always there.
  assert.deepEqual(
    document.provides.tools.map((tool) => tool.name),
    ["run", "probe", "mystery"],
  );
  const tool = document.provides.tools.find((entry) => entry.name === "mystery");
  assert.deepEqual(Object.keys(tool.input.properties), ["stdin", "files"]);
  assert.match(fileNamed(files, "NOTES.md").content, /matched no style this parser knows/);
});

test("a description out of a help text is clipped and stripped", () => {
  // A subcommand description longer than the manifest allows, with a terminal
  // escape in it, which is a stranger's string on its way into a manifest.
  const parsed = {
    style: "cobra",
    version: null,
    options: [],
    positionals: [],
    subcommands: [{ name: "go", description: `\u001b[31m${"very long ".repeat(40)}`, options: [], positionals: [] }],
  };
  const document = manifestOf(refined({ source: "cli:apk:tool", parsed, help: "" }));
  assert.deepEqual(validate(document, schema), []);
  const tool = document.provides.tools.find((entry) => entry.name === "tool_go");
  assert.equal(tool.description.length, 280);
  assert.doesNotMatch(tool.description, /[\u0000-\u001f]/);
});

test("the manifest writer round trips through the reader", () => {
  const document = {
    name: "x",
    description: "a description with a colon: and a # hash in it",
    tags: ["a", "b"],
    nested: { empty: {}, list: [], number: 7, yes: true, quoted: "on" },
    items: [{ one: 1 }, { two: "2" }],
  };
  assert.deepEqual(readYaml(toYaml(document)), document);
});

test("the reader and the validator agree with every manifest in this repository", () => {
  // Both are subsets, so the thing that says they are big enough is that they
  // accept what kitbashd already accepts.
  const seeded = ["import-cli", "import-mcp", "import-oci", "observe-count", "evaluate-latency", "workflow", "handbook"];
  for (const folder of seeded) {
    const document = readYaml(readFileSync(path.join(repo, "deploy", "org", folder, "kitbash.yaml"), "utf8"));
    assert.deepEqual(validate(document, schema), [], `deploy/org/${folder}/kitbash.yaml`);
  }
  const echo = readYaml(readFileSync(path.join(repo, "e2e", "fixtures", "echo", "kitbash.yaml"), "utf8"));
  assert.deepEqual(validate(echo, schema), []);

  // And that they refuse what it refuses.
  assert.ok(validate({ name: "Bad Name", description: "long enough to pass" }, schema).length > 0);
  assert.ok(validate({ name: "ok", description: "short" }, schema).length > 0);
  assert.ok(validate({ name: "ok", description: "x".repeat(281) }, schema).length > 0);
});

test("this kit's own manifest declares the tools its server answers", () => {
  const document = readYaml(readFileSync(path.join(here, "..", "kitbash.yaml"), "utf8"));
  assert.deepEqual(validate(document, schema), []);
  assert.deepEqual(document.provides.kit, ["import"]);
  assert.deepEqual(
    document.provides.tools.map((tool) => tool.name),
    ["import", "refine"],
  );
  // The pattern is the route pkg_import binds on, so it is the one thing in the
  // manifest that has to be the same string the generator refuses sources with.
  const pattern = document.provides.tools[0].input.properties.source.pattern;
  assert.equal(pattern, "^cli:apk:[a-z0-9][a-z0-9_.+-]*(@[A-Za-z0-9._+-]+)?$");
  assert.match("cli:apk:jq@1.7.1", new RegExp(pattern));
  assert.doesNotMatch("npm:jq@1.7.1", new RegExp(pattern));
  assert.equal(document.provides.tools[1].input.properties.source.pattern, pattern);

  // The server declares the same two tools with the same schemas.
  const source = readFileSync(path.join(here, "..", "index.js"), "utf8");
  assert.match(source, /name: "import"/);
  assert.match(source, /name: "refine"/);
  assert.ok(source.includes(pattern), "index.js and kitbash.yaml carry different source patterns");
});
