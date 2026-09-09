# kitbash product plan

> An operating system for AI agents. Files, Packages, Processes, Telemetry. Nothing built for a human at a terminal.

This is the single living document for the product. It replaces a design doc, an architecture doc and a roadmap. When a decision changes, this file changes. Status: draft v0.7, 2026-09-08.

## 1. Positioning

### 1.1 What it is

kitbash is a Linux appliance that an organization installs on one machine. Every member gets a user. Every user connects an AI agent to the machine over SSH and gets an MCP endpoint that exposes the whole system. Through that endpoint the agent reads and writes files, builds and installs packages, runs them, and inspects how they behave.

The human is not the user of the system. The agent is. The human tells the agent what they want and approves what needs approving.

### 1.2 One sentence

The agent writes source into Files, builds it into a Package, runs it as a Process, and everything it does lands in Telemetry.

### 1.3 The thesis

A general purpose Linux is roughly 80 percent interface for a human sitting at a terminal: shells, ttys, passwords, man pages, text config files, table shaped command output. An agent needs none of it. It needs structured operations with schemas, identity by key, state described before it is expanded, telemetry by default, and idempotent operations that converge to a declared state.

kitbash is not Linux with an MCP server bolted on. It is Linux with the human layer removed and an agent layer put in its place.

### 1.4 Who it is for

Organizations where several people run coding agents and keep re-solving the same problems by hand: where does shared knowledge live, how does an agent install a tool safely, how does a tool built by one person become usable by another, how do we know what agents are actually doing. A team of 3 to 50 people with one machine is the first target.

### 1.5 What it is not

- Not a workflow platform. Workflows are one installable kit, not the core.
- Not a knowledge base or search engine. Search and embeddings are installable packages, not the core.
- Not a multi-tenant SaaS. One machine is one organization. Linux users and groups are the tenancy model.
- Not a human UI. Version 1 has no web interface. A human interacts through their agent.

## 2. Object model

There are exactly four objects. New capability is added as a kit that operates on these four, never as a fifth object.

| Object | What it is | Backed by | Linux analogue |
|--------|-----------|-----------|----------------|
| Files | Any folder or file, versioned | git | filesystem |
| Packages | A runnable unit built from a folder with a manifest | OCI image | package / bundle |
| Processes | A running Package | container | process |
| Telemetry | Traces, metrics and logs from everything above | OTLP | journal |

### 2.1 Files

Files is the filesystem, and every top level folder is a git repository. Two roots exist:

- `/org`: owned by root, group `kitbash-admin`, readable by every user, writable by admins. Shared handbooks, plans, meeting notes, org wide packages. Folders are setgid so what an admin adds stays admin writable, and every repository is `core.sharedRepository=group`.
- `/home/<user>`: owned by the user. Private files, private packages.

Sharing between users uses Linux groups on folders. There is no permission system beyond what the kernel already enforces.

**Writes to `/org` by a member are approvals.** When a member's `fs_write` or `pkg_import` names a path under `/org`, the surface does not fail: it queues the call in kitbashd and answers with a `queued` problem (status 202) carrying the approval id. An admin lists the queue, and `approvals_approve` runs the queued call in the admin's session as the admin's Linux user, with the commit authored by the requester and a trailer naming the admin who approved. The result is stored on the approval so the requester reads it back with `approvals_list`. `approvals_reject` stores the reason instead. Only these two tools queue in version 1; everything else outside the caller's space is simply not permitted.

**Progressive disclosure is the reading model.** A folder is visible to an agent only if it carries a `kitbash.yaml` with a `name` and a `description`. The agent sees folder descriptions first, decides whether to enter, and only then sees file names and contents. A folder without a manifest does not exist as far as the MCP surface is concerned. This rule is what keeps Files from becoming a dump.

Any file type is a first class citizen. Markdown returns text. PNG and JPEG return image content. PDF returns page images or extracted text. The core stores blobs and knows nothing about their meaning.

Every commit is a version. Files has no separate version object.

### 2.2 Packages

A Package is a folder in Files whose manifest carries a `deploy` section. Building it produces an OCI image. The image digest is the Package version, and the build records which Files commit it came from. The chain commit to digest to process is the entire provenance story and needs no extra object.

