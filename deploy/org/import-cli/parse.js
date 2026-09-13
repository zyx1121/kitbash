// parse.js: the deterministic reader of a command line tool's own help text.
//
// One exported function, parseHelp, which takes the text a binary prints for
// --help, --version and man and answers with the structure a schema can be
// generated from. It is pure: no file system, no network, no clock, no
// randomness and no dependencies, so the same three strings always produce the
// same answer and every case it gets wrong can be reproduced from a fixture.
//
// Four help styles are recognized, because four of them cover almost every
// binary an Alpine package installs:
//
//   gnu       getopt and hand written tables, such as jq, curl, git and ffmpeg.
//   argparse  Python's argparse, recognized by its help sections and by the
//             sentence it prints for -h.
//   cobra     Go's cobra and its relatives, recognized by their command tables.
//   clap      Rust's clap, recognized by its Usage, Arguments and Options
//             headings.
//
// A help text that matches none of them is style unknown with empty structures,
// which is the signal that the draft Package has to stay at its run tool and
// that whoever imported it has to write the schemas by hand.
//
// The hard part of reading a help table is telling an option's value names
// apart from the first words of its description, because some tables separate
// them with a single space. The answer here is the description column: the
// column most option lines start their description at is measured first, and
// every line is then cut at that column when the cut lands on a space and
// leaves balanced brackets behind. Lines that overflow the column fall back to
// the first run of two or more spaces, and lines that carry no description at
// all take it from the indented lines that follow.

// Tokens that appear where a positional would and name no argument.
const USAGE_PLACEHOLDERS = new Set([
  "options",
  "option",
  "opts",
  "flags",
  "flag",
  "args",
  "arg",
  "arguments",
  "command",
  "commands",
  "subcommand",
  "subcommands",
  "global options",
]);

// Section headings whose entries are not subcommands even when they are laid
// out like a command table. gh prints all four of them.
const NON_COMMAND_SECTIONS = /^(help topics|learn more|examples?|flags|usage|options|aliases|environment(al)? variables)\b/i;

// Value names that make an option a number rather than a string. The list is
// fixed on purpose: a heuristic that reads the description would stop being
// reproducible the moment a release reworded it.
const NUMERIC_VALUE_NAMES = new Set([
  "n",
  "num",
  "number",
  "count",
  "size",
  "bytes",
  "len",
  "length",
  "seconds",
  "secs",
  "sec",
  "ms",
  "millis",
  "timeout",
  "port",
  "offset",
  "level",
  "loglevel",
  "depth",
  "width",
  "height",
  "rate",
  "bitrate",
  "quality",
  "channels",
  "priority",
  "max",
  "min",
  "limit",
  "threads",
  "jobs",
  "retries",
  "index",
  "lines",
  "columns",
  "percent",
  "quantity",
]);

// Words that are YAML booleans or that a plain scalar cannot be, kept here
// because the option name normalizer shares the check with the manifest writer.
const RESERVED_PROPERTY_NAMES = new Set(["stdin", "files", "args"]);

// ---------------------------------------------------------------- text hygiene

// man writes bold and underline as a character, a backspace and the character
// again, and a terminal that has no pager shows exactly those bytes. Strip the
// overstrike, then the ANSI sequences a coloured help writes, then the carriage
// returns a Windows built binary leaves behind.
function clean(text) {
  if (typeof text !== "string") return "";
  return text
    .replace(/\r\n?/g, "\n")
    .replace(/[^\n]\x08/g, "")
    .replace(/\x1b\[[0-9;?]*[ -\/]*[@-~]/g, "")
    .replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, "");
}

// Column arithmetic is the whole mechanism of this parser, so a tab has to
// become the spaces a terminal would have shown. jq writes its usage with tabs.
function expandTabs(line) {
  let out = "";
  for (const character of line) {
    if (character === "\t") out += " ".repeat(8 - (out.length % 8));
    else out += character;
  }
  return out;
}

