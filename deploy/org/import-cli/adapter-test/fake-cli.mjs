#!/usr/bin/env node
// fake-cli: the command the adapter tests wrap, standing in for the imported
// CLI the same way test/fake-skopeo stands in for skopeo in import-oci.
//
// It is a node script rather than a real tool so that every assertion about
// argv, stdin, files and exit codes is exact on macOS, on Linux and inside
// Alpine, where the coreutils a test could otherwise reach for are busybox
// applets with their own spellings. It is executable and carries a shebang,
// because the adapter's probe runs it as a bare binary.

import { spawn } from "node:child_process";
import { readFileSync, symlinkSync, writeFileSync } from "node:fs";

const argv = process.argv.slice(2);

// Everything the caller sent on standard input, which the dump command reports
// so a test can see that the adapter connected the pipe.
const readStdin = async () => {
  let text = "";
  process.stdin.setEncoding("utf8");
  for await (const chunk of process.stdin) text += chunk;
  return text;
};

if (argv.length === 1 && argv[0] === "--help") {
  process.stdout.write(
    "Usage: fake-cli [OPTIONS] COMMAND\n\nCommands:\n  dump   print the arguments and stdin as JSON\n  upper  write an upper case copy of a file\n",
  );
  process.exit(0);
}

// A CLI with no version flag prints nothing and fails, which is what the probe
// has to report as an empty string rather than as an error.
if (argv.length === 1 && argv[0] === "--version") process.exit(2);

const [command, ...rest] = argv;

switch (command) {
  case "dump": {
    const stdin = await readStdin();
    process.stdout.write(JSON.stringify({ args: rest, stdin, cwd: process.cwd() }));
    break;
  }
  case "upper": {
    // The first argument is a path the adapter wrote from the files input, the
    // second is a name in the working directory the adapter reads back.
    const [source, target] = rest;
    writeFileSync(target, readFileSync(source, "utf8").toUpperCase());
    process.stdout.write(`wrote ${target}\n`);
    break;
  }
  case "flood": {
    // One mebibyte per iteration, which is how a test reaches the output cap
    // without a fixture of its own.
    const chunk = "x".repeat(1024 * 1024);
    for (let i = 0; i < Number(rest[0] ?? 1); i += 1) process.stdout.write(chunk);
    break;
  }
  case "hang": {
    // Never exits, so the adapter's timeout is the only thing that ends it.
    setInterval(() => {}, 1000);
    break;
  }
  case "fork": {
    // The shape that used to wedge the adapter: a command that starts a child
    // of its own, lets it inherit the standard output pipe and exits at once.
    // The pipe stays open after this process is gone, so only killing the
    // whole process group ends the call.
    const grandchild = spawn(process.execPath, [process.argv[1], "hang"], {
      stdio: ["ignore", "inherit", "inherit"],
    });
    // Unreferenced so this process exits at once and leaves the grandchild
    // holding the pipe, which is the whole point of the fixture.
    grandchild.unref();
    process.stdout.write("forked\n");
    break;
  }
  case "symlink": {
    // Writes the output name as a link to a file outside the working
    // directory, which the adapter must refuse to read back.
    const [target, name] = rest;
    symlinkSync(target, name);
    process.stdout.write(`linked ${name}\n`);
    break;
  }
  case "fail": {
    process.stderr.write("fake-cli: the command refused.\n");
    process.exit(3);
  }
  default: {
    process.stderr.write(`fake-cli: unknown command ${command}\n`);
    process.exit(64);
  }
}