The digest of a locally built image is its OCI image ID, the sha256 of the image configuration. The build stamps the image with labels `kitbash.path`, `kitbash.name`, `kitbash.commit` and `kitbash.user`, so the image store is the build history and no second record is kept. Packages a member builds live in that member's image store; sharing a built image between members is M5 work.

There are no package kinds. A skill, a tool server, a workflow engine and an observability kit are all Packages. What distinguishes them is what their manifest declares, not a category the platform maintains. Tags exist for humans to search by. Platform logic never reads tags.

Packages can also be imported rather than built: an existing OCI image, an existing MCP server from npm or PyPI, or a plain CLI tool. Import is done by kits, see section 3. The host never installs third party software. Everything third party arrives as a Package.

### 2.3 Processes

A Process is a Package running as a rootless container under the user who started it. kitbashd supervises Processes directly: restart policy, boot restore, health, resource limits. There is no dependency on systemd user sessions. Boot restore works from the registrations kitbashd holds: at start it creates each owner's runtime directory and starts each registered container as its owner, so every Process that was running before a reboot is running after it, with the token it already had.

A Process declares how it is exposed:

- `mcp`: its tools appear on the user's MCP surface under the package namespace, for example `ffmpeg.transcode`.
- `http`: it gets an internal port, optionally a hostname through the reverse proxy.
- `none`: a batch job or a subscriber that only talks to Telemetry.

**How `mcp` exposure works.** The image's entrypoint is a stdio MCP server. The Process runs it as PID 1 with stdin held open, which keeps the container alive and is the liveness signal. Every MCP session the owner opens execs one more instance of the same entrypoint inside the container and proxies calls to it, the same way an agent runs a stdio server on a laptop. The tools the surface publishes are the ones the manifest declares, with the manifest's schemas, so input is validated against the manifest before it reaches the container. A tool the server offers but the manifest does not declare is not on the surface. Tool names are `<package>_<tool>`; a Package whose name collides with a built in family (`fs`, `pkg`, `proc`, `tel`, `users`, `approvals`) cannot be run.

Every Process carries labels `kitbash.id`, `kitbash.user`, `kitbash.package`, `kitbash.name` and `kitbash.digest`. The container runtime holds the Process state and kitbashd reads it back; there is no second record.

**Every Process can be an agent.** The same MCP surface a member reaches over SSH is reachable from inside a Process: kitbashd serves MCP over streamable HTTP at `/mcp` on the Process receiver, authenticated by the Process token, and answers each session by running `kitbash-mcp` as the Process's owner and relaying to it. A Process therefore sees exactly what its owner sees, Files, Packages, Processes, Telemetry and the tools of the owner's other Processes, under the kernel's rules, with no code path of its own. Every span such a session records carries `kitbash.caller`, the calling Process id, so what a Process did on its owner's behalf is one query. The endpoint is given to every container as `KITBASH_MCP_ENDPOINT`.

**What a Process may call.** A Process sees its owner's surface narrowed to what its Package declared in `provides.permits`, a list of tool name globs and a list of absolute path prefixes. A Process whose Package declares no `permits` block gets an empty surface over `/mcp`: the block is the declaration, not a filter over a surface the kit would otherwise have, so an admin's kit is not an admin by default. The permits travel with the registration, kitbashd hands them to the session's `kitbash-mcp` as `KITBASH_PERMITS`, and that process publishes only the tools a glob names, built in and Package tools alike, and refuses a call outside the permitted names, or one whose path or Package argument is outside every prefix, with `not-permitted` before the handler runs. A prefix covers the path itself and everything below it and may carry `*` as one whole component, as in `/home/*/flows`; a Package that permits tools and no prefix may call nothing that takes a path. `pkg_inspect` shows the block, because what a kit may do is part of reading it. None of this narrows a member's own SSH session, which is the kernel's business as before.

### 2.4 Telemetry

Telemetry is an OTLP receiver and a store. Every span, metric point and log record must carry four attributes: `user`, `package`, `process`, and where relevant `path`. Filtering by `user` answers what a person did. Filtering by `package` answers how a tool behaves. No filter is the whole machine. These are three views of one store, not three stores.