function toLines(text) {
  return clean(text).split("\n").map(expandTabs);
}

const indentOf = (line) => line.length - line.trimStart().length;

// A head is only a head when every bracket it opened is closed again, which is
// what keeps a cut at the description column from landing inside <a[:b]> and
// turning the rest of the value name into a description.
function bracketsBalanced(text) {
  const stack = [];
  const pairs = { ")": "(", "]": "[", "}": "{", ">": "<" };
  for (const character of text) {
    if (character === "(" || character === "[" || character === "{" || character === "<") stack.push(character);
    else if (pairs[character]) {
      if (stack.pop() !== pairs[character]) return false;
    }
  }
  return stack.length === 0;
}

// Split on whitespace, but keep a bracketed group together so that <jq filter>
// and [(amend|reword):]<commit> each stay one token.
function tokenize(text) {
  const tokens = [];
  let current = "";
  let depth = 0;
  for (const character of text) {
    if (character === "(" || character === "[" || character === "{" || character === "<") depth += 1;
    else if (character === ")" || character === "]" || character === "}" || character === ">") depth = Math.max(0, depth - 1);
    if (/\s/.test(character) && depth === 0) {
      if (current !== "") tokens.push(current);
      current = "";
      continue;
    }
    current += character;
  }
  if (current !== "") tokens.push(current);
  return tokens;
}

const collapse = (text) => text.replace(/\s+/g, " ").trim();

// ---------------------------------------------------------------- option heads

// Turn a value token such as <file>, [<mode>], DIRECTORY or dir into the bare
// name a schema property can be built from. An empty answer means the token
// named no value.
function valueName(token) {
  let inner = token.trim();
  // A trailing ellipsis means the value repeats and is not part of its name.
  inner = inner.replace(/\.{3}$/, "");
  while (/^[<[{(].*[>\]})]$/.test(inner)) inner = inner.slice(1, -1).trim();
  inner = inner.replace(/\.{3}$/, "").trim();
  if (inner === "") return "";
  // <jq filter> and [outfile options] name their value with the last word.
  const words = inner.split(/\s+/);
  let name = words[words.length - 1];
  // Any bracket still glued to the name belongs to a nested group such as
  // <provider1[:prvdr2[:reg[:srv]]]>, which names one value and not four.
  name = name.replace(/[<>[\]{}()]/g, "");
  // A value written as a|b, a:b or a/b names its first alternative.
  name = name.split(/[|:=/]/)[0];
  return name.replace(/[^A-Za-z0-9_.-]/g, "").trim();
}

