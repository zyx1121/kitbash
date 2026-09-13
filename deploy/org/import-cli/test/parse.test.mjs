// Parser tests against help text captured from real binaries.
//
//   cd deploy/org/import-cli && npm test
//   node --test "deploy/org/import-cli/test/*.test.mjs"
//
// Every fixture under test/fixtures was printed by the named binary inside a
// docker.io/library/alpine:3.23 container, which is the image the Packages this
// kit writes are built from, so the text here is the text the adapter's probe
// tool will report. The man page was passed through a filter that removes the
// overstrike a pager would have consumed; parse.js removes it as well, and the
// last test in this file is what says so.
//
// Each fixture asserts the same three things: the style that was detected, a
// floor on how many flags and subcommands were read, and the positional
// arguments by name. A floor rather than an exact count, because a later
// release of the binary adds flags and the point of the test is that the table
// is still being read.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";

import { parseHelp } from "../parse.js";

const fixtures = path.join(import.meta.dirname, "fixtures");
const read = (name) => {
  try {
    return readFileSync(path.join(fixtures, name), "utf8");
  } catch {
    return "";
  }
};

// One fixture read the way the adapter's probe tool would report it.
const probe = (fixture, binary) =>
  parseHelp({
    binary,
    help: read(`${fixture}.help.txt`),
    version: read(`${fixture}.version.txt`),
    man: read(`${fixture}.man.txt`),
  });

const flag = (parsed, name) => parsed.options.find((option) => option.flag === name);
const names = (entries) => entries.map((entry) => entry.name);

test("jq, a hand written GNU option table", () => {
  const parsed = probe("jq", "jq");
  assert.equal(parsed.style, "gnu");
  assert.equal(parsed.version, "1.8.2");
  assert.ok(parsed.options.length >= 25, `expected at least 25 flags, found ${parsed.options.length}`);
  assert.equal(parsed.subcommands.length, 0);
  assert.deepEqual(names(parsed.positionals), ["filter", "file"]);
  assert.equal(parsed.positionals[0].required, true);
  assert.equal(parsed.positionals[1].variadic, true);

  assert.deepEqual(flag(parsed, "--compact-output"), {
    flag: "--compact-output",
    short: "-c",
    takesValue: false,
    type: "boolean",
    description: "compact instead of pretty-printed output",
  });
  // The value name is n, so the property is a number rather than a string.
  assert.equal(flag(parsed, "--indent").type, "number");
  assert.equal(flag(parsed, "--indent").takesValue, true);
  // A table that separates a value name from the description with one space:
  // --slurpfile name file set $name to an array of JSON values read.
  assert.equal(flag(parsed, "--slurpfile").valueName, "name file");
  assert.equal(flag(parsed, "--slurpfile").type, "array");
  assert.equal(flag(parsed, "--slurpfile").description, "set $name to an array of JSON values read from the file");
  // A long option written without a short one keeps no short.
  assert.equal(flag(parsed, "--tab").short, undefined);
  assert.equal(flag(parsed, "--library-path").short, "-L");
  assert.equal(flag(parsed, "--library-path").valueName, "dir");
});

test("curl, the widest option table of the five", () => {
  const parsed = probe("curl", "curl");
  assert.equal(parsed.style, "gnu");
  assert.equal(parsed.version, "8.22.0");
  assert.ok(parsed.options.length >= 250, `expected at least 250 flags, found ${parsed.options.length}`);
  assert.equal(parsed.subcommands.length, 0);
  // curl --help all prints no usage line, so there is nothing to read a
  // positional from and the URL has to come from the caller through run.
  assert.deepEqual(parsed.positionals, []);

  assert.equal(flag(parsed, "--data").short, "-d");
  assert.equal(flag(parsed, "--data").type, "string");
  assert.equal(flag(parsed, "--max-time").type, "number");
  // A value name with punctuation in it: <header/@file>.
  assert.equal(flag(parsed, "--header").valueName, "header");
  // A line that overflows the description column falls back to the two space
  // rule rather than cutting the value name in half.
  assert.equal(flag(parsed, "--aws-sigv4").valueName, "provider1");
  assert.equal(flag(parsed, "--aws-sigv4").description, "AWS V4 signature auth");
  // Every flag that was read is a flag, not a run of description words.
  for (const option of parsed.options) {
    assert.match(option.flag, /^--?[A-Za-z0-9][A-Za-z0-9_.+-]*$/, `${option.flag} is not a flag`);
  }
});