On the wire the four attributes are namespaced as the OpenTelemetry conventions require: `kitbash.user`, `kitbash.package`, `kitbash.process`, `kitbash.path`, plus `kitbash.tool` for the surface tool a span records and `kitbash.eval` for judgments written back by kits. The query surface speaks the short names. `kitbash.user` is never trusted from a producer: kitbashd stamps it from the peer credentials of the connection that delivered the record.

**Producers.** kitbash-mcp opens one span per `tools/call` (built in and Package tools alike), a child span per build and per Process start, and one log record per build carrying the build log tail. It exports OTLP/HTTP over the kitbashd unix socket. Nothing on the surface is untraced: the span is opened by middleware before any handler runs.

From M4 every Process is a producer too. kitbashd also listens for OTLP/HTTP on the host address containers reach, and each Process is started with `KITBASH_TELEMETRY_ENDPOINT`, `KITBASH_TELEMETRY_TOKEN`, `KITBASH_PROCESS`, `KITBASH_PACKAGE`, `KITBASH_USER` and `KITBASH_FANOUT_SECRET` in its environment. The token is minted by kitbashd when the Process is registered at `proc_run` and revoked when it stops; it names exactly one Process, and kitbashd stamps `kitbash.user`, `kitbash.package` and `kitbash.process` from it. A Process that never reads the token is simply an untraced producer of nothing; its lifecycle is still traced by kitbash-mcp.

Every record also carries `kitbash.producer`, stamped by kitbashd: the member for a session record, the Process id for a Process record. `kitbash.user` is the member the record is about. The two differ only for evaluation records, see below.

**Fan out.** A Process whose manifest declares `provides.subscriptions: [telemetry]`, `expose: http` and a `port` receives every record kitbashd stores, as OTLP/HTTP JSON POSTed to the standard paths on its endpoint, after the record is stored and stamped. Delivery is best effort and asynchronous with a bounded queue per subscriber; a subscriber that is down loses records and the gap is logged, the store is the source of truth. Every delivery carries a bearer secret minted for that Process at registration and given to its container, so a subscriber knows the records came from kitbashd and not from a neighbour on the host. A subscriber run by an admin receives the whole machine. A subscriber run by a member receives that member's records only, the same rule as `tel_query`.

**Evaluation.** An evaluation kit is a subscriber that writes judgments back through the Process receiver with `kitbash.eval: true`. A judgment is about a subject, named by `kitbash.subject.trace_id` and `kitbash.subject.span_id`, and it carries the subject's `kitbash.user`, `kitbash.package`, `kitbash.process` and `kitbash.path` so that the query that asks what a Package did also returns how it was judged. kitbashd accepts those claims on an evaluation record only when the producing Process was run by an admin; a member's evaluation kit has them forced to the member's own. `kitbash.producer` always names the kit's Process, so a judgment is never mistaken for the act it judges.

**Reading.** `tel_query` returns records of one signal filtered by the four attributes and a time range. A member reads their own records; an admin reads everyone's. `tel_retention` reads the window per signal and lets an admin set it.

Evaluation is not an object. An evaluation kit subscribes to Telemetry, computes whatever it computes, and writes the result back as Telemetry with an `eval` attribute. Asking how a package version is doing is the same query as asking what it did.

Telemetry is the only object that grows without bound, so retention lives in the core: a default window per signal, configurable by admins, enforced by kitbashd. Defaults: traces 30 days, logs 14 days, metrics 30 days. kitbashd sweeps once an hour and on start.

### 2.5 The manifest

One file format serves both folder metadata and package definition. The normative definition is [`spec/manifest.schema.json`](spec/manifest.schema.json); the MCP tools that operate on these objects are defined in [`spec/mcp-surface.yaml`](spec/mcp-surface.yaml). This section is the readable summary. A folder with only the first block is visible knowledge. A folder with all blocks is a Package.