// A single token of an option head, such as --arg, -S[<keyid>] or --unified=<n>.
// Answers null when the token is not an option, which is how a line is refused.
function parseOptionToken(raw) {
  // clap marks a repeatable flag as --flag..., and the ellipsis is not part of
  // the name, so it comes off before the name is read.
  const trailing = /\.{3}$/.test(raw);
  const token = raw.replace(/\.{3}$/, "");
  const match = /^(--?)(\[no-\])?([A-Za-z0-9][A-Za-z0-9_.+-]*?)(=.*|\[.*|<.*|\{.*)?$/.exec(token);
  if (!match) return null;
  const dashes = match[1];
  const name = match[3];
  let rest = match[4] ?? "";
  const values = [];
  let takesValue = false;

  // ffmpeg writes a per stream option as -c[:<stream_spec>], which qualifies
  // the flag rather than naming a value.
  rest = rest.replace(/^\[:[^\]]*\]/, "");
  const repeatable = trailing || /\.{3}$/.test(rest);
  rest = rest.replace(/\.{3}$/, "");

  if (rest.startsWith("=")) {
    takesValue = true;
    const named = valueName(rest.slice(1));
    if (named !== "") values.push(named);
  } else if (/^\[=/.test(rest)) {
    takesValue = true;
    const named = valueName(rest.slice(2, -1));
    if (named !== "") values.push(named);
  } else if (/^[<[{]/.test(rest)) {
    takesValue = true;
    const named = valueName(rest);
    if (named !== "") values.push(named);
  } else if (rest.trim() !== "") {
    // Anything else glued to the name is not an option this parser knows.
    return null;
  }

  return { flag: `${dashes}${name}`, short: dashes === "-" && name.length === 1, takesValue, values, repeatable };
}

// Parse a whole option head, which is one or more comma separated option
// tokens and the value names that follow them. Answers null when the text is
// not an option head, and refuses a head with more than three value tokens
// because that is a description that was mistaken for one.
function parseHead(head) {
  const text = head.trim();
  if (!text.startsWith("-") || text === "-" || text === "--") return null;
  const tokens = tokenize(text.replace(/,/g, " , "));
  const atoms = [];
  const values = [];
  let repeatable = false;
  let takesValue = false;
  let loose = 0;

  for (const token of tokens) {
    if (token === "," || token === "|") continue;
    if (token.startsWith("-") && token !== "-" && token !== "--") {
      const atom = parseOptionToken(token);
      if (!atom) return null;
      atoms.push(atom);
      if (atom.takesValue) takesValue = true;
      if (atom.repeatable) repeatable = true;
      for (const value of atom.values) if (!values.includes(value)) values.push(value);
      continue;
    }
    if (atoms.length === 0) return null;
    const named = valueName(token);
    if (named === "") return null;
    // A bracketed token is a value whatever it contains. A bare word is only a
    // value when it reads like a name, because the alternative is that it is
    // the first word of a description that had no column to start in.
    const bracketed = /^[<[{(]/.test(token);
    if (!bracketed && !/^[A-Za-z_][A-Za-z0-9_.-]*$/.test(named)) return null;
    loose += 1;
    if (loose > 3) return null;
    takesValue = true;
    if (/\.{3}$/.test(token)) repeatable = true;
    if (!values.includes(named)) values.push(named);
  }

  if (atoms.length === 0) return null;
  const long = atoms.find((atom) => !atom.short);
  const short = atoms.find((atom) => atom.short);
  return {
    flag: (long ?? atoms[0]).flag,
    short: short && long ? short.flag : undefined,
    takesValue,
    values,
    repeatable,
  };
}

// The type an option's value is given in the generated schema.
function optionType({ takesValue, values }) {
  if (!takesValue) return "boolean";
  if (values.length > 1) return "array";
  const name = (values[0] ?? "").toLowerCase().replace(/[^a-z0-9]/g, "");
  if (NUMERIC_VALUE_NAMES.has(name)) return "number";
  return "string";
}

// ---------------------------------------------------------------- help layout

// Find the block of usage lines: the ones the help prints under its usage
// heading, up to the first blank line. Everything inside it is excluded from
// the option scan, because a usage line lists flags it does not describe.
function usageBlock(lines) {
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    const match = /^(\s*)(usage|Usage|USAGE|SYNOPSIS)\s*:?\s*(.*)$/.exec(line);
    if (!match) continue;
    const first = match[3].trim();
    const block = [];
    let cursor = index;
    if (first !== "") block.push(first);
    else cursor = index;
    for (let next = index + 1; next < lines.length; next += 1) {
      if (lines[next].trim() === "") {
        if (block.length > 0) {
          cursor = next;
          break;
        }
        continue;
      }
      if (indentOf(lines[next]) === 0 && block.length > 0) break;
      block.push(lines[next].trim());
      cursor = next;
    }
    return { start: index, end: cursor, lines: block };
  }
  return { start: -1, end: -1, lines: [] };
}

// Measure the column most descriptions start at. Only lines that are option
// heads with a two space gap vote, and a column needs two votes to win, which
// is enough for a help that describes two flags and few enough that a table of
// one line cannot invent a column.
function descriptionColumn(lines) {
  const votes = new Map();
  for (const line of lines) {
    if (line.trim() === "" || !line.trimStart().startsWith("-")) continue;
    const gap = /\S {2,}\S/.exec(line);
    if (!gap) continue;
    const column = gap.index + gap[0].length - 1;
    if (!parseHead(line.slice(0, column))) continue;
    votes.set(column, (votes.get(column) ?? 0) + 1);
  }
  let best = -1;
  let bestVotes = 1;
  for (const [column, count] of [...votes.entries()].sort((a, b) => a[0] - b[0])) {
    if (count > bestVotes) {
      best = column;
      bestVotes = count;
    }
  }
  return best;
}

// Cut one line into its option head and the first line of its description.
// Answers null when the line is not an option line.
function splitOptionLine(line, column) {
  if (line.trim() === "" || !line.trimStart().startsWith("-")) return null;

  // The cut at the description column, valid only when it lands on a space and
  // leaves every bracket it passed closed behind it.
  let byColumn = null;
  if (column > 0 && line.length > column && line[column] !== " " && line[column - 1] === " ") {
    const head = line.slice(0, column);
    if (bracketsBalanced(head)) {
      const parsed = parseHead(head);
      if (parsed) byColumn = { parsed, description: line.slice(column).trim() };
    }
  }

  // The cut at the first run of two or more spaces, which is where a line that
  // overflows the description column puts its description instead.
  let byGap = null;
  const gap = /\S {2,}\S/.exec(line);
  if (gap) {
    const at = gap.index + gap[0].length - 1;
    const parsed = parseHead(line.slice(0, at));
    if (parsed) byGap = { parsed, description: line.slice(at).trim() };
  }

  // When both cuts parse, the one that found more value names is the one that
  // did not cut a value name off: ffmpeg writes -ar[:<stream_spec>] <rate> so
  // that its description column falls between the flag and its value.
  if (byColumn && byGap) return byGap.parsed.values.length > byColumn.parsed.values.length ? byGap : byColumn;
  if (byColumn) return byColumn;
  if (byGap) return byGap;

  const whole = parseHead(line);
  if (whole) return { parsed: whole, description: "" };
  return null;
}

// Read every option out of a help or man body. Continuation lines are the
// indented lines that follow an option line and are not option lines
// themselves; a blank line ends a description, which is what keeps the long
// form of a clap help and the body of a man page from running together.
function scanOptions(lines, column, allowed) {
  const found = [];
  for (let index = 0; index < lines.length; index += 1) {
    if (allowed && !allowed.has(indentOf(lines[index]))) continue;
    const split = splitOptionLine(lines[index], column);
    if (!split) continue;
    const own = indentOf(lines[index]);
    const parts = split.description === "" ? [] : [split.description];
    for (let next = index + 1; next < lines.length; next += 1) {
      const line = lines[next];
      if (line.trim() === "") break;
      if ((!allowed || allowed.has(indentOf(line))) && splitOptionLine(line, column)) break;
      // A description is continued either in the description column, when the
      // table has one, or by any line indented deeper than its own head.
      const continued = column > 0 ? indentOf(line) >= column : indentOf(line) > own;
      if (!continued) break;
      parts.push(line.trim());
      index = next;
    }
    found.push({
      ...split.parsed,
      // ffmpeg writes its help topics as "-h long -- print more options", and
      // the dashes are a separator rather than the first word of a sentence.
      description: collapse(parts.join(" ")).replace(/^--\s*/, "").replace(/[;,]$/, ""),
    });
  }
  return found;
}

// Where an option head is allowed to start. A help table aligns its heads at
// one indent and the long only flags four columns further in, so those two are
// the whole allowance; a line deeper than that is a description that happens to
// open with a flag name. A man page aligns every head at one indent, so it gets
// the one.
function headIndents(lines, column, spread) {
  const indents = new Set();
  for (const line of lines) {
    if (line.trim() === "") continue;
    if (!splitOptionLine(line, column)) continue;
    indents.add(indentOf(line));
  }
  if (indents.size === 0) return indents;
  const min = Math.min(...indents);
  return spread ? new Set([min, min + 4]) : new Set([min]);
}

// One option record per flag, first sighting wins and a later sighting fills in
// a description or a value the first one did not carry.
function mergeOptions(groups) {
  const byFlag = new Map();
  for (const option of groups) {
    const existing = byFlag.get(option.flag);
    if (!existing) {
      byFlag.set(option.flag, { ...option });
      continue;
    }
    if (!existing.short && option.short) existing.short = option.short;
    if (!existing.takesValue && option.takesValue) {
      existing.takesValue = true;
      existing.values = option.values;
    }
    if (existing.values.length === 0 && option.values.length > 0) existing.values = option.values;
    if (existing.description === "" && option.description !== "") existing.description = option.description;
    if (option.repeatable) existing.repeatable = true;
  }
  return [...byFlag.values()].map((option) => ({
    flag: option.flag,
    ...(option.short ? { short: option.short } : {}),
    takesValue: option.takesValue,
    ...(option.values.length > 0 ? { valueName: option.values.join(" ") } : {}),
    type: optionType(option),
    description: option.description,
    ...(option.repeatable ? { repeatable: true } : {}),
  }));
}

// ---------------------------------------------------------------- positionals

// Whether a usage token names a positional, and under what name. A token is an
// option group rather than a positional when its first word is a flag, and a
// choice between options when a bar separates its alternatives, so both are
// answered with null.
function usagePositional(token) {
  let inner = token.trim();
  const variadic = /\.{3}[\]})>]*$/.test(inner);
  const required = !/^[[{]/.test(inner);
  inner = inner.replace(/\.{3}$/, "");
  while (/^[<[{(].*[>\]})]$/.test(inner) && bracketsBalanced(inner.slice(1, -1))) {
    inner = inner.slice(1, -1).trim().replace(/\.{3}$/, "");
  }
  if (inner === "") return null;

  const words = inner.split(/\s+/);
  const first = words[0].replace(/^[<[{(]+/, "");
  if (first.startsWith("-")) return null;
  // A bar at the outermost level separates whole alternatives, which a single
  // schema property cannot stand for.
  let depth = 0;
  for (const character of inner) {
    if ("([{<".includes(character)) depth += 1;
    else if (")]}>".includes(character)) depth -= 1;
    else if (character === "|" && depth === 0) return null;
  }

  const name = valueName(inner)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  if (name === "") return null;
  return { name, required, variadic, description: "" };
}

// The name a usage or section token becomes as a schema property.
function positionalName(token) {
  return valueName(token)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
}

// Read the positionals out of the first usage alternative. A usage that spans
// several lines is one alternative when the extra lines do not start with the
// binary again, which is how git's six line usage stays one and jq's three
// usages stay three.
function scanPositionals(block, binary) {
  if (block.length === 0) return [];
  const used = [block[0]];
  for (let index = 1; index < block.length; index += 1) {
    const line = block[index].trim();
    // A line that opens with a word rather than a bracket or a flag starts the
    // next usage alternative, and only the first alternative is read.
    if (!/^[[{(<-]/.test(line)) break;
    used.push(line);
  }

  const tokens = tokenize(used.join(" "));
  const positionals = [];
  let leading = true;
  for (const token of tokens) {
    if (leading) {
      // The command path is the run of bare words the usage opens with, which
      // is the binary and, in a subcommand's help, the subcommand as well.
      if (/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(token)) continue;
      leading = false;
    }
    const entry = usagePositional(token);
    if (!entry) continue;
    if (USAGE_PLACEHOLDERS.has(entry.name)) continue;
    if (entry.name === binary) continue;
    if (positionals.some((existing) => existing.name === entry.name)) continue;
    positionals.push(entry);
  }
  return positionals;
}

// argparse and clap both print a section that names the positionals and
// describes them. Its entries fill in descriptions, and name positionals a
// usage line did not carry.
function scanPositionalSection(lines, column) {
  const entries = [];
  let inside = false;
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    if (line.trim() === "") continue;
    if (indentOf(line) === 0) {
      inside = /^(positional arguments|arguments|args|positional_arguments)\s*:?\s*$/i.test(line.trim());
      continue;
    }
    if (!inside) continue;
    if (line.trimStart().startsWith("-")) continue;
    const own = indentOf(line);
    const gap = /\S {2,}\S/.exec(line);
    const head = gap ? line.slice(0, gap.index + gap[0].length - 1) : line;
    const name = positionalName(head.trim());
    if (name === "" || USAGE_PLACEHOLDERS.has(name)) continue;
    const parts = [];
    if (gap) parts.push(line.slice(gap.index + gap[0].length - 1).trim());
    for (let next = index + 1; next < lines.length; next += 1) {
      if (lines[next].trim() === "") break;
      if (indentOf(lines[next]) <= own) break;
      parts.push(lines[next].trim());
      index = next;
    }
    entries.push({
      name,
      required: !/^\[/.test(head.trim()),
      variadic: /\.{3}/.test(head.trim()),
      description: collapse(parts.join(" ")),
    });
  }
  return entries;
}

// ---------------------------------------------------------------- subcommands

// A command table is a run of indented lines whose first word is a bare
// lowercase name and whose rest is a sentence. It is only read when the usage
// says the binary takes a command at all, which keeps an option table from
// being mistaken for one.
function scanSubcommands(lines, block, skip) {
  const usage = block.join(" ");
  if (!/[<[](sub)?command[>\]]|\bCOMMAND\b|\bsubcommand\b/i.test(usage)) return [];
  const found = [];
  let section = "";
  for (let index = 0; index < lines.length; index += 1) {
    if (skip.has(index)) continue;
    const line = lines[index];
    if (line.trim() === "") continue;
    if (indentOf(line) === 0) {
      section = line.trim();
      continue;
    }
    if (NON_COMMAND_SECTIONS.test(section)) continue;
    const match = /^\s{2,}([a-z][a-z0-9]*(?:[_-][a-z0-9]+)*):?(?: {2,}(.*))?$/.exec(line);
    if (!match) continue;
    const name = match[1];
    const description = collapse(match[2] ?? "");
    if (description === "") continue;
    if (found.some((entry) => entry.name === name)) continue;
    found.push({ name, description, options: [], positionals: [] });
  }
  return found;
}

// ---------------------------------------------------------------- man pages

// Read a man page's SYNOPSIS and OPTIONS sections. A man page indents both its
// headings' bodies, so the option heads sit on their own lines and their
// descriptions follow indented, which scanOptions already handles.
function scanMan(text, binary) {
  const lines = toLines(text);
  const sections = new Map();
  let current = "";
  for (const line of lines) {
    if (/^[A-Z][A-Z0-9 ]*$/.test(line.trim()) && indentOf(line) === 0) {
      current = line.trim();
      if (!sections.has(current)) sections.set(current, []);
      continue;
    }
    if (current !== "") sections.get(current).push(line);
  }

  const optionLines = [];
  for (const [name, body] of sections) {
    if (/OPTIONS|^INVOKING\b|SWITCHES/.test(name)) optionLines.push(...body);
  }
  const column = descriptionColumn(optionLines);
  // The raw sightings, not merged records: the caller merges the man page and
  // the help text together so one flag described in both stays one option.
  const options = scanOptions(optionLines, column, headIndents(optionLines, column, false));

  const synopsis = sections.get("SYNOPSIS") ?? [];
  const block = [];
  for (const line of synopsis) {
    if (line.trim() === "") {
      if (block.length > 0) break;
      continue;
    }
    block.push(line.trim());
  }
  const positionals = scanPositionals(block, binary);
  return { options, positionals };
}

// ---------------------------------------------------------------- style

function detectStyle(help, optionCount, subcommandCount, hasUsage) {
  if (/^\s*-h, --help\s+show this help message and exit/m.test(help)) return "argparse";
  if (/^(positional arguments|optional arguments|options):\s*$/m.test(help) && /^usage: /m.test(help)) return "argparse";
  if (/^\s*Available Commands:\s*$/m.test(help)) return "cobra";
  if (/^\s*Global Flags:\s*$/m.test(help)) return "cobra";
  if (/for more information about a command/i.test(help)) return "cobra";
  if (/^Usage: /m.test(help) && /^(Arguments|Options|Commands):\s*$/m.test(help)) return "clap";
  if (/^USAGE:\s*$/m.test(help) && /^(POSITIONAL ARGUMENTS|OPTIONS|ARGS):\s*$/m.test(help)) return "clap";
  if (/^\s+-h, --help\s+Print help/m.test(help)) return "clap";
  if (optionCount > 0 || subcommandCount > 0 || hasUsage) return "gnu";
  return "unknown";
}

// ---------------------------------------------------------------- version

// The first version number on the first line of whatever the binary printed.
// jq answers jq-1.8.2, curl answers a paragraph, git answers a sentence, and
// all three start with the number that matters.
function extractVersion(version, help) {
  for (const text of [version, help]) {
    const cleaned = clean(text ?? "");
    if (cleaned.trim() === "") continue;
    const head = cleaned.split("\n").slice(0, 2).join(" ");
    const match = /(?:^|[^\w.])v?(\d+\.\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.]+)?)/.exec(head);
    if (match) return match[1];
  }
  return null;
}

// ---------------------------------------------------------------- the parser

/**
 * Read a binary's own help text into the structure a manifest is generated
 * from. Pure: the same inputs always produce the same answer.
 *
 * @param {{binary?: string, help?: string, version?: string, man?: string}} input
 * @returns {{version: string|null, subcommands: Array, options: Array, positionals: Array, style: string}}
 */
export function parseHelp({ binary = "", help = "", version = "", man = "" } = {}) {
  const name = String(binary).trim();
  const helpText = clean(help);
  const lines = toLines(help);
  const block = usageBlock(lines);
  const skip = new Set();
  for (let index = block.start; index >= 0 && index <= block.end; index += 1) skip.add(index);

  const bodyLines = lines.map((line, index) => (skip.has(index) ? "" : line));
  const column = descriptionColumn(bodyLines);
  const helpOptions = scanOptions(bodyLines, column, headIndents(bodyLines, column, true));
  const subcommands = scanSubcommands(lines, block.lines, skip);

  const manResult = man && man.trim() !== "" ? scanMan(man, name) : { options: [], positionals: [] };
  const options = mergeOptions([...helpOptions, ...manResult.options]);

  const positionals = scanPositionals(block.lines, name);
  for (const entry of manResult.positionals) {
    if (!positionals.some((existing) => existing.name === entry.name)) positionals.push(entry);
  }
  for (const entry of scanPositionalSection(lines, column)) {
    const existing = positionals.find((candidate) => candidate.name === entry.name);
    if (existing) {
      if (existing.description === "") existing.description = entry.description;
      if (entry.variadic) existing.variadic = true;
    } else {
      positionals.push(entry);
    }
  }

  const style = detectStyle(helpText, options.length, subcommands.length, block.lines.length > 0);
  if (style === "unknown") {
    return { version: extractVersion(version, help), subcommands: [], options: [], positionals: [], style };
  }
  return { version: extractVersion(version, help), subcommands, options, positionals, style };
}

// Exported for the generator, which names schema properties the same way.
export function propertyName(raw) {
  const normalized = String(raw)
    .replace(/^-+/, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  if (normalized === "") return "";
  if (/^[0-9]/.test(normalized)) return `opt_${normalized}`;
  if (RESERVED_PROPERTY_NAMES.has(normalized)) return `${normalized}_flag`;
  return normalized;
}
