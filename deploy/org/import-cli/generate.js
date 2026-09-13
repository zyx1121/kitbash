// generate.js: the files that make an Alpine package a kitbash Package.
//
// Two entry points. draft builds the Package that can be built and run knowing
// nothing but the package name and the version, so it needs no network: two
// tools, run and probe, and probe is what the agent calls to read the binary's
// own help. refined builds the Package again from what probe reported and what
// parse.js made of it: one tool per subcommand, or one for the binary when it
// has none, each carrying a JSON Schema derived from the flags.
//
// Both write the same five files. The sixth, adapter.js, is the kit's own file
// and is added by index.js, so this module stays pure and has no file system of
// its own.
//
// ---------------------------------------------------------------------------
// tools.json is the contract between this generator and the adapter. The
// adapter reads nothing else: it answers tools/list from the inputSchema of
// every entry and builds argv from the rest. The shape is fixed.
//
//   {
//     "binary": "jq",
//     "tools": {
//       "<toolName>": {
//         "argv": ["jq"],                       // fixed prefix, binary then subcommand words
//         "options": {                          // input property -> flag
//           "<prop>": {"flag": "--compact-output", "takesValue": false, "type": "boolean"},
//           "<prop2>": {"flag": "--arg", "takesValue": true, "type": "string", "repeat": false}
//         },
//         "positionals": ["filter", "input"],  // input property names, in argv order; a property of type "file" is written from files[] to a tmp path and the path is passed
//         "stdin": "stdin",                     // input property whose string goes to stdin, or null
//         "outputs": ["output"]                 // positional property names naming files the adapter reads back after the run (base64 in result.files)
//       },
//       "run":   {"argv": ["jq"], "options": {}, "positionals": ["args"], "spread": "args", "stdin": "stdin", "outputs": []},
//       "probe": {"probe": true}
//     }
//   }
//
// Input schema conventions the adapter relies on: `files` is
// `[{name, contentBase64}]`; a positional whose schema has
// `"format": "kitbash-file"` names a file from `files` by its `name`. Result
// shape from the adapter: `{exitCode, stdout, stderr, files: [{name,
// contentBase64}]}` with the 8 MiB caps.
//
// Two details this generator adds on top of that shape, neither of which
// changes a key. Every tool entry also carries `inputSchema`, the same schema
// the manifest declares, because the adapter answers tools/list from tools.json
// alone and never parses YAML. And an option of type `array` passes its
// elements as consecutive argv words after one occurrence of the flag, which is
// what `"repeat": false` means; no option this generator writes sets it to
// true.
// ---------------------------------------------------------------------------

import { propertyName } from "./parse.js";

// A description, of a folder or of a tool, is at most 280 characters.
const MAX_DESCRIPTION = 280;
const MIN_DESCRIPTION = 10;
// stdout, stderr and every file the adapter carries are capped at this.
const MAX_PAYLOAD = 8 * 1024 * 1024;
// A Package whose name equals a built in tool family cannot be run, PLAN.md 2.3.
const RESERVED_NAMES = new Set(["fs", "pkg", "proc", "tel", "users", "approvals"]);
// The source syntax this kit routes on. The same pattern is in kitbash.yaml.
const SOURCE_PATTERN = /^cli:apk:[a-z0-9][a-z0-9_.+-]*(@[A-Za-z0-9._+-]+)?$/;
// A positional with one of these names is a file the caller supplies.
const INPUT_FILE_NAMES = new Set(["file", "files", "infile", "input", "inputs", "in", "path", "paths", "source", "src", "filename"]);
// A positional with one of these names is a file the tool writes and the
// adapter reads back into result.files.
const OUTPUT_FILE_NAMES = new Set(["outfile", "output", "outputs", "out", "dest", "destination", "target"]);
// Node 22 is the base every kit and every Package this kit writes is built on.
const NODE_BASE = "node:22-alpine";

// ---------------------------------------------------------------- text helpers