```yaml
# kitbash.yaml
name: ffmpeg
description: Transcode and probe media files. Use for any audio or video conversion.
tags: [media, cli]

provides:
  tools:
    - name: transcode
      description: Convert a media file to another container or codec.
      input: { $ref: schemas/transcode.in.json }
      output: { $ref: schemas/transcode.out.json }
  prompt: SKILL.md            # optional, content an agent should read before using the tools
  subscriptions: [telemetry]  # optional, this package wants the OTLP fan out
  kit: [import]               # optional, this package implements a kit hook, see section 3
  permits:                    # optional, what a Process of this package may call over /mcp
    tools: [fs_read, fs_list] # tool name globs; nothing declared is nothing permitted
    paths: [/org, /home/*]    # absolute path prefixes, * as one whole component

deploy:
  units:
    - type: container
      build: .                # or image: docker.io/jrottenberg/ffmpeg@sha256:...
      expose: mcp
      health: { exec: ["ffmpeg", "-version"] }
      limits: { cpu: "1", memory: "512Mi" }
```

Rules the core enforces on every manifest:

1. `name` and `description` are required for visibility.
2. Every tool has an input and an output JSON schema. Composition binds on schemas, never on names or tags.
3. `deploy.units[].type` is `container` or `files`. There is no third type.
4. A `build` unit must point inside the same folder. A Package cannot reach outside its own tree at build time.

### 2.6 Invariants

- Four objects. A proposal that needs a fifth is a kit or is rejected.
- The host is immutable. Third party software is a Package or does not exist.
- Everything an agent does through the MCP surface is Telemetry. There is no untraced path.
- Every operation is idempotent and describes the desired state. Repeating a call is safe.
- Errors are structured: cause, effect, and a suggested fix. Never a bare string.

### 2.7 Standards

kitbash adopts existing specifications wherever one exists. Nothing in this list is invented here.

| Specification | Where it binds | Note |
|---------------|----------------|------|
| OCI image, runtime and distribution specs | Packages, Processes | A Package version is an OCI digest |
| OpenTelemetry semantic conventions and W3C Trace Context | Telemetry | Attribute names follow the conventions; `traceparent` crosses process boundaries. kitbashd and kitbash-mcp speak OTLP/HTTP with the protocol's message definitions compiled in and nothing of the gRPC stack, because neither ever makes an RPC |
| Model Context Protocol, 2025-06 revision | The MCP surface | Files map onto MCP primitives: binary content as resources, `prompt` entries as prompts, everything else as tools |
| JSON Schema 2020-12 | Tool input and output in the manifest | Composition binds on schemas, so the draft is pinned |
| RFC 9457 Problem Details | Every error | Structured errors are `type`, `title`, `detail`, `instance`, plus a `fix` extension |
| RFC 3339 timestamps, UUIDv7 identifiers | Everywhere | Sortable, unambiguous |
| Semantic Versioning, git SHA | kitbashd releases, Files versions | Packages use the OCI digest |

Adopted from M4 onward, when Packages start crossing trust boundaries: SLSA provenance with in-toto attestations for builds, Sigstore signatures on images, an SPDX or CycloneDX SBOM per Package, and CloudEvents as the envelope for the Telemetry fan out.

Deliberately not followed: the Filesystem Hierarchy Standard and the Linux Standard Base. Both describe a Linux for humans. `/org` is not an FHS path and does not need to be.

## 3. Kits

A kit is a Package that implements one or more of the five hooks in the lifecycle:

| Hook | Question it answers | Examples |
|------|--------------------|----------|
| import | How does something from outside become a Package | import-oci, import-mcp, import-cli |
| build | How does a folder become an image | the built in buildah path; a Nix or Bazel builder later |
| run | Where does a Process execute | the built in rootless podman runner; a k8s or PVE runner later |
| observe | Who consumes the Telemetry fan out | an observability kit such as sensorium |
| evaluate | Who writes judgments back into Telemetry | an LLM judge, a statistical drift detector |

A kit installs the same way as any Package and is versioned, traced and removable the same way. kitbashd has no special knowledge of any installed kit. It only knows the hook contract.

**Built in versus installed.** Exactly two things are built into kitbashd because the system cannot boot without them: the OCI build path and the rootless podman runner. Everything else, including the workflow engine and every import kit, is installed. This boundary is fixed. Adding a third built in requires changing this document first.

**Workflows are a kit.** A workflow engine is a Package that reads graph files from Files, calls tools on other Processes, and emits Telemetry, all through the MCP surface every Process can reach. The core does not know what a workflow is: the graph format, the templating between steps and the way one workflow names another are the engine's contract, published in its manifest and its prompt file, never in this document or the specs. A workflow that references another workflow is the engine's concern, resolved by schema compatibility of inputs and outputs.