test("git, a command table without an option table", () => {
  const parsed = probe("git", "git");
  assert.equal(parsed.style, "gnu");
  assert.equal(parsed.version, "2.52.0");
  assert.ok(parsed.subcommands.length >= 20, `expected at least 20 subcommands, found ${parsed.subcommands.length}`);
  for (const wanted of ["clone", "commit", "push", "status", "rebase"]) {
    assert.ok(names(parsed.subcommands).includes(wanted), `expected the subcommand ${wanted}`);
  }
  assert.equal(parsed.subcommands.find((entry) => entry.name === "commit").description, "Record changes to the repository");
  // The six line usage lists only flags, and a flag inside a usage is not an
  // option this parser declares, because the usage describes none of them.
  assert.equal(parsed.options.length, 0);
  assert.deepEqual(parsed.positionals, []);
});

test("git commit, a subcommand help joined with its man page", () => {
  const parsed = probe("git-commit", "git");
  assert.equal(parsed.style, "gnu");
  assert.ok(parsed.options.length >= 40, `expected at least 40 flags, found ${parsed.options.length}`);
  assert.equal(parsed.subcommands.length, 0);
  // The usage runs over eight lines and ends with the one positional.
  assert.deepEqual(names(parsed.positionals), ["pathspec"]);
  assert.equal(parsed.positionals[0].variadic, true);

  assert.equal(flag(parsed, "--message").short, "-m");
  assert.equal(flag(parsed, "--message").valueName, "message");
  // git writes --[no-]all, and the flag is the affirmative one.
  assert.equal(flag(parsed, "--all").short, "-a");
  assert.equal(flag(parsed, "--all").takesValue, false);
  // --allow-empty appears in the man page's OPTIONS section and not in the
  // help table, which is the whole reason the man page is read.
  const manOnly = flag(parsed, "--allow-empty");
  assert.ok(manOnly, "expected --allow-empty, which only the man page describes");
  assert.match(manOnly.description, /^Usually recording a commit/);
  assert.equal(flag(parsed, "--unified").type, "number");
});

test("ffmpeg, single dash long options and a usage of its own", () => {
  const parsed = probe("ffmpeg", "ffmpeg");
  assert.equal(parsed.style, "gnu");
  assert.equal(parsed.version, "8.0.1");
  assert.ok(parsed.options.length >= 30, `expected at least 30 flags, found ${parsed.options.length}`);
  assert.equal(parsed.subcommands.length, 0);
  // usage: ffmpeg [options] [[infile options] -i infile]... {[outfile options] outfile}...
  assert.deepEqual(names(parsed.positionals), ["infile", "outfile"]);

  // A single dash long option has no short form to pair with.
  assert.equal(flag(parsed, "-f").takesValue, true);
  assert.equal(flag(parsed, "-f").valueName, "fmt");
  assert.equal(flag(parsed, "-f").short, undefined);
  assert.equal(flag(parsed, "-vn").takesValue, false);
  // -ar[:<stream_spec>] <rate>: the stream qualifier is not the value, and the
  // description column falls between the flag and the value it takes.
  assert.equal(flag(parsed, "-ar").valueName, "rate");
  assert.equal(flag(parsed, "-ar").type, "number");
  assert.equal(flag(parsed, "-ar").description, "set audio sampling rate (in Hz)");
});

test("gh, a cobra command table", () => {
  const parsed = probe("gh", "gh");
  assert.equal(parsed.style, "cobra");
  assert.equal(parsed.version, "2.83.0");
  assert.ok(parsed.subcommands.length >= 25, `expected at least 25 subcommands, found ${parsed.subcommands.length}`);
  for (const wanted of ["auth", "pr", "repo", "issue", "api"]) {
    assert.ok(names(parsed.subcommands).includes(wanted), `expected the subcommand ${wanted}`);
  }
  // The HELP TOPICS section is laid out like a command table and lists no
  // commands, so none of its entries is one.
  for (const topic of ["actions", "exit-codes", "formatting", "mintty"]) {
    assert.ok(!names(parsed.subcommands).includes(topic), `${topic} is a help topic, not a subcommand`);
  }
  assert.equal(parsed.options.length, 2);
  assert.equal(flag(parsed, "--help").description, "Show help for command");
  assert.deepEqual(parsed.positionals, []);
});