// Everything that reaches a manifest came out of a binary's help text, which is
// a stranger's string. Control characters are removed rather than escaped,
// because no description has a use for them and a terminal sequence in one is a
// trap for whoever reads it next.
export function clip(text, max = MAX_DESCRIPTION) {
  if (typeof text !== "string") return "";
  const flat = text
    .replace(/[\x00-\x1f\x7f]/g, " ")
    .replace(/\s+/g, " ")
    .trim();
  return flat.length <= max ? flat : `${flat.slice(0, max - 3).trimEnd()}...`;
}

// A description shorter than ten characters fails the manifest schema, so a
// short one is padded with the sentence that says where it came from.
function describe(text, fallback) {
  const clipped = clip(text);
  if (clipped.length >= MIN_DESCRIPTION) return clipped;
  return clip(clipped === "" ? fallback : `${clipped}. ${fallback}`);
}

// Package names in a manifest are ^[a-z0-9]+(-[a-z0-9]+)*$, at most 64 characters.
export function packageNameFrom(raw) {
  const name = String(raw)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64)
    .replace(/-+$/, "");
  return /^[a-z0-9]+(-[a-z0-9]+)*$/.test(name) ? name : "";
}

// A tool name is <pkg>_<subcommand>, kebab turned to snake because a tool name
// is read as one identifier and the surface splits it on the first underscore.
function toolNameFrom(pkg, subcommand) {
  const head = pkg.replace(/-/g, "_");
  const tail = subcommand ? `_${String(subcommand).toLowerCase().replace(/[^a-z0-9]+/g, "_")}` : "";
  return `${head}${tail}`.replace(/_+/g, "_").replace(/^_+|_+$/g, "").slice(0, 63);
}

// ---------------------------------------------------------------- YAML writing

const YAML_BOOLEANS = new Set(["y", "n", "yes", "no", "on", "off", "true", "false", "null", "~"]);