**The contract, hook by hook.** A kit talks to kitbashd the way every Package does: through its manifest, its MCP tools and the Telemetry receiver. There is no plugin API.

| Hook | The kit declares | kitbashd does |
|------|------------------|---------------|
| import | `kit: [import]` and a tool `import` whose `source` pattern is the route | `pkg_import` picks the kit whose schema accepts the source and writes the files it returns |
| build | `kit: [build]` and a tool `build` with input `{path, context}` and output `{digest, log}` | declared for version 1; the built in OCI path builds every Package until a manifest can name a builder |
| run | `kit: [run]` and a tool `run` with input `{package, digest, name, unit}` and output `{id, state, endpoint}` | declared for version 1; the built in rootless podman runner runs every Process until a manifest can name a runner |
| observe | `kit: [observe]`, `subscriptions: [telemetry]`, `expose: http`, `port` | POSTs every stored record to the Process as OTLP/HTTP JSON, see 2.4 Fan out |
| evaluate | `kit: [evaluate]` and usually the observe declarations as well | accepts records with `kitbash.eval: true` and subject attributes through the Process receiver, see 2.4 Evaluation |

Build and run dispatch to an installed kit is the one part of the contract version 1 declares without exercising: nothing needs a second builder or runner yet, and the manifest field that would name one is not added until something does.

**The import hook.** An import kit is a running Process whose manifest declares `provides.kit: [import]` and a tool named `import`. The tool's input schema has a `source` string, constrained by a pattern to the source syntax the kit understands, and its output is a list of files, each a relative path with text or base64 content. `pkg_import` walks the caller's running kits, picks the one whose `import` input schema accepts the source string, calls the tool, and writes the returned files into the target folder as one commit. The choice binds on schemas, not on a registry of kit names. Two kits accepting the same source is a conflict the caller resolves by stopping one.

**Import kits are the package manager.** A human runs `apt install ffmpeg` and reads `--help`. An agent asks import-cli to wrap ffmpeg, gets a Package with tools that carry schemas, and calls `ffmpeg.transcode` with a structured result. import-mcp is nearly automatic because MCP servers already declare tool schemas. import-cli is the hard one: it drafts a manifest from `--help` and man pages, runs the tool to validate the schema, and only then admits the Package. That validation loop is work an agent can do itself.

## 4. kitbashOS

### 4.1 Base

Alpine Linux plus one daemon, `kitbashd`, written in Go and shipped as a static binary in an apk. The apk ships two binaries: `kitbashd`, the resident daemon OpenRC starts at boot, and `kitbash-mcp`, the per session process sshd starts for each member. From M3 kitbashd is the OTLP receiver and the Telemetry store. The container runtime still supervises Processes on kitbashd's behalf between boots; from M5 kitbashd restores every registered Process at boot, holds the approval queue and creates members. Alpine is chosen for its appliance lineage, its 8 MB root filesystem and its package manager. Go is chosen because a static binary has no musl versus glibc problem and no runtime to install.

Proxmox VE is the precedent for the packaging model: a standard base distribution plus one package that turns it into the appliance. An installable ISO or a LinuxKit image can come later without changing kitbashd.

### 4.2 What is on the host

Seven components at most. Five are existing software, and the seventh is not installed today.

1. Linux kernel
2. OpenRC as minimal init
3. sshd, used only as the MCP transport
4. rootless podman with cgroups v2 and subuid ranges configured
5. git
6. kitbashd, which is also the OTLP receiver
7. OpenTelemetry collector, only if a Process ever needs a protocol kitbashd does not speak; Alpine ships none and M3 installs none

### 4.3 What is removed

Everything that exists only because a human sits at a terminal.

- Interactive shells for regular users, ttys, getty, login banners, motd.
- Passwords, sudo, su. Identity is an SSH key. Privilege escalation is an approval, not a prompt.
- man pages, locales, pagers, editors, tab completion, colored output.
- Human shaped command output. Every operation returns JSON with a schema.
- Hand edited config files, cron syntax, and package manager CLIs for users. State is declared through the MCP surface and kitbashd converges to it.

### 4.4 What changes shape

