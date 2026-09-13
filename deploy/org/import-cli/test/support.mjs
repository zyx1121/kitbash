// Two readers the tests need and the kit does not, kept out of the kit so that
// the image stays three files of code.
//
// readYaml understands the subset generate.js writes plus the two spellings the
// hand written kit manifests use: block mappings, block sequences, flow
// sequences of plain scalars, folded and literal block scalars, plain scalars
// and JSON quoted scalars. No anchors, no tags, no multi document. Reading the generated manifest
// back with it is what lets a test validate the bytes rather than the object
// they were written from.
//
// The plain scalar resolver is the part that matters most: a manifest is read
// on the Go side by gopkg.in/yaml.v3, and a description such as
// `0x00000000deadbeef` written as a plain scalar comes back from it as an
// integer and fails the schema. resolvePlain answers what that reader answers,
// so a generated manifest that would break on a real host breaks here first.
//
// validate is a JSON Schema 2020-12 subset: enough of it to run
// spec/manifest.schema.json against a document, which is the check that says a
// generated manifest would be visible on a real host. Unsupported keywords are
// ignored rather than guessed at, so the answer is never a false failure.

// ---------------------------------------------------------------- YAML reading

// What gopkg.in/yaml.v3 resolves a plain scalar to, which is the reader the Go
// side of this repository uses. Faithful enough to catch a generated manifest
// whose description a Go reader would hand back as a number:
//
//   resolve.go dispatches on the first byte. A letter other than the ones the
//   word table holds is always a string. A digit or a sign is tried as a
//   timestamp, then as an integer with strconv.ParseInt base 0, which reads
//   0x, 0o, 0b and a leading zero as octal and allows underscores between
//   digits, then as a float. A leading dot is tried as a float.
//
// Anything it does not resolve is the string it was written as.
const YAML_WORDS = new Map([
  ["", null], ["~", null], ["null", null], ["Null", null], ["NULL", null],
  ["true", true], ["True", true], ["TRUE", true],
  ["false", false], ["False", false], ["FALSE", false],
  [".nan", Number.NaN], [".NaN", Number.NaN], [".NAN", Number.NaN],
  [".inf", Infinity], [".Inf", Infinity], [".INF", Infinity],
  ["+.inf", Infinity], ["+.Inf", Infinity], ["+.INF", Infinity],
  ["-.inf", -Infinity], ["-.Inf", -Infinity], ["-.INF", -Infinity],
]);

// yaml.v3's yamlStyleFloat.
const YAML_FLOAT = /^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$/;
// The timestamp layouts parseTimestamp accepts, as one pattern.
const YAML_TIMESTAMP = /^[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}([Tt ]+[0-9]{1,2}:[0-9]{2}:[0-9]{2}(\.[0-9]*)?\s*([Zz]|[-+][0-9]{1,2}(:?[0-9]{2})?)?)?$/;

// strconv.ParseInt(text, 0, 64), which is what makes 0x, 0o, 0b and a leading
// zero integers rather than strings.
function goParseInt(text) {
  let body = text;
  let sign = 1;
  if (body.startsWith("+")) body = body.slice(1);
  else if (body.startsWith("-")) {
    sign = -1;
    body = body.slice(1);
  }
  if (body === "") return null;
  let base = 10;
  let digits = body;
  if (/^0[xX]/.test(body)) [base, digits] = [16, body.slice(2)];
  else if (/^0[oO]/.test(body)) [base, digits] = [8, body.slice(2)];
  else if (/^0[bB]/.test(body)) [base, digits] = [2, body.slice(2)];
  else if (/^0[0-7]+$/.test(body)) [base, digits] = [8, body.slice(1)];
  const allowed = { 2: /^[01]+$/, 8: /^[0-7]+$/, 10: /^[0-9]+$/, 16: /^[0-9a-fA-F]+$/ }[base];
  if (digits === "" || !allowed.test(digits)) return null;
  return sign * Number.parseInt(digits, base);
}

export function resolvePlain(value) {
  if (YAML_WORDS.has(value)) return YAML_WORDS.get(value);
  const first = value[0];
  if (first === ".") {
    return YAML_FLOAT.test(value) ? Number.parseFloat(value) : value;
  }
  if (!/[0-9+-]/.test(first ?? "")) return value;
  if (YAML_TIMESTAMP.test(value)) return new Date(value.replace(" ", "T"));
  // yaml.v3 strips underscores before it tries a number.
  const plain = value.replace(/_/g, "");
  const integer = goParseInt(plain);
  if (integer !== null) return integer;
  if (YAML_FLOAT.test(plain)) return Number.parseFloat(plain);
  return value;
}