// A scalar is written plain when nothing in it can start a YAML construct, and
// double quoted otherwise. JSON quoting is valid YAML quoting, so the quoted
// branch is JSON.stringify.
function yamlScalar(value) {
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean" || typeof value === "number") return JSON.stringify(value);
  const text = String(value);
  if (text === "") return '""';
  if (YAML_BOOLEANS.has(text.toLowerCase())) return JSON.stringify(text);
  if (/^[-?:,[\]{}#&*!|>'"%@`]/.test(text)) return JSON.stringify(text);
  if (/: |\s#|[\n\t]/.test(text)) return JSON.stringify(text);
  if (/[:\s]$/.test(text)) return JSON.stringify(text);
  if (/^[0-9]/.test(text) && !/[^0-9.eE+-]/.test(text)) return JSON.stringify(text);
  return text;
}

function yamlKey(key) {
  const text = String(key);
  if (/^[A-Za-z_$][A-Za-z0-9_.$-]*$/.test(text) && !YAML_BOOLEANS.has(text.toLowerCase())) return text;
  return JSON.stringify(text);
}

const isPlainObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);

// A deterministic YAML writer for the small document shapes a manifest uses:
// maps, arrays and scalars, in insertion order, with no anchors and no folding.
export function toYaml(value, indent = 0) {
  const pad = " ".repeat(indent);
  if (Array.isArray(value)) {
    if (value.length === 0) return `${pad}[]\n`;
    return value
      .map((item) => {
        if (isPlainObject(item) || Array.isArray(item)) {
          const body = toYaml(item, indent + 2);
          return `${pad}-${body.slice(indent + 1)}`;
        }
        return `${pad}- ${yamlScalar(item)}\n`;
      })
      .join("");
  }
  if (isPlainObject(value)) {
    const keys = Object.keys(value);
    if (keys.length === 0) return `${pad}{}\n`;
    return keys
      .map((key) => {
        const item = value[key];
        if (isPlainObject(item) || Array.isArray(item)) {
          const empty = Array.isArray(item) ? item.length === 0 : Object.keys(item).length === 0;
          if (empty) return `${pad}${yamlKey(key)}: ${Array.isArray(item) ? "[]" : "{}"}\n`;
          return `${pad}${yamlKey(key)}:\n${toYaml(item, indent + 2)}`;
        }
        return `${pad}${yamlKey(key)}: ${yamlScalar(item)}\n`;
      })
      .join("");
  }
  return `${pad}${yamlScalar(value)}\n`;
}

// ---------------------------------------------------------------- the source

/**
 * Read cli:apk:<pkg>[@<version>] into its two parts. Throws a plain Error whose
 * message is the detail of the problem the server reports.
 */
export function parseSource(source) {
  if (typeof source !== "string" || !SOURCE_PATTERN.test(source)) {
    throw new Error(`${typeof source === "string" ? source : String(source)} is not cli:apk:<pkg> or cli:apk:<pkg>@<version>.`);
  }
  const rest = source.slice("cli:apk:".length);
  const at = rest.indexOf("@");
  const pkg = at === -1 ? rest : rest.slice(0, at);
  const version = at === -1 ? "" : rest.slice(at + 1);
  const name = packageNameFrom(pkg);
  if (name === "") throw new Error(`No manifest name can be derived from the apk package ${pkg}.`);
  if (RESERVED_NAMES.has(name)) {
    throw new Error(`${pkg} would become the Package name ${name}, which is a built in tool family.`);
  }
  return { pkg, version, name };
}

// ---------------------------------------------------------------- schema parts

const filesProperty = () => ({
  type: "array",
  description: "Files to place beside the command before it runs. A positional whose format is kitbash-file names one of these by its name.",
  maxItems: 32,
  items: {
    type: "object",
    additionalProperties: false,
    required: ["name", "contentBase64"],
    properties: {
      name: { type: "string", description: "The name a kitbash-file positional refers to. No slash, no dot component." },
      contentBase64: { type: "string", contentEncoding: "base64", description: `The file's bytes, at most ${MAX_PAYLOAD} bytes decoded.` },
    },
  },
});

const stdinProperty = (binary) => ({
  type: "string",
  description: `Text written to ${binary} on standard input and then closed. At most ${MAX_PAYLOAD} bytes.`,
  maxLength: MAX_PAYLOAD,
});

const runOutput = (binary) => ({
  type: "object",
  required: ["exitCode", "stdout", "stderr"],
  properties: {
    exitCode: { type: "integer", description: `The status ${binary} exited with. A non zero status is reported, not raised.` },
    stdout: { type: "string", description: `What ${binary} wrote to standard output, truncated at ${MAX_PAYLOAD} bytes.` },
    stderr: { type: "string", description: `What ${binary} wrote to standard error, truncated at ${MAX_PAYLOAD} bytes.` },
    files: {
      type: "array",
      description: "The files named by this tool's outputs, read back after the run.",
      items: {
        type: "object",
        required: ["name", "contentBase64"],
        properties: { name: { type: "string" }, contentBase64: { type: "string", contentEncoding: "base64" } },
      },
    },
  },
});

const probeOutput = (binary) => ({
  type: "object",
  required: ["help", "version", "man"],
  properties: {
    help: { type: "string", description: `What ${binary} --help printed, or the error it printed instead.` },
    version: { type: "string", description: `What ${binary} --version printed, or an empty string.` },
    man: { type: "string", description: `What man ${binary} printed, or an empty string when the image carries no man page.` },
  },
});

// ---------------------------------------------------------------- tool building

// The schema property one parsed option becomes, and the tools.json entry that
// turns that property back into argv.
function optionProperty(option, taken) {
  let name = propertyName(option.flag);
  if (name === "") return null;
  if (taken.has(name)) {
    let attempt = 2;
    while (taken.has(`${name}_${attempt}`)) attempt += 1;
    name = `${name}_${attempt}`;
  }
  taken.add(name);

  const description = describe(option.description, `The ${option.flag} flag of the command.`);
  const schema =
    option.type === "array"
      ? { type: "array", items: { type: "string" }, description }
      : { type: option.type, description };
  if (option.type === "array" && option.valueName) schema.description = describe(`${option.description} Values: ${option.valueName}.`, description);

  const wiring = { flag: option.flag, takesValue: option.takesValue, type: option.type };
  if (option.takesValue) wiring.repeat = false;
  return { name, schema, wiring };
}

// The schema property one positional becomes. A positional named like an input
// file carries format kitbash-file, which tells the adapter to write the named
// entry of files to a temporary path and pass that path instead.
function positionalProperty(positional, taken) {
  let name = propertyName(positional.name);
  if (name === "") return null;
  if (taken.has(name)) {
    let attempt = 2;
    while (taken.has(`${name}_${attempt}`)) attempt += 1;
    name = `${name}_${attempt}`;
  }
  taken.add(name);

  const isInputFile = INPUT_FILE_NAMES.has(positional.name);
  const isOutputFile = OUTPUT_FILE_NAMES.has(positional.name);
  const description = describe(
    positional.description,
    isInputFile
      ? `The ${positional.name} argument. Name an entry of files here rather than a path on the host.`
      : `The ${positional.name} argument of the command.`,
  );
  const item = { type: "string", ...(isInputFile ? { format: "kitbash-file" } : {}) };
  const schema = positional.variadic
    ? { type: "array", items: item, description }
    : { ...item, description };
  return { name, schema, required: positional.required && !positional.variadic, isOutputFile };
}

// One tool: the schema the manifest declares and the tools.json entry beside it.
function buildTool({ toolName, description, binary, argv, options, positionals }) {
  const taken = new Set(["stdin", "files"]);
  const properties = {};
  const wiring = {};
  const order = [];
  const outputs = [];
  const required = [];

  for (const positional of positionals) {
    const built = positionalProperty(positional, taken);
    if (!built) continue;
    properties[built.name] = built.schema;
    order.push(built.name);
    if (built.required) required.push(built.name);
    if (built.isOutputFile) outputs.push(built.name);
  }
  for (const option of options) {
    const built = optionProperty(option, taken);
    if (!built) continue;
    properties[built.name] = built.schema;
    wiring[built.name] = built.wiring;
  }

  properties.stdin = stdinProperty(binary);
  properties.files = filesProperty();

  const input = {
    type: "object",
    additionalProperties: false,
    ...(required.length > 0 ? { required } : {}),
    properties,
  };

  return {
    manifestTool: { name: toolName, description: clip(description), input, output: runOutput(binary) },
    entry: { argv, options: wiring, positionals: order, stdin: "stdin", outputs, inputSchema: input },
  };
}

// The two tools every Package this kit writes carries, drafted or refined. run
// is the escape hatch that works whatever the help said, and probe is what the
// agent calls between the two builds.
function baseTools(binary) {
  const runInput = {
    type: "object",
    additionalProperties: false,
    required: ["args"],
    properties: {
      args: {
        type: "array",
        description: `The arguments to pass to ${binary}, one array element per argv word. No shell is involved, so quoting and globbing do not apply.`,
        items: { type: "string" },
        maxItems: 256,
      },
      stdin: stdinProperty(binary),
      files: filesProperty(),
    },
  };
  const probeInput = { type: "object", additionalProperties: false, properties: {} };

  return {
    manifestTools: [
      {
        name: "run",
        description: clip(`Run ${binary} with the arguments given, verbatim. Use it for anything the generated tools do not cover, and to check what a flag does before a schema is written for it.`),
        input: runInput,
        output: runOutput(binary),
      },
      {
        name: "probe",
        description: clip(`Report what ${binary} says about itself: its --help output, its --version output and its man page when the image carries one. Feed the answer back to the import-cli kit's refine tool.`),
        input: probeInput,
        output: probeOutput(binary),
      },
    ],
    entries: {
      run: { argv: [binary], options: {}, positionals: ["args"], spread: "args", stdin: "stdin", outputs: [], inputSchema: runInput },
      probe: { probe: true, inputSchema: probeInput },
    },
  };
}

// ---------------------------------------------------------------- the files

function dockerfile({ pkg, version, source }) {
  const pinned = version === "" ? pkg : `${pkg}=${version}`;
  return [
    `# Generated by import-cli from ${source}.`,
    "#",
    "# A stdio MCP server whose one job is to run one binary. kitbash runs it as",
    "# PID 1 with stdin held open and execs one more instance per MCP session.",
    `FROM ${NODE_BASE}`,
    "",
    "WORKDIR /app",
    "",
    "# The binary itself, from the Alpine repositories the base image points at.",
    `# A version that apk cannot resolve fails the build, which is the right time`,
    "# to find out: apk names a release as <pkgver>-r<pkgrel>.",
    `RUN apk add --no-cache ${pinned}`,
    "",
    "# No dependencies: the adapter is one file of plain Node, and tools.json is",
    "# the whole contract between it and the tools this Package declares.",
    "COPY package.json adapter.js tools.json ./",
    "",
    'CMD ["node", "adapter.js"]',
    "",
  ].join("\n");
}

function packageJson(name) {
  return `${JSON.stringify(
    {
      name,
      version: "0.1.0",
      private: true,
      type: "module",
      description: `kitbash Package wrapping the ${name} command line tool.`,
      main: "adapter.js",
    },
    null,
    2,
  )}\n`;
}

function manifest({ name, description, tools, source }) {
  const document = {
    name,
    description: clip(description),
    tags: ["cli", "imported", "apk"],
    provides: { tools },
    deploy: {
      units: [
        {
          type: "container",
          build: ".",
          expose: "mcp",
          // The Package runs a binary a caller chose with arguments a caller
          // chose, so it gets the bound the import kits get rather than the
          // machine's.
          limits: { memory: "512Mi" },
        },
      ],
    },
  };
  return `# Generated by import-cli from ${source}. Edit it: the generator read a\n# help text, and a help text is not a specification.\n${toYaml(document)}`;
}

function toolsJson({ binary, entries }) {
  return `${JSON.stringify({ binary, tools: entries }, null, 2)}\n`;
}

// The calling contract, repeated in every Package so that an agent that found
// the folder knows how to use the tools without reading the kit.
function callingContract(binary, toolNames) {
  return [
    "## Calling these tools",
    "",
    `Every tool runs \`${binary}\` in this Package's container with no shell, so`,
    "quoting, globbing and redirection do not apply. The result is always the same",
    "object:",
    "",
    "```json",
    '{ "exitCode": 0, "stdout": "", "stderr": "", "files": [{ "name": "out.png", "contentBase64": "..." }] }',
    "```",
    "",
    `A non zero \`exitCode\` is reported rather than raised, and \`stdout\`, \`stderr\``,
    `and every file are capped at ${MAX_PAYLOAD} bytes each way.`,
    "",
    "To pass a file in, put it in `files` as `{name, contentBase64}` and name it",
    "from the positional whose schema carries `\"format\": \"kitbash-file\"`. The",
    "adapter writes it to a temporary path and passes that path. A positional the",
    "tool writes to is read back and returned in `files` under the same name.",
    "",
    "`stdin` is written to the command and then closed.",
    "",
    `Tools: ${toolNames.map((tool) => `\`${tool}\``).join(", ")}.`,
    "",
  ].join("\n");
}

// ---------------------------------------------------------------- draft

/**
 * The files for a Package that can be built and run knowing only the apk
 * package name and version. Two tools, run and probe, and no schemas derived
 * from anything, because nothing has been read yet.
 *
 * @param {{source: string}} input
 * @returns {Array<{path: string, content: string}>}
 */
export function draft({ source }) {
  const { pkg, version, name } = parseSource(source);
  const binary = pkg;
  const base = baseTools(binary);

  const description = describe(
    `Runs the Alpine package ${pkg}${version === "" ? "" : ` ${version}`} as a Package. This is the draft import: call probe, pass what it answers to the import-cli kit's refine tool, and write the refined files over these.`,
    `Runs the Alpine package ${pkg} as a Package.`,
  );

  const notes = [
    `# ${name}`,
    "",
    `Drafted by import-cli from \`${source}\`. It has not read the binary yet.`,
    "",
    "## Finish the import",
    "",
    "1. `pkg_build` this folder and `proc_run` it.",
    `2. Call \`${name}_probe\` with \`{}\`. It answers with the \`--help\`, \`--version\` and`,
    "   `man` text of the binary.",
    "3. Call the import-cli kit's `refine` tool with `{source, help, version, man}`,",
    "   using the same `source` as above.",
    "4. `fs_write` the files it returns over this folder, build again and run again.",
    "   The Package then has one tool per subcommand, with schemas.",
    "",
    "Until then the only structured tool is `run`, which takes an argv array.",
    "",
    callingContract(binary, [`${name}_run`, `${name}_probe`]),
    "## What the generator could not decide",
    "",
    "- Everything about the binary's flags. No help text has been read, so no",
    "  schema could be derived and nothing here is more than the package name.",
    version === ""
      ? "- The version. The source named none, so the Dockerfile installs whatever the base image's Alpine branch carries, and two builds of this folder can differ."
      : `- Whether apk resolves \`${pkg}=${version}\`. apk names a release as \`<pkgver>-r<pkgrel>\`, so a bare upstream version can fail the build; correct the Dockerfile if it does.`,
    `- Whether the binary is called \`${pkg}\`. An apk package does not have to install a binary of its own name. If \`run\` answers that the command is not found, fix \`binary\` and every \`argv\` in tools.json.`,
    "",
  ].join("\n");

  return [
    { path: "Dockerfile", content: dockerfile({ pkg, version, source }) },
    { path: "kitbash.yaml", content: manifest({ name, description, tools: base.manifestTools, source }) },
    { path: "tools.json", content: toolsJson({ binary, entries: base.entries }) },
    { path: "package.json", content: packageJson(name) },
    { path: "NOTES.md", content: notes },
  ];
}