| Human version | Agent version |
|---------------|---------------|
| `ls` | Folder names and descriptions first, contents on request |
| Reading a log file | A Telemetry query with `user` and `package` attributes |
| A sequence of commands | A declared desired state that kitbashd converges to, safe to repeat |
| sudo and a password | An operation queued for approval by an admin user or an authorized agent |
| An error string | A structured error with cause, effect and suggested fix |

### 4.5 Identity and transport

Every organization member is a Linux user with an SSH public key. Their agent configures one MCP server:

```
ssh alice@kitbash.example.org
```

sshd applies `ForceCommand kitbash-mcp` to every regular user. The connection is an MCP stdio session. Authentication, encryption and multi user isolation are sshd's job. kitbashd contains no authentication code for members in version 1. OIDC or an HTTP MCP endpoint can be added later without touching the object model. Processes are the one identity kitbashd checks itself: a bearer token per Process on the Telemetry receiver, minted at `proc_run`, because a container's network address says nothing about who runs it.

Admins are members of the `kitbash-admin` group. They create users, manage `/org`, and approve queued operations, all through the same MCP surface.

Creating a member is root's work, so the `users` family is served by kitbashd: kitbash-mcp forwards the call over the socket, kitbashd checks the peer is an admin and runs the system's own tools (`useradd`, `usermod`, the subordinate id files, `authorized_keys`, the runtime directory). Removing a member stops and unregisters their Processes, ends their sessions, archives their home under `/org/.archive/<name>` where the surface cannot see it, and deletes the account. The first admin has no admin to create them: `kitbash-adduser` on the console does the same work and stays as the bootstrap and break glass path.

kitbash-mcp talks to kitbashd over a unix socket, `/run/kitbash/kitbashd.sock`, owned by root with group `kitbash-users` and mode 0660. Processes talk to kitbashd over TCP on the host's address, port 4318, because rootless networking delivers `host.containers.internal` to that address and not to loopback; the host firewall scopes who else can reach it, and the token decides whose records they are. kitbashd learns who is calling from the socket's peer credentials, the same kernel fact sshd relied on, and reads group membership from the system. There is no token and no second identity. The socket carries OTLP/HTTP on the standard paths and a small JSON API for queries and settings; the machine readable definition is [`spec/kitbashd-api.yaml`](spec/kitbashd-api.yaml).

One break glass path exists for the operator: a serial console or a dedicated `ops` user with a real shell, disabled by default and enabled only from the console. Without it the first stuck machine is a reinstall.

### 4.6 The three infrastructure layers

Infrastructure is three layers, and the object model binds to the shape of an OCI image rather than to any layer's API.

1. **Machine layer, Proxmox VE or any VM host.** Version 1 is one VM. kitbashd does not manage the hypervisor. A PVE runner kit can later treat a container or VM as a Process for workloads that need a GPU or Windows.
2. **Unit layer, rootless podman.** The built in runner. Every Process is a container. A reverse proxy reads process metadata and gives `http` Processes a hostname.
3. **Orchestration layer, Kubernetes.** Deferred. The triggers to adopt it are written down: more than one machine, a need for autoscaling, or a customer requirement. Adoption means one more run kit, not a schema change, because a Process already describes image, environment, ports, health and limits, which map one to one onto a Kubernetes Deployment.

### 4.7 Storage

kitbashd uses an embedded SQLite database for its own state and for Telemetry, at `/var/lib/kitbash/kitbashd.db`, opened through a pure Go driver so the binary stays static. One machine, one file, no second daemon. If Telemetry volume outgrows SQLite the store becomes a pluggable interface and an observability kit takes over long term retention. Postgres and ClickHouse are explicitly out of scope for the host.

## 5. Version 1

### 5.1 Scope

Version 1 proves the loop on one machine with several users: an agent writes source into Files, builds it into a Package, runs it as a Process, calls its tools through MCP, and reads the Telemetry that results.

### 5.2 Not doing in version 1

- A human web interface.
- Any authentication code. sshd is the authentication.
- More than one machine, fleet management, or a control plane.
- A cross machine workshop for sharing kits. The manifest format is the interface a future workshop will index.
- Kubernetes or any runner other than rootless podman.
- A custom OS image. Alpine plus apk is the installation.
- Search, embeddings or retrieval in the core.
- A permission model beyond Linux users, groups and file modes.
- Installing anything on the host that is not one of the seven components.