test("fd, a clap help whose descriptions sit under their flags", () => {
  const parsed = probe("fd", "fd");
  assert.equal(parsed.style, "clap");
  assert.equal(parsed.version, "10.2.0");
  assert.ok(parsed.options.length >= 40, `expected at least 40 flags, found ${parsed.options.length}`);
  assert.deepEqual(names(parsed.positionals), ["pattern", "path"]);
  // The Arguments section describes both positionals, and the usage says only
  // that they are there.
  assert.match(parsed.positionals[0].description, /^the search pattern/);
  assert.equal(parsed.positionals[1].variadic, true);

  assert.equal(flag(parsed, "--hidden").short, "-H");
  assert.equal(flag(parsed, "--hidden").takesValue, false);
  // -u, --unrestricted... is repeatable, and the ellipsis is not part of the name.
  assert.ok(flag(parsed, "--unrestricted"), "expected --unrestricted without its ellipsis");
  assert.equal(flag(parsed, "--unrestricted").repeatable, true);
  // A description that runs over four lines is one description.
  assert.match(flag(parsed, "--hidden").description, /can be overridden with --no-hidden\.$/);
});

test("python http.server, an argparse help", () => {
  const parsed = probe("http-server", "server.py");
  assert.equal(parsed.style, "argparse");
  assert.equal(parsed.options.length, 5);
  assert.equal(parsed.subcommands.length, 0);
  assert.deepEqual(names(parsed.positionals), ["port"]);
  assert.equal(parsed.positionals[0].description, "bind to this port (default: 8000)");
  assert.equal(parsed.positionals[0].required, false);

  // argparse writes -b ADDRESS, --bind ADDRESS and puts the description on the
  // next line, so neither cut applies and the head is the whole line.
  assert.equal(flag(parsed, "--bind").short, "-b");
  assert.equal(flag(parsed, "--bind").valueName, "ADDRESS");
  assert.equal(flag(parsed, "--bind").description, "bind to this address (default: all interfaces)");
  assert.equal(flag(parsed, "--cgi").takesValue, false);
});

test("a help text in no style at all is read as nothing", () => {
  const parsed = parseHelp({
    binary: "mystery",
    help: "mystery 1.0\nSomething happened. Ask the author what it does.\n",
    version: "mystery 1.0",
  });
  assert.equal(parsed.style, "unknown");
  assert.deepEqual(parsed.options, []);
  assert.deepEqual(parsed.subcommands, []);
  assert.deepEqual(parsed.positionals, []);
  // The version is still read: it came from its own string and not from the
  // help text the parser could make nothing of.
  assert.equal(parsed.version, "1.0");
});

test("an empty call is not a crash", () => {
  const parsed = parseHelp({});
  assert.equal(parsed.style, "unknown");
  assert.equal(parsed.version, null);
  assert.deepEqual(parsed.options, []);
  assert.deepEqual(parseHelp().options, []);
});

test("the overstrike a pager would have consumed is removed", () => {
  // man writes a bold character as the character, a backspace and the character
  // again, and a terminal with no pager in front of it shows exactly that.
  const bold = (text) => [...text].map((character) => `${character}\u0008${character}`).join("");
  const help = [
    "usage: x [options]",
    "",
    bold("OPTIONS"),
    `       ${bold("--allow")}, ${bold("-a")}`,
    "           Allow it.",
    "",
  ].join("\n");
  const parsed = parseHelp({ binary: "x", help });
  assert.equal(parsed.style, "gnu");
  assert.deepEqual(
    parsed.options.map((option) => option.flag),
    ["--allow"],
  );
  assert.equal(parsed.options[0].short, "-a");
  assert.equal(parsed.options[0].description, "Allow it.");
});

test("the same input always gives the same answer", () => {
  const once = probe("jq", "jq");
  const twice = probe("jq", "jq");
  assert.deepEqual(once, twice);
});