function scalar(text) {
  const value = text.trim();
  if (value.startsWith('"')) return JSON.parse(value);
  // A flow sequence of plain scalars, which is how the hand written manifests
  // write their tags and their kit hooks.
  if (/^\[.*\]$/.test(value) && value !== "[]") {
    return value
      .slice(1, -1)
      .split(",")
      .map((item) => item.trim())
      .filter((item) => item !== "")
      .map((item) => scalar(item));
  }
  if (value === "[]") return [];
  // A flow mapping of plain scalars, which is how the hand written manifests
  // write limits and short property schemas.
  if (/^\{.*\}$/.test(value) && value !== "{}") {
    const map = {};
    for (const entry of value.slice(1, -1).split(",")) {
      const at = entry.indexOf(":");
      if (at === -1) continue;
      map[entry.slice(0, at).trim()] = scalar(entry.slice(at + 1));
    }
    return map;
  }
  if (value === "{}") return {};
  return resolvePlain(value);
}

function significantLines(text) {
  return text
    .split("\n")
    .map((line, number) => ({ line, number }))
    .filter(({ line }) => line.trim() !== "" && !/^\s*#/.test(line));
}

function readBlock(rows, start, indent) {
  if (start >= rows.length) return [null, start];
  const first = rows[start].line;
  const own = first.length - first.trimStart().length;
  if (own !== indent) throw new Error(`line ${rows[start].number + 1}: expected indent ${indent}, found ${own}`);

  if (first.trimStart().startsWith("- ")) {
    const items = [];
    let cursor = start;
    while (cursor < rows.length) {
      const row = rows[cursor];
      const at = row.line.length - row.line.trimStart().length;
      if (at < indent) break;
      if (at > indent) throw new Error(`line ${row.number + 1}: unexpected indent in a sequence`);
      if (!row.line.trimStart().startsWith("- ")) break;
      const inline = row.line.slice(indent + 2);
      if (/^[A-Za-z_$"][^:]*:( |$)/.test(inline)) {
        // A sequence of mappings: the dash carries the first key, and the rest
        // of the mapping is indented two further.
        const rewritten = rows.slice(cursor).map((entry, offset) =>
          offset === 0 ? { line: `${" ".repeat(indent + 2)}${inline}`, number: entry.number } : entry,
        );
        const [value, used] = readBlock(rewritten, 0, indent + 2);
        items.push(value);
        cursor += used;
        continue;
      }
      items.push(scalar(inline));
      cursor += 1;
    }
    return [items, cursor - start];
  }

  const map = {};
  let cursor = start;
  while (cursor < rows.length) {
    const row = rows[cursor];
    const at = row.line.length - row.line.trimStart().length;
    if (at < indent) break;
    if (at > indent) throw new Error(`line ${row.number + 1}: unexpected indent in a mapping`);
    if (row.line.trimStart().startsWith("- ")) break;
    const body = row.line.slice(indent);
    const match = /^("(?:[^"\\]|\\.)*"|[^:]+):(?:\s(.*))?$/.exec(body);
    if (!match) throw new Error(`line ${row.number + 1}: not a mapping entry: ${body}`);
    const key = match[1].startsWith('"') ? JSON.parse(match[1]) : match[1].trim();
    const rest = (match[2] ?? "").trim();
    // A folded or a literal block scalar: the indented lines that follow are
    // the value, joined with a space or kept as lines.
    if (rest === ">-" || rest === ">" || rest === "|-" || rest === "|") {
      const parts = [];
      let ahead = cursor + 1;
      while (ahead < rows.length) {
        const line = rows[ahead].line;
        if (line.length - line.trimStart().length <= indent) break;
        parts.push(line.trim());
        ahead += 1;
      }
      map[key] = parts.join(rest.startsWith(">") ? " " : "\n");
      cursor = ahead;
      continue;
    }
    if (rest !== "") {
      map[key] = scalar(rest);
      cursor += 1;
      continue;
    }
    if (cursor + 1 >= rows.length) {
      map[key] = null;
      cursor += 1;
      continue;
    }
    const next = rows[cursor + 1].line;
    const nextIndent = next.length - next.trimStart().length;
    if (nextIndent <= indent) {
      map[key] = null;
      cursor += 1;
      continue;
    }
    const [value, used] = readBlock(rows, cursor + 1, nextIndent);
    map[key] = value;
    cursor += 1 + used;
  }
  return [map, cursor - start];
}

export function readYaml(text) {
  const rows = significantLines(text);
  if (rows.length === 0) return null;
  const first = rows[0].line;
  const [value] = readBlock(rows, 0, first.length - first.trimStart().length);
  return value;
}

// ------------------------------------------------------------ schema checking

function typeOf(value) {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  if (Number.isInteger(value)) return "integer";
  return typeof value === "object" ? "object" : typeof value;
}

function matchesType(value, want) {
  const kind = typeOf(value);
  if (want === "number") return kind === "number" || kind === "integer";
  if (want === "integer") return kind === "integer";
  return kind === want;
}

function resolve(schema, root) {
  if (!schema || typeof schema.$ref !== "string") return schema;
  if (!schema.$ref.startsWith("#/")) return schema;
  let node = root;
  for (const part of schema.$ref.slice(2).split("/")) node = node?.[part];
  return node ?? schema;
}

/**
 * Check one document against one schema. Answers the list of complaints, which
 * is empty when the document satisfies it.
 */
export function validate(document, schema, root = schema, at = "") {
  const problems = [];
  const rules = resolve(schema, root);
  if (!rules || typeof rules !== "object") return problems;
  const where = at === "" ? "the document" : at;

  if (rules.type !== undefined) {
    const wanted = Array.isArray(rules.type) ? rules.type : [rules.type];
    if (!wanted.some((want) => matchesType(document, want))) {
      problems.push(`${where} is ${typeOf(document)}, and the schema wants ${wanted.join(" or ")}`);
      return problems;
    }
  }
  if (rules.const !== undefined && document !== rules.const) {
    problems.push(`${where} is ${JSON.stringify(document)}, and the schema wants ${JSON.stringify(rules.const)}`);
  }
  if (Array.isArray(rules.enum) && !rules.enum.some((option) => option === document)) {
    problems.push(`${where} is ${JSON.stringify(document)}, which is not one of ${JSON.stringify(rules.enum)}`);
  }

  if (typeof document === "string") {
    if (typeof rules.pattern === "string" && !new RegExp(rules.pattern, "u").test(document)) {
      problems.push(`${where} does not match ${rules.pattern}`);
    }
    if (typeof rules.maxLength === "number" && [...document].length > rules.maxLength) {
      problems.push(`${where} is ${[...document].length} characters, and at most ${rules.maxLength} are allowed`);
    }
    if (typeof rules.minLength === "number" && [...document].length < rules.minLength) {
      problems.push(`${where} is ${[...document].length} characters, and at least ${rules.minLength} are needed`);
    }
  }

  if (typeof document === "number") {
    if (typeof rules.maximum === "number" && document > rules.maximum) problems.push(`${where} is above ${rules.maximum}`);
    if (typeof rules.minimum === "number" && document < rules.minimum) problems.push(`${where} is below ${rules.minimum}`);
  }

  if (Array.isArray(document)) {
    if (typeof rules.minItems === "number" && document.length < rules.minItems) {
      problems.push(`${where} has ${document.length} items, and at least ${rules.minItems} are needed`);
    }
    if (typeof rules.maxItems === "number" && document.length > rules.maxItems) {
      problems.push(`${where} has ${document.length} items, and at most ${rules.maxItems} are allowed`);
    }
    if (rules.uniqueItems === true) {
      const seen = document.map((item) => JSON.stringify(item));
      if (new Set(seen).size !== seen.length) problems.push(`${where} repeats an item`);
    }
    if (rules.items) {
      document.forEach((item, index) => problems.push(...validate(item, rules.items, root, `${where}[${index}]`)));
    }
  }

  if (document !== null && typeof document === "object" && !Array.isArray(document)) {
    for (const key of rules.required ?? []) {
      if (!(key in document)) problems.push(`${where} carries no ${key}`);
    }
    const declared = rules.properties ?? {};
    for (const [key, value] of Object.entries(document)) {
      if (declared[key]) {
        problems.push(...validate(value, declared[key], root, `${where}.${key}`));
        continue;
      }
      if (rules.additionalProperties === false) problems.push(`${where} carries ${key}, which the schema does not declare`);
      else if (rules.additionalProperties && typeof rules.additionalProperties === "object") {
        problems.push(...validate(value, rules.additionalProperties, root, `${where}.${key}`));
      }
    }
  }

  if (Array.isArray(rules.oneOf)) {
    const passing = rules.oneOf.filter((option) => validate(document, option, root, where).length === 0);
    if (passing.length !== 1) problems.push(`${where} satisfies ${passing.length} of the ${rules.oneOf.length} alternatives, and exactly one is wanted`);
  }
  if (Array.isArray(rules.allOf)) {
    for (const option of rules.allOf) problems.push(...validate(document, option, root, where));
  }
  if (rules.not && validate(document, rules.not, root, where).length === 0) {
    problems.push(`${where} satisfies a schema it must not`);
  }

  return problems;
}
