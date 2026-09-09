# Writing a kit

A Package is a folder in Files whose `kitbash.yaml` carries a `deploy` block.
Building it produces an OCI image; running that image is a Process. A kit is a
Package that also implements one of the five lifecycle hooks. There are no
package kinds: what a Package is comes from what its manifest declares.

Write the folder in the member's home first. Move it to `/org` when it works,
which is an approval.

## The manifest

Four blocks, and only the first two are required.

- `name` and `description` make the folder visible. Without them nothing here
  can be read.
- `tags` are for humans to search by. Nothing on the platform reads them.
- `provides` says what the folder offers: `tools`, `prompt`, `subscriptions`,
  `kit`.
- `deploy` says how it runs. Its presence makes the folder a Package.

Every tool declares an input and an output JSON Schema, inline or as a `$ref` to
a `.json` file in the same folder. Composition binds on schemas, never on names.
The normative file is `spec/manifest.schema.json`; a manifest that fails it is
`invalid-manifest` at write time, so the folder never becomes an unreadable one.

## A minimal kit

Three files. Write them with `fs_write`, one commit each.

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
        properties:
          subject: { type: string, minLength: 1 }
      output:
        type: object
        required: [greeting]
        properties:
          greeting: { type: string }

deploy:
  units:
    - type: container
      build: .
      expose: mcp
      limits: { memory: 256Mi }
```

`Dockerfile`:

```dockerfile
# greet: a stdio MCP server. kitbash runs it as PID 1 with stdin held open and
# execs one more instance of this entrypoint per MCP session.
FROM node:22-alpine
WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY kitbash.yaml index.js ./
ENTRYPOINT ["node", "/app/index.js"]
```

`index.js`, with `@modelcontextprotocol/sdk` pinned in `package.json`:

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

Nothing but MCP goes to stdout. A log line goes to stderr, which is what
`proc_logs` reads.

## How the tools appear

A Process with `expose: mcp` puts every tool its manifest declares on the
owner's surface as `<package>_<tool>`, so `greet` gives `greet_hello`. Package
names carry no underscore, so the first underscore is the split.

- The published schemas are the manifest's, and input is validated against them
  before the call reaches the container.
- A tool the server offers but the manifest does not declare is not published.
- A package named after a family (`fs`, `pkg`, `proc`, `tel`, `users`,
  `approvals`) is refused at `proc_run`.
- A tool name another running Process already answers is a conflict: the Process
  runs, its tools stay off the surface.

Results come back in two shapes. A tool that answers with text only has its
content passed through as it came. A tool that answers with structured content
has it validated against the manifest's output schema, and output that does not
match is an `internal` problem naming the Package. Answer with both, as the
example does, and every caller is served.

## The five hooks

A kit declares hooks in `provides.kit`. kitbashd knows the hook contract and
nothing about any installed kit.

| Hook | What the kit declares |
|------|----------------------|
| import | `kit: [import]` and a tool named `import` whose input `source` is a string with a `pattern`. `pkg_import` picks the running kit whose schema accepts the source, calls it, and writes the files it returns as one commit |
| observe | `kit: [observe]`, `subscriptions: [telemetry]`, `expose: http` and a `port`. kitbashd POSTs every stored record to the Process as OTLP/HTTP JSON on `/v1/traces`, `/v1/logs` and `/v1/metrics` |
| evaluate | `kit: [evaluate]`, usually with the observe declarations. The kit writes judgments back with `kitbash.eval: true` and `kitbash.subject.trace_id` and `kitbash.subject.span_id` |
| build | `kit: [build]` and a tool `build` with input `{path, context}` and output `{digest, log}`. Declared, not dispatched: the built in OCI path builds every Package |
| run | `kit: [run]` and a tool `run` with input `{package, digest, name, unit}` and output `{id, state, endpoint}`. Declared, not dispatched: the built in rootless podman runner runs every Process |

An import kit's `source` pattern is the route, so make it exact. Two running
kits that accept the same source is a conflict the member resolves by stopping
one.

Every fan out request carries `Authorization: Bearer` with the secret in
`KITBASH_FANOUT_SECRET`. Check it and answer 401 otherwise, or anything else on
the host that reaches the port can feed the kit records. Delivery is best effort
with a bounded queue: treat the push as a wake up and `tel_query` as the truth.

An evaluation kit run by an admin may claim the subject's `user`, `package`,
`process` and `path`, so a judgment is found by the same query as its subject. A
member's kit has those four stamped as its own. `kitbash.producer` always names
the kit's Process, so a judgment is never mistaken for the act it judges.

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
subscriber among them accepts what arrives because it was given no secret to
check.

`KITBASH_MCP_ENDPOINT` is MCP over streamable HTTP, `Authorization: Bearer` with
the Process token. Through it a Process is an agent with exactly its owner's
surface: Files, Packages, Processes, Telemetry and the tools of the owner's
other Processes. Open a session with an `initialize` POST; anything else without
a session is `bad-request`. At most 8 sessions live per Process, and a session
idle for 10 minutes ends. Every span such a session records carries
`kitbash.caller`, so what a kit did for its owner is one query.

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
path outside the prefixes is refused with `not-permitted`.

## Errors

Answer a failure as RFC 9457 problem details in one text block with
`isError: true`: `type` (`https://kitbash.zyx.tw/errors/<slug>`), `title`,
`status`, `detail`, `instance` and a `fix`. A problem of a kitbash type reaches
the agent unchanged, so a kit's `not-found` stays a `not-found`. Any other error
text is wrapped as `bad-request` with that text as the detail. Never answer a
bare string, and always write a `fix` that says what to do next.