// ---------------------------------------------------------------- refined

/**
 * The files for a Package whose tools carry schemas read from the binary's own
 * help text. One tool per subcommand when the help listed any, one for the
 * binary when it did not, plus run and probe.
 *
 * @param {{source: string, parsed: object, help?: string}} input
 * @returns {Array<{path: string, content: string}>}
 */
export function refined({ source, parsed, help = "" }) {
  const { pkg, version, name } = parseSource(source);
  const binary = pkg;
  const reading = parsed ?? { style: "unknown", options: [], positionals: [], subcommands: [], version: null };
  const style = reading.style ?? "unknown";
  const options = Array.isArray(reading.options) ? reading.options : [];
  const positionals = Array.isArray(reading.positionals) ? reading.positionals : [];
  const subcommands = Array.isArray(reading.subcommands) ? reading.subcommands : [];

  const base = baseTools(binary);
  const manifestTools = [...base.manifestTools];
  const entries = { ...base.entries };
  const derived = [];

  if (subcommands.length === 0) {
    const built = buildTool({
      toolName: toolNameFrom(name, ""),
      description: `Run ${binary} with the flags and arguments its help text describes. Every property is one flag or one positional; anything the schema does not carry can still be passed through the run tool.`,
      binary,
      argv: [binary],
      options,
      positionals,
    });
    manifestTools.push(built.manifestTool);
    entries[built.manifestTool.name] = built.entry;
    derived.push(built.manifestTool.name);
  } else {
    for (const subcommand of subcommands) {
      const toolName = toolNameFrom(name, subcommand.name);
      if (toolName === "" || entries[toolName]) continue;
      const built = buildTool({
        toolName,
        description: describe(subcommand.description, `Run ${binary} ${subcommand.name}.`),
        binary,
        argv: [binary, ...String(subcommand.name).split(/\s+/).filter(Boolean)],
        // A command table names the subcommands and describes nothing else, so
        // the flags read from the top level help are the ones on offer. A
        // subcommand's own flags arrive when its own help is refined.
        options: Array.isArray(subcommand.options) && subcommand.options.length > 0 ? subcommand.options : [],
        positionals: Array.isArray(subcommand.positionals) ? subcommand.positionals : [],
      });
      manifestTools.push(built.manifestTool);
      entries[built.manifestTool.name] = built.entry;
      derived.push(built.manifestTool.name);
    }
  }

  const description = describe(
    `Runs the Alpine package ${pkg}${version === "" ? "" : ` ${version}`} as a Package. Its tools were generated from the binary's own help text, one per ${subcommands.length === 0 ? "binary" : "subcommand"}, and return exit code, stdout, stderr and any file the command wrote.`,
    `Runs the Alpine package ${pkg} as a Package.`,
  );

  const undecided = [];
  if (style === "unknown") {
    undecided.push("- The help text matched no style this parser knows, so no flag and no argument became a schema. Everything has to go through `run`, or be written by hand from the help text below.");
  }
  if (positionals.length === 0 && subcommands.length === 0) {
    undecided.push("- No positional arguments were found. If the command takes any, add them to the tool's `properties` and to `positionals` in tools.json, in argv order.");
  }
  for (const option of options) {
    if (option.type === "array") {
      undecided.push(`- \`${option.flag}\` takes more than one value (${option.valueName}). It is typed as an array whose elements follow the flag as consecutive words. Check that against the help text.`);
    }
  }
  for (const positional of positionals) {
    if (INPUT_FILE_NAMES.has(positional.name)) {
      undecided.push(`- \`${positional.name}\` was taken for a file the caller supplies, so it carries \`format: kitbash-file\`. If it is really a plain string, drop the format.`);
    }
    if (OUTPUT_FILE_NAMES.has(positional.name)) {
      undecided.push(`- \`${positional.name}\` was taken for a file the command writes, so the adapter reads it back into \`files\`. If the command writes to standard output instead, drop it from \`outputs\` in tools.json.`);
    }
  }
  if (subcommands.length > 0) {
    undecided.push(`- Every subcommand tool carries no flags of its own. The top level help lists subcommands and not their flags, so refine each subcommand separately by probing \`${binary} <subcommand> --help\` and editing its tool.`);
  }
  if (undecided.length === 0) undecided.push("- Nothing. Every flag and argument in the help text became a property, which does not make any of them correct.");

  const helpText = typeof help === "string" ? help : "";

  const notes = [
    `# ${name}`,
    "",
    `Refined by import-cli from \`${source}\`, reading a help text in the \`${style}\` style.`,
    reading.version ? `The binary reported version \`${reading.version}\`.` : "The binary reported no version.",
    "",
    `Flags read: ${options.length}. Subcommands read: ${subcommands.length}. Positional arguments read: ${positionals.length}.`,
    "",
    callingContract(binary, [`${name}_run`, `${name}_probe`, ...derived.map((tool) => `${name}_${tool}`)]),
    "## What the generator could not decide",
    "",
    ...undecided,
    "",
    "## The help text this was read from",
    "",
    "```text",
    helpText === "" ? "(the caller passed no help text)" : helpText.replace(/```/g, "'''"),
    "```",
    "",
  ].join("\n");

  return [
    { path: "Dockerfile", content: dockerfile({ pkg, version, source }) },
    { path: "kitbash.yaml", content: manifest({ name, description, tools: manifestTools, source }) },
    { path: "tools.json", content: toolsJson({ binary, entries }) },
    { path: "package.json", content: packageJson(name) },
    { path: "NOTES.md", content: notes },
  ];
}