### 5.3 Milestones

Each milestone is done when its acceptance sentence is true on a real machine, not in a unit test.

**M1 Boot.** An Alpine VM on Proxmox with kitbashd installed from an apk. A user connects over SSH from Claude Code and the MCP surface lists `/org` folders by name and description, enters one, and reads a Markdown file and a PNG.

**M2 Packages.** The agent writes a folder with a manifest and a Dockerfile, asks kitbashd to build it, runs it as a Process with `expose: mcp`, and calls one of its tools with a schema validated input. import-mcp wraps an npm MCP server the same way.

**M3 Telemetry.** Every MCP call, build, and tool invocation from M2 appears in Telemetry with the four required attributes. The agent queries by `package` and by `user` and gets consistent answers. Retention deletes records older than the configured window.

**M4 Kits.** The five hook contract is implemented. An observability kit subscribes to the fan out and receives every span. import-oci wraps a Docker Hub image. An evaluation kit writes one judgment back and it is queryable as Telemetry.

**M5 Multi user.** An admin creates three users through MCP. Each has a private home, sees `/org`, and cannot see the others' homes or Processes. A write to `/org` by a regular user lands in the approval queue and an admin approves it through MCP. All Processes come back after a reboot.

**M6 Workflow kit.** A workflow engine Package reads a graph from Files, calls tools on two other Processes, and one workflow references another. The core did not change to make this work.

### 5.4 Risks

- Rootless podman on Alpine needs cgroups v2, subuid ranges and fuse-overlayfs configured correctly. This is M1 work and is the first thing to verify on real hardware.
- Development must happen in a VM, not an LXC container. Nested container runtimes inside LXC are unreliable.
- import-cli is open ended. It is not in version 1 milestones on purpose. import-mcp and import-oci cover most real needs.
- The progressive disclosure rule depends on people and agents writing good descriptions. The core enforces presence, not quality. An evaluation kit that flags never read folders is the intended feedback loop.

### 5.5 Open decisions

The MCP tool surface is decided in `spec/mcp-surface.yaml`: six families, `fs` implemented in M1, the rest declared.

- Whether `files` deploy units are needed in version 1 at all, or whether every Package is a container until a real case appears.
- Resource limits: rootless podman on OpenRC has no cgroup delegation, so `limits` are passed to the runtime and recorded but not enforced until kitbashd places member sessions in delegated cgroups.
- Health: `health` is recorded but not probed until kitbashd supervises Processes. Liveness in M2 is PID 1 of the container.
- A Process started in one MCP session appears on another session's surface when that session reconnects, not live.
- Import kits run under the member who imports. Whether an admin can run a kit once for every member is an M5 question.
- Metrics: the store and the receiver accept them from M3; the first producers are the M4 kits.
- Build and run kits: the contract is declared, dispatch is not implemented, and the manifest has no field to name a builder or runner. Add the field when a second builder or runner exists.
- Fan out is best effort. A subscriber that must not miss a record should read the store through `tel_query` and treat the push as a wake up.
- Approvals cover `fs_write` and `pkg_import` into `/org`. Whether `proc_run` of an `/org` Package by a member should run as a shared Process rather than a private one is open; today it runs privately under the member.
- Sharing a built image between members is still open. An `/org` Package built by one member is built again by the next, from the same commit to the same image ID.
- A Process acting as an agent has its owner's full surface. Narrowing what a Process may call (a per Process allow list in the manifest) is deferred until a kit needs less than its owner has.

## 6. Vocabulary

Four nouns, all plural, none invented here.

| Term | Meaning |
|------|---------|
| Files | The versioned filesystem, `/org` and `/home/<user>` |
| Packages | Runnable units with a manifest, built to or imported as OCI images |
| Processes | Running Packages, supervised by kitbashd |
| Telemetry | Traces, metrics and logs, OTLP in, queryable out |
| kit | A Package that implements one of the five lifecycle hooks |
| manifest | `kitbash.yaml`, the one file that makes a folder visible and optionally runnable |
| kitbashd | The single daemon that is the product |
| kitbashOS | Alpine plus kitbashd |