## Limits

- A manifest is at most 64 KiB. A file read or written through `fs` is at most
  1 MiB.
- A description, of a folder or of a tool, is at most 280 characters.
- `limits.cpu` and `limits.memory` are passed to the container runtime and
  recorded, and not enforced until kitbashd runs Processes in delegated cgroups.
  Declare them anyway: the manifest is the declared state.
- `health` is recorded, not probed. Liveness is PID 1 of the container.
- A member holds at most 64 registered Processes and 64 pending approvals.
- A body on the Process receiver is capped at 4 MiB.

## Test before building

Every seeded kit ships a driver under `test/` that runs the kit as a plain
process against fakes: a fake kitbashd receiver for a subscriber, a fake MCP
surface for a kit that calls tools. Copy that shape and run it with
`bun run test/greet.test.mjs`. A driver finds a wrong schema or a missing header
in a second, where a container build and a Process start take a minute.

## The build and run loop

1. `fs_write` the manifest, the Dockerfile and the code, one commit each.
2. `pkg_build` with the folder path. It builds from the last commit, so an
   uncommitted change is a `conflict`. The answer carries the digest and the
   tail of the build log.
3. `proc_run` with the package path. The same name and digest returns the
   running Process, a new digest replaces it. The answer lists the tools that
   joined the surface.
4. Call the tool, here `greet_hello` with `{"subject": "world"}`.
5. `proc_logs` for the raw stream, `tel_query` for the spans. Every call is one
   span carrying `kitbash.user`, `kitbash.package`, `kitbash.process` and
   `kitbash.tool`.

Repeating any of these is safe. They describe a state, they do not add one.

## The seeded kits

They are under `/org`, and each one is a worked example.

- `import-mcp`: an import kit that wraps an npm MCP server. A pattern routed
  `source`, files back to `pkg_import`.
- `import-oci`: an import kit that wraps an OCI image pinned by digest.
- `observe-count`: the smallest observe kit. It counts the fan out and writes
  the totals back as metrics.
- `evaluate-latency`: an observe and evaluate kit. It judges slow spans and
  writes the judgment back against the subject.
- `workflow`: a Package that calls the owner's tools through
  `KITBASH_MCP_ENDPOINT`. Its `WORKFLOW.md` is what a `prompt` entry is for.
