# Writing a kit

A Package is a folder in Files whose `kitbash.yaml` carries a `deploy` block.
Building it produces an OCI image; running that image is a Process. A kit is a
Package that also implements one of the five lifecycle hooks. There are no
package kinds: what a Package is comes from what its manifest declares. Write
the folder in the member's home first, and move it to `/org` when it works,
which is an approval.

## The manifest

Four blocks, and only `name` and `description` are required.

- `name` and `description` make the folder visible. Without them nothing here
  can be read.
- `tags` are optional and are for humans to search by. Nothing on the platform
  reads them.
- `provides` says what the folder offers: `tools`, `prompt`, `subscriptions`
  and `kit`. `deploy` says how it runs, and makes the folder a Package.

Every tool declares an input and an output JSON Schema, inline or as a `$ref` to
a `.json` file in the same folder. Composition binds on schemas, never on names.
A manifest that fails `spec/manifest.schema.json` is `invalid-manifest` at write
time, so a folder never becomes an unreadable one.

## A minimal kit

Four files. Write them with `fs_write`, one commit each.

`kitbash.yaml`:

```yaml
name: greet
description: Greets a subject by name. Use it to check that a Package's tools reach the surface.
tags: [example]
provides:
  tools:
    - name: hello
      description: Greet the subject and return the greeting as text and as structured output.
      input:
        type: object
        additionalProperties: false
        required: [subject]
        properties: { subject: { type: string, minLength: 1 } }
      output:
        type: object
        required: [greeting]
        properties: { greeting: { type: string } }

deploy:
  units:
    - type: container
      build: .
      expose: mcp
      limits: { memory: 256Mi }
```

`Dockerfile`:

```dockerfile
# greet: a stdio MCP server, run as PID 1 with stdin held open. One more
# instance of this entrypoint is exec'd per MCP session.
FROM node:22-alpine
WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY kitbash.yaml index.js ./
ENTRYPOINT ["node", "/app/index.js"]
```

`package.json`, pinning the one dependency. `"type": "module"` is what makes the
`import` lines below load:

```json
{ "name": "greet", "version": "0.1.0", "private": true, "type": "module",
  "main": "index.js", "dependencies": { "@modelcontextprotocol/sdk": "1.30.0" } }
```

`index.js`:

```js
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";

const input = { type: "object", required: ["subject"], properties: { subject: { type: "string" } } };
const server = new Server({ name: "greet", version: "0.1.0" }, { capabilities: { tools: {} } });

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [{ name: "hello", description: "Greet the subject.", inputSchema: input }],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const greeting = `Hello ${request.params.arguments.subject}`;
  return { content: [{ type: "text", text: greeting }], structuredContent: { greeting } };
});

await server.connect(new StdioServerTransport());
console.error("[greet] ready");
```

Nothing but MCP goes to stdout. A log line goes to stderr, which `proc_logs`
reads.

## How the tools appear

A Process with `expose: mcp` puts every tool its manifest declares on the
owner's surface as `<package>_<tool>`, so `greet` gives `greet_hello`. A package
name carries no underscore, so the first underscore is the split.

- The published schemas are the manifest's, and input is validated against them
  before the call reaches the container.
- A tool the server offers but the manifest does not declare is not published.
- A package named after a built in family is refused at `proc_run`.
- A tool name another running Process already answers is skipped rather than
  published. The Process runs and its other tools join the surface; only the
  clashing name stays off it.

Results come back in two shapes. Text only content passes through as it came.
Structured content is validated against the manifest's output schema, and output
that does not match is an `internal` problem naming the Package. Answer with
both, as the example does, and every caller is served.

## The five hooks

A kit declares hooks in `provides.kit`. kitbashd knows the hook contract and
nothing about any kit that implements it.

| Hook | What the kit declares |
|------|----------------------|
| import | `kit: [import]` and a tool named `import` whose input `source` is a string with a `pattern`. `pkg_import` picks the running kit whose schema accepts the source, calls it, and writes the files it returns as one commit |
| observe | `kit: [observe]`, `subscriptions: [telemetry]`, `expose: http` and a `port`. kitbashd POSTs every stored record to the Process as OTLP/HTTP JSON on `/v1/traces`, `/v1/logs` and `/v1/metrics` |
| evaluate | `kit: [evaluate]`, usually with the observe declarations. The kit writes judgments back with `kitbash.eval: true` and `kitbash.subject.trace_id` and `kitbash.subject.span_id` |
| build | `kit: [build]` and a tool `build` with input `{path, context}` and output `{digest, log}`. `pkg_build` calls it for a unit whose `builder` names this Package folder |
| run | `kit: [run]` and a tool `run` with input `{package, digest, name, unit}` and output `{id, state, endpoint}`, and optionally `stop` with input `{id}`. `proc_run` calls it for a unit whose `runner` names this Package folder |

An import kit's `source` pattern is the route, so make it exact. Two running
kits accepting the same source is a conflict, resolved by stopping one.

A build or run kit is named rather than chosen: the Package that wants it
writes `builder: /org/nix-build` or `runner: /org/pve-runner` beside its `build`
context, and the kit has to be running as a Process of the caller when the tool
is called. A kit that is not running is `not-found`, and a kit whose tool schema
does not accept what the hook is called with is `invalid-manifest`. A Process a
run kit started is the kit's: kitbashd registers it, lists it with its runner,
and never starts, stops or restores it, so `proc_stop` forwards to the kit's
`stop` tool and answers `not-permitted` when the kit declares none. The id the
`run` tool answers with is the Process id, so it is a UUIDv7.

Every fan out request carries `Authorization: Bearer` with the secret in
`KITBASH_FANOUT_SECRET`. Check it and answer 401 otherwise, or anything else on
the host that reaches the port can feed the kit records. Delivery is best effort
with a bounded queue: treat the push as a wake up and `tel_query` as the truth.

An evaluation kit run by an admin may claim the subject's `user`, `package`,
`process` and `path`, so a judgment is found by the same query as its subject. A
member's kit has those four stamped as its own, and `kitbash.producer` always
names the kit's Process.

## The environment a Process receives

kitbashd registers a Process before its container starts and gives it:

| Variable | What it is |
|----------|-----------|
| `KITBASH_TELEMETRY_ENDPOINT` | `http://host.containers.internal:4318`, the Process receiver |
| `KITBASH_TELEMETRY_TOKEN` | This Process's token. It names one Process and is revoked when it stops |
| `KITBASH_PROCESS` | The Process id |
| `KITBASH_PACKAGE` | The Package path |
| `KITBASH_USER` | The member who ran it |
| `KITBASH_MCP_ENDPOINT` | `http://host.containers.internal:4318/mcp` |
| `KITBASH_FANOUT_SECRET` | The bearer every fan out request to this Process carries |

A manifest that declares any of these names is not honoured. A Process started
while kitbashd was unreachable is given none of them: it exports nothing, and a
subscriber among them accepts what arrives because it has no secret to check.

`KITBASH_MCP_ENDPOINT` is MCP over streamable HTTP, `Authorization: Bearer` with
the Process token. Through it a Process is an agent with exactly its owner's
surface: Files, Packages, Processes, Telemetry and the tools of the owner's
other Processes. Open a session with an `initialize` POST; anything else without
a session is `bad-request`. At most 8 sessions live per Process, and one idle
for 10 minutes ends. Every span such a session records carries `kitbash.caller`,
so what a kit did for its owner is one query.

## permits

A concurrent change adds `provides.permits` to the manifest. A kit says what it
needs and gets that and no more when it calls through `/mcp`:

```yaml
provides:
  permits:
    tools: [fs_read, fs_list, "*_*"]
    paths: [/org, /home/*]
```

`tools` are globs over surface tool names, `paths` are Files prefixes. A Process
with no `permits` block gets an empty surface through `/mcp`, so a kit that
calls anything must declare it. The permits travel with the registration, and a
path outside the prefixes is refused with `not-permitted`. Until that change
lands the schema refuses the block, so a manifest carrying it is
`invalid-manifest`: write it when this section stops saying so.

## Errors

Answer a failure as RFC 9457 problem details in one text block with
`isError: true`: `type` (`https://kitbash.zyx.tw/errors/<slug>`), `title`,
`status`, `detail`, `instance` and a `fix`. A problem of a kitbash type reaches
the agent unchanged, so a kit's `not-found` stays a `not-found`; any other error
text is wrapped as `bad-request`. Never answer a bare string, and always write a
`fix` that says what to do next.

## Limits

- 64 KiB a manifest, 1 MiB a file through `fs`, 4 MiB a receiver body.
- A description, of a folder or of a tool, is at most 280 characters.
- `limits.cpu` and `limits.memory` are passed to the container runtime and
  recorded, and not enforced until kitbashd runs Processes in delegated cgroups.
  Declare them anyway: the manifest is the declared state.
- `health.http` is probed: kitbashd requests that path on the Process's endpoint every `health.interval` (30s by default, 5s at the fastest) and writes one `kitbash.health` metric record per probe, 1 for healthy and 0 for unhealthy. It restarts nothing. `health.exec` is recorded, not run. Liveness is still PID 1 of the container.
- A member holds at most 64 registered Processes and 64 pending approvals.

## Test before building

Four of the five seeded Packages ship a driver under `test/` that runs the
Package as a plain process against fakes: a fake kitbashd receiver for a
subscriber, a fake MCP surface for one that calls tools. Copy that shape. A
driver finds a wrong schema or a missing header in a second, where a build and a
Process start take a minute. Run it on the author's own machine with
`bun run test/greet.test.mjs`, not here: this host has no node and no bun, and
the surface has no tool that runs a command.

## The build and run loop

1. `fs_write` the manifest, the Dockerfile, the `package.json` and the code, one
   commit each.
2. `pkg_build` with the folder path. It builds from the last commit, so an
   uncommitted change is a `conflict`. The answer carries the digest and the
   tail of the build log.
3. `proc_run` with the package path. The same name and digest returns the
   running Process, a new digest replaces it, and the answer lists the tools
   that joined the surface.
4. Call the tool, here `greet_hello` with `{"subject": "world"}`.
5. `proc_logs` for the raw stream, `tel_query` for the spans, each carrying
   `kitbash.user`, `kitbash.package`, `kitbash.process` and `kitbash.tool`.

Repeating any of these is safe. They describe a state, they do not add one.

## The seeded Packages

Worked examples under `/org`. Four declare a kit hook; `workflow` declares none.

- `import-mcp`: an import kit for an npm MCP server. A pattern routed `source`.
- `import-oci`: an import kit for an OCI image pinned by digest.
- `observe-count`: the smallest observe kit. It counts the fan out.
- `evaluate-latency`: observe and evaluate. It judges slow spans.
- `workflow`: it calls the owner's tools through `KITBASH_MCP_ENDPOINT`, and its
  `WORKFLOW.md` is what a `prompt` entry is for.
