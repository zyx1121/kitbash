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

**Progressive disclosure is the reading model.** A folder is visible to an agent only if it carries a `kitbash.yaml` with a `name` and a `description`, and that manifest speaks for everything beneath it: the folders inside a Package, `src`, `public`, `deploy`, are part of what the manifest describes and need no manifest of their own, and a nested `kitbash.yaml` only begins a new description where one is wanted. The agent sees folder descriptions first, decides whether to enter, and only then sees file names and contents. A folder with no manifest above it does not exist as far as the MCP surface is concerned. This rule is what keeps Files from becoming a dump, and stopping it at the manifest is what keeps it from taxing every folder of source: the first clean agent trial paid four calls to learn that `public/` wanted a manifest, and nobody browses inside a Package by description. The same chain, cut at the nearest manifest, is what a mount source is checked by (2.3).

**One write, one commit, any number of files.** `fs_write` takes one file, or a `files` list of them under one root and one message, and makes one commit either way. A Package is several files that belong together, and the history should say so; writing them one call at a time also cost the first trial thirteen calls where one would do.

Any file type is a first class citizen. Markdown returns text. PNG and JPEG return image content. PDF returns page images or extracted text. The core stores blobs and knows nothing about their meaning.

Every commit is a version. Files has no separate version object.

**A Process writing through a mount makes no commit, and that is a known hole.** A unit can declare `mounts` and see folders of Files as bind mounts, see 2.3. A container that writes through a `rw` mount writes into the working tree of that repository directly: the file is there, `fs_read` and `fs_list` see it at once, and `fs_history` does not, because nothing committed it. The repository is left dirty, and the next `fs_write` by the member commits their own change and whatever the Process left beside it, under the member's name. Version 1 accepts this: the alternative is kitbashd committing on behalf of a Process, which needs an author, a message and a policy for a Process that writes a hundred times a minute, and that is a later kit rather than a core change. Nothing is silently hidden: a member reads the Process's files with `fs_read` like any other file.

### 2.2 Packages

A Package is a folder in Files whose manifest carries a `deploy` section. Building it produces an OCI image. The image digest is the Package version, and the build records which Files commit it came from. The chain commit to digest to process is the entire provenance story and needs no extra object.

The digest of a locally built image is its OCI image ID, the sha256 of the image configuration. The build stamps the image with labels `kitbash.path`, `kitbash.name`, `kitbash.commit` and `kitbash.user`, so the image store is the build history and no second record is kept.

**A built image is shared, not built again.** Packages a member builds live in that member's rootless image store, which no other member can read, so an `/org` Package would otherwise be built once per member from the same commit to a digest each of them has to trust separately. kitbashd records every build of an `/org` path as `(path, commit, digest, builder)` and copies one image between two members on request: it runs `podman save --format oci-archive <digest>` as the builder piped into `podman load` as the requester, two setuid children joined by a pipe the kernel holds, each in its own member's cgroup leaf, and then checks the requester's own store for the digest before it answers. `pkg_build` of an `/org` path asks for the builds of the current commit before it builds anything: an image of that commit already in this member's store is returned as it is, one another member has is copied, and only a commit nobody has built is built. One commit is therefore one digest for everybody, and repeating `pkg_build` converges rather than producing a second digest for a commit that already has one. `proc_run` with a digest this member does not have fetches it the same way; `pkg_inspect` merges the records with the local image list, so an agent sees who built what. A copy is served only for a build somebody recorded, and only for `/org`: a Package in a home is that member's alone, and kitbashd is root, so it never reads a private image store on somebody else's behalf. Every failure falls back to building, which is what happened before the record existed.

**A record is a claim, and the image is the evidence.** A digest is a number a caller chose; what says an image is a build of one Package at one commit is the labels the build stamped inside it. So kitbashd accepts a record only from a member who holds that digest and whose image carries `kitbash.path` and `kitbash.commit` for what they are recording, checks the same labels on the builder's image before it copies anything, and `pkg_build` checks them again on what it holds or receives before it answers that digest instead of building. Without those three checks a member could record the commit somebody else is about to build against an old image, and that member's next `pkg_build` would answer the old image and never build their change. The row's builder is never replaced either, because the row is what decides whom a copy is asked of, and a member's records are pruned only against their own, so no member can push another's build out of the table.

There are no package kinds. A skill, a tool server, a workflow engine and an observability kit are all Packages. What distinguishes them is what their manifest declares, not a category the platform maintains. Tags exist for humans to search by. Platform logic never reads tags.

Packages can also be imported rather than built: an existing OCI image, an existing MCP server from npm or PyPI, or a plain CLI tool. Import is done by kits, see section 3. The host never installs third party software. Everything third party arrives as a Package.

### 2.3 Processes

A Process is a Package running as a rootless container under the user who started it. kitbashd supervises Processes directly: restart policy, boot restore, health, resource limits. There is no dependency on systemd user sessions. Boot restore works from the registrations kitbashd holds: at start it creates each owner's runtime directory and starts each registered container as its owner, so every Process that was running before a reboot is running after it.

**kitbashd runs the container, not the session.** A member cannot move their own process into a delegated cgroup, so limits declared in a manifest would be a record and nothing more. kitbashd owns the tree: `/sys/fs/cgroup/kitbash`, one cgroup per member below it, and one cgroup per Process below that, `/sys/fs/cgroup/kitbash/<member>/<process id>`. The Process's cgroup is the ceiling: root writes `memory.max`, `cpu.max` and `pids.max` there and those files stay root's. What the member is given is the directory of that cgroup and its three delegation files, which is exactly enough for their rootless podman to create the container's cgroup beneath the ceiling and move the container into it. The member's own cgroup stays root's in full, so nobody can make a Process cgroup beside the ones kitbashd made, and a member cannot raise what their own Process may spend.

Every `podman run`, `stop` and `rm` of a Process runs as its owner from inside the member's own leaf, `/sys/fs/cgroup/kitbash/<member>/run`, with `--cgroup-parent=/kitbash/<member>/<process id>`, so `memory.max` inside the container is what the manifest asked for while the podman child that started it spends none of that memory. A member's MCP session is placed in the same leaf, which it asks for once at startup: moving a process between cgroups needs write access to the common ancestor's, and a session sshd started shares only the root cgroup with anything of kitbash's, so without it `proc` tools that exec into a container are refused by the kernel. `proc_run` registers the Process and then asks kitbashd to start it; the daemon writes the environment file itself, so the Telemetry token and the fan out secret never cross a request body or a command line. The cgroup goes when the container does, and a member's whole cgroup goes with their account.

A host whose cgroup filesystem is not version 2 or not writable runs that Process unplaced, records its limits and says so on that start: the fail open is per member and per attempt, never a daemon that has given up. It covers a host with no tree to place anything in, and not a host that places some processes outside the one kitbashd built. A member logind knows is given a systemd user manager, and rootless podman asks that manager for a scope to hold its pause process; that scope is outside `/sys/fs/cgroup/kitbash`, so the common ancestor of it and the container's cgroup is the root cgroup, the kernel refuses to move the container into its own cgroup, and the Process does not start at all. A kitbash host runs OpenRC and has no logind, which is why `/run/user/<uid>` is made by `/usr/share/kitbash/rootless-prereqs.sh`, which kitbashd's service script runs before it starts, rather than by a session; wherever kitbash is run on a systemd host, a member must be kept out of logind's hands and that directory made the same way. Restore recreates each Process's cgroup with its ceiling, because the cgroup filesystem does not survive a reboot and a container whose parent is gone does not start at all: the limits travel with the registration, so what a Process was started under is what it comes back under. A registration written before limits were recorded, and a container created before any of this existed, come back without a ceiling and gain one the next time they are run.

A Process declares how it is exposed:

- `mcp`: its tools appear on the user's MCP surface under the package namespace, for example `ffmpeg.transcode`.
- `http`: it gets an internal port and, when the host has a domain, a hostname with TLS: `<name>.<member>.<domain>` by default, or the `hostname` the unit declares. kitbashd is the reverse proxy, see below.
- `none`: a batch job or a subscriber that only talks to Telemetry. With a `schedule` it is a job kitbashd starts on time, see below.

**How `mcp` exposure works.** The image's entrypoint is a stdio MCP server. The Process runs it as PID 1 with stdin held open, which keeps the container alive and is the liveness signal. Every MCP session the owner opens execs one more instance of the same entrypoint inside the container and proxies calls to it, the same way an agent runs a stdio server on a laptop. The tools the surface publishes are the ones the manifest declares, with the manifest's schemas, so input is validated against the manifest before it reaches the container. A tool the server offers but the manifest does not declare is not on the surface. Tool names are `<package>_<tool>`; a Package whose name collides with a built in family (`fs`, `pkg`, `proc`, `tel`, `users`, `approvals`, `secrets`) cannot be run.

Every Process carries labels `kitbash.id`, `kitbash.user`, `kitbash.package`, `kitbash.name` and `kitbash.digest`. The container runtime holds the Process state and kitbashd reads it back; there is no second record.

**How a Process sees Files.** A unit declares `mounts`, at most four, each one a `source` folder of Files, a `target` path inside the container and a `mode` of `ro` or `rw`. A source is a folder under the owner's own home or a top level folder of `/org`, and it has to be visible by the same rule the `fs` family reads by: it, or a folder above it within its root, carries a `kitbash.yaml` with a name and a description (2.1). A folder an agent cannot list is a folder a Process cannot be given, which keeps progressive disclosure one rule rather than two, and it is one function rather than two implementations of it. `rw` is allowed only under the owner's own home. `/org` is read only for everyone, admins included, because an approval is how a member writes there and a Process with a `rw` mount would be a way past the queue. A target may not be `/` or land under `/proc`, `/sys`, `/dev`, `/etc`, `/bin`, `/sbin`, `/usr`, `/lib` or `/lib64`. Mounts are independent of `provides.permits`: a permit governs what a Process may call back over `/mcp`, a mount is a kernel fact the Process needs no permit for.

kitbashd validates every mount as root, at registration and again at every start: the source is opened beneath `/home/<owner>` or `/org` with `RESOLVE_BENEATH` and `RESOLVE_NO_SYMLINKS`, so no component may be a symlink, inside the root as well as out of it, and no resolution may leave the root, the descriptor is fstatted, it has to be a directory, and under a home it has to be owned by that member. The resolved path is what podman is given, as `--mount type=bind,src=<resolved>,dst=<target>,ro=<true|false>,bind-nonrecursive,nosuid,nodev,noexec`. The mounts travel with the registration and are authoritative, so a restore mounts what was recorded rather than what a manifest says today, and a mount that has stopped being legal fails the restore with a problem the owner reads through `proc_list`. `kitbash-mcp` runs the same check as the member first, only so the problem arrives in the session that caused it; it is not trusted and the daemon's check is the one that decides. There is no `--userns` flag: container root already maps to the member. What a Process writes through a `rw` mount leaves no commit, see 2.1.

**How a Process gets a secret.** A unit declares `secrets`, at most sixteen names, each one an environment variable the container needs and does not get from the image, from `environment` or from kitbashd: an API key, a token for a service outside this machine. A name is the variable name, `^[A-Z][A-Z0-9_]{0,63}$`, and may not be one `environment` also sets or one kitbashd speaks for (`KITBASH_*`); a manifest that does either is `invalid-manifest`. The values live with kitbashd and nowhere in Files or in an image: a member writes one with `secrets_set`, sees which names they hold with `secrets_list`, which answers names and timestamps and never a value, and drops one with `secrets_remove`. Secrets are the member's own, scoped by the socket's peer credentials the same way their home is: an admin holds their own set and reads nobody else's, and there is no shared set, because a Process runs under one member and the value has to be that member's to give. kitbashd keeps them as root owned files, `/var/lib/kitbash/secrets/<member>/<NAME>`, directory `0700` and file `0600`, outside the database so the daily backup copy carries none of them, and removes the directory with the account.

At every start, the first and every restore, kitbashd resolves each declared name to the member's current value and writes it into the same environment file it writes the Telemetry token into: the daemon writes it, the member owns it for the length of the start because their `podman` is the one that reads it, and it is removed when the start returns, so a value never crosses `podman`'s command line and is never in a request body of `proc_run`. A declared name the member has not set is `not-found` at `proc_run`, naming the secret and the fix, and on restore it is the problem the owner reads through `proc_list`, the same shape as a mount that stopped being legal. The names travel with the registration; the values do not, which is what makes rotation one call: `secrets_set` writes the new value, and the next start of every Process that declares it reads it. `secrets_set` restarts nothing, as a health probe restarts nothing: which Processes to stop and run again is the owner's decision, and `proc_stop` followed by `proc_run` is how it is made. A value is one line of at most 8 KiB, no NUL, no newline, because the environment file is line based; a multi line credential is encoded by the member before it is set. `pkg_inspect` shows the declared names, because what a Package needs is part of reading it.

What crosses the wire once is the value of `secrets_set`, from the agent over SSH to `kitbash-mcp` and over the socket to kitbashd. The span that call records carries the name and no value, `podman`'s error output is redacted before it is repeated, and no problem detail ever quotes a value. The member's own Processes can read the environment their runtime is given, which is the point: the file kitbashd writes is theirs to read for the length of the start and is gone after it, and the values it was made from stay root owned. A Process may call `secrets_set` over `/mcp` only when its permits name it, like every other tool.

**A swapped source is refused, not detected: the bind mounts are checked in the container's own namespace before its first instruction runs.** podman resolves the source path itself, in its own process, after kitbashd has looked at it, so a source replaced by a symlink in between is followed by podman and the container is given whatever it pointed at. That is not a small hole: an admin could point a `rw` mount at `/org` that way and write the shared root with no approval behind it, and the approval queue is the trail of every change there. Reporting it afterwards would not be enough either, because by then the entrypoint has run and a single write has landed.

A Package that declares more than one unit runs as one pod and these four steps run once per unit inside it, see 5.6. So a Process is started in four steps rather than one. `podman create` makes the container with its mounts. `podman init` has the runtime build its rootfs and its bind mounts and leave its init process created, with the image's entrypoint not yet executed. kitbashd, as root, then asks the runtime which source is mounted at each target and in which mode, and stats each target through `/proc/<pid>/root`, which resolves in the container's own mount namespace, holding the device and inode it finds there to the ones the validation opened. Only then does `podman start` run the entrypoint. A mismatch, a container the runtime would not prepare, or a container with no process after it was prepared, is `not-permitted` with the detail "the mount source changed between validation and start", and that container is removed having executed nothing. There is no fallback: a container kitbashd cannot read is a container that does not start.

The boot restore does the same, reading what the runtime calls a container as well as its process id, because a prepared container has a process and has run nothing. A Process that is running is read through the process it already has, because its namespace is the only witness of what it holds and this daemon did not make it. One found prepared is where a start pauses, so it is read the same way and then started. One in any other state is not started by name, because a start makes the bind mounts again and the entrypoint would be running before anything could read them: it is made again through the same four steps. The heal path is the same four steps too. What all of this protects is the `/org` approval trail and progressive disclosure.

**How `http` exposure works.** Sharing a web application is giving someone its address, and the Process that serves it runs under the member who made it, because that is whose work it is. There is no shared identity to hand a service to and no second step after `proc_run`. kitbashd is the reverse proxy: it routes by host name to the port the Process's container publishes, the same port health probes are held to, and it terminates TLS itself or leaves that to the gateway in front of it. `KITBASH_TLS=acme`, the default with a domain, listens on 80 and 443 and obtains certificates; `KITBASH_TLS=gateway` listens on 80 only and trusts the `Host` a gateway that already terminated TLS forwards, which is how a host behind a NAT with one public address is put on the internet: the gateway forwards everything under the domain to the host, and kitbashd answers the gateway's question whether a name is served at `/.kitbash/ask`, so the gateway obtains one certificate per name on demand. The operator gives the host a domain once, `KITBASH_DOMAIN` at install, with a wildcard record for it and everything below it pointing at the host's public address, or at a gateway that forwards 80 and 443 to it; a host with no route from the outside has no domain and `http` Processes keep their internal port only, as today. With a domain, every `http` Process is `<name>.<member>.<domain>`: both labels are already valid DNS labels by the patterns the surface enforces, and the member's label is the attribution, visible in the address. A unit may declare `hostname` instead, one name outside the domain: under `KITBASH_DOMAIN` there are derived names and nothing else, so a declared name there is `invalid-manifest` and no member can take the apex or a neighbour's address. What makes a declared name the member's is that it points here, so kitbashd resolves it at registration and at every start and serves it only when the answer carries an address of this host, which the operator names as `KITBASH_PUBLIC_ADDRESS` where the host sits behind a gateway, or is a `CNAME` to the derived name; anything else is `not-permitted` naming what it resolved to, and on restore it is the problem its owner reads through `proc_list`. Certificates come from ACME, one per host name, obtained when the name is first served or at start and renewed by the daemon; there is no wildcard certificate, because a wildcard covers one label and the default names have two. `proc_run` answers with `url`, `proc_list` shows it, and the proxy writes one span per request it forwards with the four attributes. A route carries the port the runtime said that Process's container publishes, read when the route is built, so a container that died outside `proc_stop`, a crash or an OOM kill, would keep a port nothing is bound to: a forward nothing accepted the connection for is re-resolved, and a container the runtime no longer calls running, or does not have at all, loses its port and answers 503 rather than 502, which the health probe does on its own cadence as well. Only a dial failure asks, and the answer is asked once per route at a time and kept for five seconds, so what a 502 loop costs this host is one inspect per name per five seconds and not one per request. A `hostname` that another Process on the host already serves is `conflict` at registration. A unit that declares `provides.subscriptions` is a receiver for kitbashd rather than a service for the world, so it gets no name and no `url`, its port is there for the fan out alone, and a `hostname` on such a unit is `invalid-manifest`. The proxy is not a third built in: it is how the built in runner exposes what it runs, see section 3.

**How a schedule works.** A unit with `expose: none` may declare `schedule`, five field cron syntax, read in UTC. `proc_run` registers the job and starts nothing; kitbashd starts the container at each tick as its owner, through the same four steps as any start, and the run ends when the entrypoint exits. A tick that arrives while the previous run is still executing is skipped and recorded. Every run writes one metric record `kitbash.schedule` carrying the four attributes, the exit code and whether the tick ran or was skipped, so "did it run this morning" is a `tel_query`. Ticks missed while the host was down are not made up: the next tick runs. `proc_logs` reads the last run. `proc_stop` unregisters the job, and a `schedule` on a unit with any other `expose` is `invalid-manifest`. Like the proxy, the scheduler is a way the built in runner starts what it runs, not a hook and not a third built in.

**Every Process can be an agent.** The same MCP surface a member reaches over SSH is reachable from inside a Process: kitbashd serves MCP over streamable HTTP at `/mcp` on the Process receiver, authenticated by the Process token, and answers each session by running `kitbash-mcp` as the Process's owner and relaying to it. A Process therefore sees exactly what its owner sees, Files, Packages, Processes, Telemetry and the tools of the owner's other Processes, under the kernel's rules, with no code path of its own. Every span such a session records carries `kitbash.caller`, the calling Process id, so what a Process did on its owner's behalf is one query. The endpoint is given to every container as `KITBASH_MCP_ENDPOINT`.

**What a Process may call.** A Process sees its owner's surface narrowed to what its Package declared in `provides.permits`, a list of tool name globs and a list of absolute path prefixes. A Process whose Package declares no `permits` block gets an empty surface over `/mcp`: the block is the declaration, not a filter over a surface the kit would otherwise have, so an admin's kit is not an admin by default. The permits travel with the registration, kitbashd hands them to the session's `kitbash-mcp` as `KITBASH_PERMITS`, and that process publishes only the tools a glob names, built in and Package tools alike, and refuses a call outside the permitted names, or one whose path or Package argument is outside every prefix, with `not-permitted` before the handler runs. A tool glob matches literal names, built in tools included, because writing `fs_*` is declaring the fs family; the one reserved word is `packages`, which stands for every tool the owner's running Processes publish and for no built in, so a kit that composes other kits asks for that and not for `*_*`, which would be asking for `fs_write` and `users_remove` as well. A prefix covers the path itself and everything below it and may carry `*` as one whole component, as in `/home/*/flows`; a Package that permits tools and no prefix may call nothing that takes a path. `fs_list` without a path and `pkg_list` enumerate the caller's roots rather than naming one, so they are governed by the tool list, with the same floor as the rest: a Package that permits no prefix at all may not call `fs_list`, which is a tool that takes a path. Approving is the one call that runs another tool inside its own handler, so `approvals_approve` checks the queued path against the same prefixes before it executes and stores a `not-permitted` on the approval when it does not fit. `pkg_inspect` shows the block, because what a kit may do is part of reading it. None of this narrows a member's own SSH session, which is the kernel's business as before.

### 2.4 Telemetry

Telemetry is an OTLP receiver and a store. Every span, metric point and log record must carry four attributes: `user`, `package`, `process`, and where relevant `path`. Filtering by `user` answers what a person did. Filtering by `package` answers how a tool behaves. No filter is the whole machine. These are three views of one store, not three stores.

On the wire the four attributes are namespaced as the OpenTelemetry conventions require: `kitbash.user`, `kitbash.package`, `kitbash.process`, `kitbash.path`, plus `kitbash.tool` for the surface tool a span records, `kitbash.eval` for judgments written back by kits, and `kitbash.unit` for a record about one unit of a Package that runs as several, see 5.6. The query surface speaks the short names. `kitbash.user` is never trusted from a producer: kitbashd stamps it from the peer credentials of the connection that delivered the record. `kitbash.unit` is not trusted either. kitbashd stamps it on the records it writes about one unit, which is the health probe, because a probe requests the face and the reading is that unit's; the records it writes about a Process as a whole, the proxy's span per request and a job's tick, carry none. A record a producer sends it on keeps it only when it names a unit of that Process, otherwise the attribute is dropped and the record is kept.

**Producers.** kitbash-mcp opens one span per `tools/call` (built in and Package tools alike), a child span per build and per Process start, and one log record per build carrying the build log tail. It exports OTLP/HTTP over the kitbashd unix socket. Nothing on the surface is untraced: the span is opened by middleware before any handler runs.

From M4 every Process is a producer too. kitbashd also listens for OTLP/HTTP on the host address containers reach, and each Process is started with `KITBASH_TELEMETRY_ENDPOINT`, `KITBASH_TELEMETRY_TOKEN`, `KITBASH_PROCESS`, `KITBASH_PACKAGE`, `KITBASH_USER` and `KITBASH_FANOUT_SECRET` in its environment. The token is minted by kitbashd when the Process is registered at `proc_run` and revoked when it stops; it names exactly one Process, and kitbashd stamps `kitbash.user`, `kitbash.package` and `kitbash.process` from it. A Process that never reads the token is simply an untraced producer of nothing; its lifecycle is still traced by kitbash-mcp.

Every record also carries `kitbash.producer`, stamped by kitbashd: the member for a session record, the Process id for a Process record. `kitbash.user` is the member the record is about. The two differ only for evaluation records, see below.

**Fan out.** A Process whose manifest declares `provides.subscriptions: [telemetry]`, `expose: http` and a `port` receives every record kitbashd stores, as OTLP/HTTP JSON POSTed to the standard paths on its endpoint, after the record is stored and stamped. Delivery is best effort and asynchronous with a bounded queue per subscriber; a subscriber that is down loses records and the gap is logged, the store is the source of truth. Every delivery carries a bearer secret minted for that Process at registration and given to its container, so a subscriber knows the records came from kitbashd and not from a neighbour on the host. A subscriber run by an admin receives the whole machine. A subscriber run by a member receives that member's records only, the same rule as `tel_query`.

**Health.** A Process whose Package declares `deploy.units[0].health.http` and publishes an endpoint is probed by kitbashd: it requests that path on the Process's endpoint every `health.interval`, thirty seconds by default and five seconds at the fastest, with a five second timeout, and writes one metric record `kitbash.health` per probe, value 1 for healthy and 0 for unhealthy, carrying the four attributes plus `kitbash.health.status`, the HTTP status or the class of the failure. Only a Process that publishes one is probed, which today means `expose: http`: an `expose: mcp` Process is a stdio server with no port, so it declares a path that nothing requests. A Process a run kit owns is not probed either, because there is no container of it on this host and kitbashd does not supervise it. The port is not the member's to choose either: kitbashd asks the runtime, as the owner, which host ports that Process's own container publishes, and refuses a registration whose endpoint names anything else, so a declaration can never point the daemon at a neighbour's Process or at a port it has no business requesting. A probe restarts nothing and changes no registration: what it produces is Telemetry, so acting on it belongs to a kit or to the owner, and `health.exec` is recorded and run by nobody.

**Evaluation.** An evaluation kit is a subscriber that writes judgments back through the Process receiver with `kitbash.eval: true`. A judgment is about a subject, named by `kitbash.subject.trace_id` and `kitbash.subject.span_id`, and it carries the subject's `kitbash.user`, `kitbash.package`, `kitbash.process` and `kitbash.path` so that the query that asks what a Package did also returns how it was judged. kitbashd accepts those claims on an evaluation record only when the producing Process was run by an admin; a member's evaluation kit has them forced to the member's own. `kitbash.producer` always names the kit's Process, so a judgment is never mistaken for the act it judges.

**Reading.** `tel_query` returns records of one signal filtered by the four attributes and a time range. A member reads their own records; an admin reads everyone's. `tel_retention` reads the window per signal and lets an admin set it.

**Internal causes.** The cause of an internal problem is recorded as a log record with `kitbash.internal: true`, which `tel_query` answers to admins alone: the agent is given the problem's instance to quote, and the admin queries by it rather than reading a log file on the host. No producer marks a record that way: kitbashd drops `kitbash.internal` from every export, and kitbash-mcp reports a cause to `POST /kitbash/v1/internal` instead, which is the one path that writes one.

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
  permits:                              # optional, what a Process of this package may call over /mcp
    tools: [fs_read, fs_list, packages] # globs, plus packages: the other kits' tools, no built in
    paths: [/org, /home/*]              # absolute path prefixes, * as one whole component

deploy:
  units:
    - type: container
      build: .                # or image: docker.io/jrottenberg/ffmpeg@sha256:...
      builder: /org/nix-build # optional, the build kit that builds this unit, see section 3
      runner: /org/pve-runner # optional, the run kit that runs this Process, see section 3
      expose: mcp                 # or http, which gets <name>.<member>.<domain> with TLS, or none
      # hostname: app.example.com # http only: a name the member owns instead of the default
      # schedule: "0 8 * * *"     # none only: kitbashd starts the container at each tick, UTC
      # name: probe               # required when a Package declares more than one unit, see section 5.6
      environment: { LOG_LEVEL: info }  # plain variables, written as the manifest spells them
      command: [ffmpeg, -listen]        # optional, the words that replace the image's CMD
      secrets: [ANTHROPIC_API_KEY]   # names only; the member sets values with secrets_set, see section 2.3
      health: { http: /healthz, interval: 30s }  # probed and recorded, see section 2.4
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

**Built in versus installed.** Exactly two things are built into kitbashd because the system cannot boot without them: the OCI build path and the rootless podman runner. Everything else, including the workflow engine and every import kit, is installed. This boundary is fixed. Adding a third built in requires changing this document first. The reverse proxy and the scheduler of section 2.3 are not a third: they are two ways the built in runner exposes and starts what it runs, decided by the manifest and not by a hook, and a run kit that takes a Process elsewhere takes them with it.

**Workflows are a kit.** A workflow engine is a Package that reads graph files from Files, calls tools on other Processes, and emits Telemetry, all through the MCP surface every Process can reach. The core does not know what a workflow is: the graph format, the templating between steps and the way one workflow names another are the engine's contract, published in its manifest and its prompt file, never in this document or the specs. A workflow that references another workflow is the engine's concern, resolved by schema compatibility of inputs and outputs.

**The contract, hook by hook.** A kit talks to kitbashd the way every Package does: through its manifest, its MCP tools and the Telemetry receiver. There is no plugin API.

| Hook | The kit declares | kitbashd does |
|------|------------------|---------------|
| import | `kit: [import]` and a tool `import` whose `source` pattern is the route | `pkg_import` picks the kit whose schema accepts the source and writes the files it returns |
| build | `kit: [build]` and a tool `build` with input `{path, context}` and output `{digest, log}` | `pkg_build` calls the kit a unit's `builder` names and records the digest it answers as the Package version |
| run | `kit: [run]` and a tool `run` with input `{package, digest, name, unit}` and output `{id, state, endpoint}` | `proc_run` calls the kit a unit's `runner` names and registers what it answers as the Process |
| observe | `kit: [observe]`, `subscriptions: [telemetry]`, `expose: http`, `port` | POSTs every stored record to the Process as OTLP/HTTP JSON, see 2.4 Fan out |
| evaluate | `kit: [evaluate]` and usually the observe declarations as well | accepts records with `kitbash.eval: true` and subject attributes through the Process receiver, see 2.4 Evaluation |

**The build and run hooks, and how dispatch works.** A container unit names its kit in a field beside its build context: `builder: /org/nix-build` and `runner: /org/pve-runner`. The value is a Package folder, and the manifest of that folder declares `provides.kit: [build]` or `[run]` with the hook's tool. `build` is the build context, a string, which is why the two fields sit beside it rather than under it: a folder that names its builder still names the context that builder reads. A unit that names neither is built by the OCI path and run by the rootless podman runner, so nothing changes for a manifest written before the fields existed.

Dispatch is core routing and not a third built in: the two built ins of the paragraph above are still the only things that build and run a Package on their own, and what the core adds is the choice between them and a kit the caller is already running.

`pkg_build` resolves the commit and the build context first, because kitbash builds from a commit and rule 4 of section 2.5 is the core's whoever does the building, and then calls the kit's `build` with `{path, context}`. The digest it answers is the Package version and its log tail is the one log record per build; the span is the same span a local build opens. A build a kit made elsewhere is not recorded with kitbashd, because a record is a claim about an image in the recording member's own store and a copy is served from there.

`proc_run` calls the kit's `run` with `{package, digest, name, unit}`, the unit as the manifest wrote it, and registers the `{id, state, endpoint}` it answers as the Process. The kit owns what it started: the registration carries the runner and no container name, so boot restore skips it, kitbashd refuses to start, stop or remove a container it does not have, and `proc_stop` forwards to the kit's optional `stop` tool or answers `not-permitted` saying the runner owns it. `proc_list` reads such a Process back from the registry, which is the only thing on this host that knows it exists. The kit is the one that converges, because it is called with the same Package and the same name every time; a run that answers with a new id has replaced the Process of that name, and the registration it replaced is dropped rather than left holding a token. An endpoint the kit answers that is not this host's loopback address is reported to the caller and not registered, because the Telemetry fan out is POSTed to what is registered.

A builder is paired with a runner in practice. The image a build kit makes is wherever that kit made it, not in the member's own image store, so a Package that names a builder and nothing else has a digest this host cannot start: `proc_run` says so and names the builder rather than sending the agent back to `pkg_build`.

Both hooks fail the same way. A kit a manifest names and nobody is running is `not-found` with the fix to run it; a kit whose tool schema does not accept what the hook is called with is `invalid-manifest`, because dispatch binds on schemas and a Package whose tool refuses the hook does not implement it.

**The import hook.** An import kit is a running Process whose manifest declares `provides.kit: [import]` and a tool named `import`. The tool's input schema has a `source` string, constrained by a pattern to the source syntax the kit understands, and its output is a list of files, each a relative path with text or base64 content. `pkg_import` walks the caller's running kits, picks the one whose `import` input schema accepts the source string, calls the tool, and writes the returned files into the target folder as one commit. The choice binds on schemas, not on a registry of kit names. Two kits accepting the same source is a conflict the caller resolves by stopping one.

**Import kits are the package manager.** A human runs `apt install ffmpeg` and reads `--help`. An agent asks import-cli to wrap ffmpeg, gets a Package with tools that carry schemas, and calls `ffmpeg.transcode` with a structured result. import-mcp is nearly automatic because MCP servers already declare tool schemas. import-cli is the hard one, and it is two steps rather than one. `import` drafts a Package that builds and runs knowing only the apk package name: it carries a `run` tool that takes an argv array and a `probe` tool that reports the binary's `--help`, `--version` and man text. The agent builds it, runs it, calls `probe`, hands what it answered to the kit's `refine` tool, and writes the files that come back over the folder. The Package then has one tool per subcommand, or one for the binary, each carrying a schema derived from the flags. No model is involved: the draft is a deterministic parser for the getopt, argparse, cobra and clap help styles, and a text it cannot read yields the `run` tool and notes saying so. Neither tool reaches the network and neither runs the binary, so the kit holds no permits. That validation loop is work an agent does itself.

## 4. kitbashOS

### 4.1 Base

Alpine Linux plus one daemon, `kitbashd`, written in Go and shipped as a static binary in an apk. The apk ships two binaries: `kitbashd`, the resident daemon OpenRC starts at boot, and `kitbash-mcp`, the per session process sshd starts for each member. From M3 kitbashd is the OTLP receiver and the Telemetry store. The container runtime still supervises Processes on kitbashd's behalf between boots; from M5 kitbashd restores every registered Process at boot, holds the approval queue and creates members. Alpine is chosen for its appliance lineage, its 8 MB root filesystem and its package manager. Go is chosen because a static binary has no musl versus glibc problem and no runtime to install.

Proxmox VE is the precedent for the packaging model: a standard base distribution plus one package that turns it into the appliance. The ISO is that model carried one step further, the way the Proxmox installer is Debian's with one repository added: an unmodified Alpine image built with Alpine's own mkimage, with the kitbashd apk and its dependencies on it and an answer file for an unattended install. Nothing in Alpine is forked.

### 4.2 What is on the host

Seven components at most. Five are existing software, and the seventh is not installed today.

1. Linux kernel
2. OpenRC as minimal init
3. sshd, used only as the MCP transport
4. rootless podman with cgroups v2 and subuid ranges configured
5. git
6. kitbashd, which is also the OTLP receiver
7. OpenTelemetry collector, only if a Process ever needs a protocol kitbashd does not speak; Alpine ships none and M3 installs none

nftables is on the host as well. It is the kernel's own packet filter rather than an eighth component: `deploy/install.sh` writes one ruleset with it, and that ruleset scopes one port, the Process receiver of 4.5.

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

Creating a member is root's work, so the `users` family is served by kitbashd: kitbash-mcp forwards the call over the socket, kitbashd checks the peer is an admin and runs the system's own tools (`useradd`, `usermod`, the subordinate id files, `authorized_keys`, the runtime directory). Removing a member is a job kitbashd runs for itself: the call marks the member `removing` and answers 202, and the daemon then stops and unregisters their Processes, ends their sessions, deletes their secrets, archives their home under `/org/.archive/<name>` where the surface cannot see it, and deletes the account. A removal is not the length of a request, so every step is idempotent, a step that fails is logged and the job goes on, the state it ends in is `removed` or `failed` with the step that failed, and `users_list` is where an admin reads it. While it runs the member takes no new work, their scheduled jobs come off the ticker before anything is stopped, and `users_create` of that name is `conflict` until the removal has finished. The mark is in the store, so a daemon that stopped in the middle of one takes it up again at its next start, the archive of a home whose account was already deleted included. The first admin has no admin to create them: `kitbash-adduser` on the console does the same work and stays as the bootstrap and break glass path.

kitbash-mcp talks to kitbashd over a unix socket, `/run/kitbash/kitbashd.sock`, owned by root with group `kitbash-users` and mode 0660. Processes talk to kitbashd over TCP on the host's address, port 4318, because rootless networking delivers `host.containers.internal` to that address and not to loopback; `deploy/install.sh` writes the nftables ruleset that keeps it to the interface a rootless container reaches it on, which is loopback, and the token decides whose records they are. 4318 from any real interface is dropped, whatever source address it claims. That ruleset decides no other port: 22 is accepted first so it cannot lock an operator out, everything else stays as it was, and a host firewall beyond those two ports is still the operator's. kitbashd learns who is calling from the socket's peer credentials, the same kernel fact sshd relied on, and reads group membership from the system. There is no token and no second identity. The socket carries OTLP/HTTP on the standard paths and a small JSON API for queries and settings; the machine readable definition is [`spec/kitbashd-api.yaml`](spec/kitbashd-api.yaml).

**What needs kitbashd.** `proc_run`, `proc_stop`, the `tel` family, the `users` family, the `approvals` family and `/mcp` all go through the daemon; without it they answer internal with the fix "kitbashd is not running on this host; ask an administrator to start it." `proc_run` refuses rather than starting a Process nobody supervises, because a Process started without the daemon has no token, no fan out secret and no cgroup, and version 1 has no untraced path. What keeps working through the kernel alone: the `fs` family, `pkg_build`, `pkg_list`, `pkg_inspect`, `proc_list` and `proc_logs`, which read the member's own files and their own container runtime.

One break glass path exists for the operator: a serial console or a dedicated `ops` user with a real shell, disabled by default and enabled only from the console. Without it the first stuck machine is a reinstall.

**What the surface says about itself.** The agents that connect are general ones, and a general agent asked for a web application scaffolds it on the laptop it runs on unless something tells it otherwise. So `kitbash-mcp` answers `initialize` with `instructions`, the one field of the protocol that lands in every client's system prompt without anyone asking for it, and that text is the whole deployment conversation: this host is where the work runs, Files under the member's home are the source, a `kitbash.yaml` with `build` and `expose` is all a Package needs, `pkg_build` then `proc_run` ships it, `http` gets an address, `schedule` runs on time, and nothing else has to be decided. What it says is measured, not written once: the M10 acceptance runs a clean agent against it and counts the tokens. The first trial, 2026-09-19, todo web application, same clean agent, same words: without instructions zero kitbash calls and a local scaffold, with them 27 kitbash calls and a running Process. Its 35 calls read as a bill: eleven exploring `/org` and the examples for the shape of a manifest, four failing on a source folder without a manifest, thirteen writing one file each, four doing the work. So the instructions carry one complete minimal Package, `kitbash.yaml` with a `rw` mount for state and the Dockerfile beside it, and say that folders inside a Package need no manifest; the text stays under 2 KiB because it is in every turn.

**Every answer is the size of the question.** A tool result is read by a model at a price per byte, so the surface answers what was asked and nothing more. `fs_list` of a root answers names and descriptions, not the files of every folder; `pkg_list` answers paths and latest digests; `proc_list` takes a `package` and answers that Package's Processes, and without one answers one line per Process; `pkg_build` answers the last twenty lines of the log and the digest; a problem answers cause and fix in two sentences. The first trial's root listing was 7 KiB and its Process listing 5.7 KiB, more than the todo application it wrote.

**What M10 measured, 2026-09-19.** The same clean agent (Claude Code, a fresh configuration directory, no CLAUDE.md, no memory, the account's default model claude-sonnet-5, only the kitbash MCP server), the same three sentences, once on the laptop with no kitbash and once with kitbash 0.12.1 (the third with 0.13.0), tokens read from the client's own accounting:

| Sentence | Laptop, no kitbash | kitbash | Where it landed |
|---|---|---|---|
| "Make a todo web app I can share with my classmates." | 19 turns, 18 calls, 4 failed, USD 0.30; a local Express app and advice to deploy on Render or Railway | 14 turns, 13 calls (8 kitbash), USD 0.28 | `https://todo.loki.kitbash.zyx.tw`, opens, shared list persists on a mount |
| "Every morning at eight, fetch the weather for Hsinchu and post it as a new item on our class board" | 6 turns, 3 failed; stopped and asked the user which compromise to take, because nothing on a laptop schedules durably | 7 turns, 6 calls (5 kitbash), USD 0.18 | `/home/loki/weather-bot` registered with `schedule: 0 0 * * *` (the agent converted 08:00 Taipei to UTC), first tick the next morning |
| "Set up a PDF conversion service my classmates can use." | 3 turns, nothing built; three questions back, one of them where to host it | 25 turns, 24 calls (14 kitbash), USD 0.39 | `https://pdf-converter.loki.kitbash.zyx.tw`, LibreOffice behind Express, converted a text file to a 9 KB PDF |

Three readings. The deployment conversation is gone: no sentence produced a question about where or how to run, and the two that the laptop could not finish finished. The token count is even on the task both sides could do (the todo application), and it was 36 turns and USD 0.50 before 4.5's rules, so the rules paid for the round trips to the host and a little more. Where kitbash spends more it is doing more: the PDF service's 25 turns bought a running LibreOffice at a public address, against three questions. The number to watch next is the 8 to 14 kitbash calls per sentence: `fs_write` with `files`, `pkg_build`, `proc_run` and one `proc_logs` is four, and the rest is the agent reading before it writes, which the instructions could shorten further only by growing. Two more shapes were tried the same evening: three GitHub repositories to an address (all three answered; the two service compose file was folded into one container by the agent, which is a gap to close) and one injected fault (a stopped container, mitigated in under three minutes through `proc_list`, `proc_logs` and `proc_run`). M11 makes these a fixture in `eval/`.

### 4.6 The three infrastructure layers

Infrastructure is three layers, and the object model binds to the shape of an OCI image rather than to any layer's API.

1. **Machine layer, Proxmox VE or any VM host.** Version 1 is one VM. kitbashd does not manage the hypervisor. A PVE runner kit can later treat a container or VM as a Process for workloads that need a GPU or Windows.
2. **Unit layer, rootless podman.** The built in runner. Every Process is a container. kitbashd is the reverse proxy that gives `http` Processes a hostname and a certificate, and the scheduler that starts a `schedule` unit on time, see 2.3.
3. **Orchestration layer, Kubernetes.** Deferred. The triggers to adopt it are written down: more than one machine, a need for autoscaling, or a customer requirement. Adoption means one more run kit, not a schema change, because a Process already describes image, environment, ports, health and limits, which map one to one onto a Kubernetes Deployment.

### 4.7 Storage

kitbashd uses an embedded SQLite database for its own state and for Telemetry, at `/var/lib/kitbash/kitbashd.db`, opened through a pure Go driver so the binary stays static. One machine, one file, no second daemon. kitbashd copies that file once a day with `VACUUM INTO /var/lib/kitbash/backup/kitbashd-<stamp>.db`, keeps the newest seven and reports the last one in health; Files are git repositories the host snapshot covers. Secrets are not in the database: they are root owned files under `/var/lib/kitbash/secrets/<member>/`, see 2.3, so a backup copy taken off the host carries none of them. If Telemetry volume outgrows SQLite the store becomes a pluggable interface and an observability kit takes over long term retention. Postgres and ClickHouse are explicitly out of scope for the host.

### 4.8 Releases

The version has one source: `pkgver` in `packaging/apk/APKBUILD`. The binaries, the apk, the ISO, the git tag and the GitHub release all take their version from it, and CI fails any commit whose README names another one (`packaging/release/check.sh`).

A release is one commit. `make bump VERSION=X.Y.Z` writes the new version everywhere and commits `Release X.Y.Z`. When that commit reaches `main`, `release.yml` sees a `pkgver` with no tag, builds the apk and the ISO on a clean Alpine, boots the ISO once in QEMU, and only then creates the tag `vX.Y.Z` and the GitHub release with the apk, the signing public key, the two binaries, the ISO and their checksums. Nobody creates a tag or a release by hand, and a push to `main` whose `pkgver` already has a tag releases nothing.

Versions are SemVer. Before 1.0 the minor number moves for every milestone or breaking change and the patch number for fixes.

## 5. Version 1

### 5.1 Scope

Version 1 proves the loop on one machine with several users: an agent writes source into Files, builds it into a Package, runs it as a Process, calls its tools through MCP, and reads the Telemetry that results.

### 5.2 Not doing in version 1

- A human web interface.
- Any authentication code. sshd is the authentication.
- More than one machine, fleet management, or a control plane.
- A cross machine workshop for sharing kits. The manifest format is the interface a future workshop will index.
- Kubernetes or any runner other than rootless podman.
- A custom distribution or kernel. kitbashOS is Alpine plus the kitbashd apk, and the ISO is Alpine's own image with that apk on it (4.8).
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

**M7 import-cli.** The agent imports `cli:apk:jq`, builds it, runs it, calls its `probe` tool, calls the kit's `refine` with what probe reported, writes the files it answers with, builds and runs again, and calls the refined `jq` tool with a filter and a document and gets the filtered result. The manifest it ran was drafted by the kit and refined from the Package's own probe. A version in the source is an apk constraint: `jq@1.8.2-r0` pins one apk release with `=`, and `jq@1.8.2`, which names no release, asks for the newest release of that version with `~=`.

**M8 Files mounts.** A member writes a file with `fs_write` under their home, runs a Package whose unit mounts that folder read only, and its tool reads the file back; a second unit mounted `rw` writes a file the member then reads with `fs_read`; a mount of another member's home and a `rw` mount of `/org` are both refused at `proc_run`.

**M9 Secrets.** A member calls `secrets_set` with a name and a value, `secrets_list` shows the name and no value, a Package whose unit declares that name runs and its tool reads the variable, `proc_run` of the same Package under a second member who has not set it is refused naming the secret, and the span `secrets_set` recorded in Telemetry carries no value.

**M10 Ship.** A clean Claude Code with the kitbash MCP server and nothing else configured is told three things by a member who names no tool, no host and no way of deploying: "make a todo web app I can share with my classmates", "every morning at eight fetch the weather and post it to the group", and "set up a PDF conversion service". The first appears at `https://<name>.<member>.<domain>` and opens in a browser, the second is a `kitbash.schedule` record at the next morning's tick, and the third is a URL a second member opens. The tokens the three conversations cost are counted against the same three tasks done on the laptop without kitbash, and the numbers are in 4.5. Done 2026-09-19 on kitbash 0.12.1 and 0.13.0, the weather tick pending its first morning.

**M11 Bench.** What M10 measured by hand is a fixture. `eval/` holds three classes of five sentences each: from nothing (a web application, a scheduled job, a service), from an existing repository (a GitHub address and "get it running for my classmates", the shape EnvBench and SetupBench measure), and from a fault the harness injects first (a container stopped, a mount filled, a secret revoked, a bad image, a dependency down, the shape AIOpsLab and ITBench measure). Every sentence carries a check that reads the outcome and never the transcript: an address answers, a record exists, an item was posted, a service answers again. `eval/bench` runs one sentence against one agent, Claude Code or Codex, connected to a host as a member the round creates and removes afterwards, k times, and writes one row per run: turns, tool calls, kitbash calls, tool errors, questions asked back, cost, wall time, passed, and for a fault the time to mitigate; pass^k is the sentences every one of k runs passed. Results live in `eval/results/` by date, agent and kitbash version, because what the instructions say is tuned against them (4.5). Done when one round of both agents over all fifteen sentences is committed and the three readings of 4.5 are restated with its numbers. The bench runs on an operator's machine that holds the agents' credentials, not in CI.

### 5.4 Risks

- Rootless podman on Alpine needs cgroups v2, subuid ranges and fuse-overlayfs configured correctly. This is M1 work and is the first thing to verify on real hardware.
- Development must happen in a VM, not an LXC container. Nested container runtimes inside LXC are unreliable.
- The progressive disclosure rule depends on people and agents writing good descriptions. The core enforces presence, not quality. An evaluation kit that flags never read folders is the intended feedback loop.

### 5.5 Open decisions

The MCP tool surface is decided in `spec/mcp-surface.yaml`: seven families, `fs` implemented in M1, `secrets` in M9, the rest declared.

- Whether `files` deploy units are needed in version 1 at all, or whether every Package is a container until a real case appears.
- A Process started in one MCP session appears on another session's surface when that session reconnects, not live.
- Import kits run under the member who imports. Whether an admin can run a kit once for every member is an M5 question.
- Metrics: the store and the receiver accept them from M3; the first producers are the M4 kits.
- Fan out is best effort. A subscriber that must not miss a record should read the store through `tel_query` and treat the push as a wake up.
- Approvals cover `fs_write` and `pkg_import` into `/org`. `proc_run` of an `/org` Package runs privately under the member, and that is the decision, not an interim: a service is shared by its address, and it runs under whoever runs it. Whether a member's `mcp` Process should be able to publish its tools onto other members' surfaces is deferred until a case appears.
- A Process acting as an agent has its owner's full surface. Narrowing what a Process may call (a per Process allow list in the manifest) is deferred until a kit needs less than its owner has.
- A secret set through `secrets_set` passes through the agent that calls it, so the agent's context holds the value once. A path that keeps the value out of the agent, such as `ssh kitbash-mcp secrets set NAME` reading stdin, is deferred until a member asks for it.
- Model weights are gigabytes and every member who runs the same model would hold a copy under `volumes`. Whether an admin may hold a shared read only volume under `/org` that units of any member mount is open until the second member runs the first model.
- The GPU image is the larger half of M13. If it slows the GPU milestone, `limits.gpu` and `volumes` land first on a Linux GPU host that exists, and the image follows as its own release.

### 5.6 After version 1: GPU hosts, a cluster, and models as Processes

The occasion is concrete. On 2026-09-20 a document OCR model (jina-ocr-v1 under vLLM) went into service for the organization on a Windows machine with one 10 GB GPU, by hand: a systemd unit, a port forward, a firewall rule, a gateway route and a DNS record, none of it a Package and none of it in Telemetry. More models of that shape are coming, embeddings, rerankers, chat models, and the organization has several GPU machines. The question is whether they become Processes. They do, in three milestones that begin after M12, and each one keeps the four objects and the two built ins. M12 comes first because M11 found it: a Package that is two services.

**What already fits.** A model server is an `http` Process: an image, an environment, a port, a health path and limits. The proxy gives it `<name>.<member>.<domain>`, `secrets` carries its API key, the scheduler can start and stop it, and every request it serves is a span. What is missing is three things: a GPU inside the container, more than one machine, and routing by the `model` field of a request rather than by host name.

**M12 Composition.** The M11 rounds met the same wall twice: `docker/awesome-compose/flask-redis` and `pgadmin` are two services, and both agents had to fold the second into the first container or give up. The manifest has said `deploy.units` in the plural since 2.5, and version 1 ran the first unit only. From M12 a Package with several `container` units runs as one Process that is one podman pod: the units share a network namespace and reach each other on `localhost` by the ports they listen on, each unit is its own image (`build:` a folder of the Package or `image:` a pinned digest), and `mounts`, `secrets`, `environment`, `limits` and `volumes` are per unit as they are today. A unit of a Package that declares more than one carries a `name`, lower case and unique among them, because a unit that is one of several is what `proc_logs` asks for and what `proc_list` and Telemetry name it by; the single unit of a Package may omit it and is the Package itself. A unit may also declare a `command`, the words that run in place of its image's `CMD`, which is how one image serves two units of a Package that differ by what they run. Exactly one unit declares `expose` as `mcp` or `http`; it is the Process's face, `expose: none` on a unit is the same as omitting it, and a Package where two declare a face, or none does, is `invalid-manifest`. The four step start of 2.3 runs per unit inside the pod, the cgroup ceiling is the pod's and the units' limits sum to it unless one of them declares that resource unbounded, in which case the Process runs without a ceiling for it because a ceiling above an unbounded part is not one, `proc_logs` takes an optional `unit` and defaults to the exposed one, `proc_list` shows the units and their states, and the Process is `running` when every unit is. A pod is still one Process: one registration, one token, one address, one row in every listing, so nothing on the surface counts units and nothing in Telemetry changes but a `kitbash.unit` attribute on a unit's own records. A `compose.yaml` is not read; the agent writes the manifest, and the instructions carry one two unit example. Done when a clean agent given the flask-redis address writes a two unit Package, `proc_run` starts one pod with both, the counter increments at its address, `proc_logs` reads either unit, and stopping one unit's process inside the pod makes `proc_list` say which unit is down.

**M13 GPU host.** A unit may declare `limits.gpu`, a count, and kitbashd starts that Process with the devices the Container Device Interface names, `--device nvidia.com/gpu=<index>`, one index per unit and never the same index twice on one host. A host with no GPU refuses such a unit at `proc_run` with `not-found` naming the device, and a host whose GPUs are all held refuses it with `conflict` naming the Process that holds each one. A unit may also declare `volumes`, at most four, each a `name` and a `target`: a directory kitbashd keeps for the member at `/var/lib/kitbash/volumes/<member>/<name>`, outside Files because model weights are gigabytes and a commit is the wrong unit for them, outside the image because a digest should not change when a weight file is downloaded, and removed with the account. A volume is a unit attribute like `secrets`, not a fifth object: it has no tool family, `proc_list` shows what a Process holds, and `pkg_inspect` shows what a Package asks for. kitbashOS becomes two images of one system. The Alpine image of 4.1 stays the reference and the default: control hosts, general nodes, and every machine without a GPU. A second image, kitbashOS GPU, is Ubuntu 24.04 LTS plus the same static `kitbashd`, shipped as a deb with a systemd unit, installed unattended by Ubuntu's own autoinstall the way the Alpine ISO uses Alpine's answer file, with the vendor's driver and the container toolkit on it and nothing else. Ubuntu because the vendor tests the driver, the CUDA stack, the container toolkit and every inference engine against Ubuntu LTS first, and a GPU host's failures are reproduced there; Debian would match Proxmox VE's base and is the fallback if the vendor's Ubuntu repository ever lags. The two images have the same seven components, the same surface, the same manifest and the same release: one `pkgver`, one commit, two packages and two ISOs from `release.yml`, one QEMU boot each. What differs is what 4.2 already allows to differ: the init that starts `kitbashd`, the package format, and the driver, which is the kernel's business and not an eighth component. Where the systemd host would hand a member to logind, the image keeps them out of it and makes `/run/user/<uid>` itself, as 2.3 requires. The GPU image is a full kitbashOS and not a node only image, because the first case is one GPU machine that should run everything, Packages, proxy and scheduler, by itself; M14 makes it a node as well. Nothing in Ubuntu is forked, as nothing in Alpine is, and what is removed in 4.3 is removed from both. On Alpine 4.3 is a list of things never installed; on Ubuntu it is a list of things removed, and the list is longer, because Ubuntu Server ships for an administrator at a terminal. The GPU image starts from the Ubuntu minimal cloud image rather than the server ISO, since installing less is steadier than removing more, and its build removes what is left: snapd and every snap, cloud-init after the one autoinstall run it exists for, unattended-upgrades, ubuntu-pro and its client, landscape, motd-news, apport, whoopsie and popularity-contest, because each of them phones out, updates itself or reboots the host on a schedule of its own, which an immutable host does not have; man pages, locales beyond C.UTF-8, bash-completion, editors and byobu, as 4.3 says; and whatever the driver metapackage pulls in that a headless host does not need, NetworkManager, ModemManager, avahi, cups, bluetooth and polkit, which the build refuses rather than removes. apt is not for members: the host is immutable and third party software is a Package, so apt runs in the image build and in a release and at no other time. What remains is the seven components with the driver beside them. The list is a script in `packaging/`, and the e2e boot of the GPU image asserts that none of the removed units exist and no snap is mounted, so the next Ubuntu release cannot grow them back unnoticed. The image is larger than the Alpine one by glibc, systemd, the kernel modules and the driver, which is the price of the GPU and not of Ubuntu. Done when the GPU image installs unattended on a machine with one GPU and `kitbashd` reports healthy, a member writes a Package whose unit declares `limits.gpu: 1`, a `volumes` entry for the weights and `expose: http`, runs it, and a document sent to its address comes back as Markdown; a second `proc_run` of a GPU unit on the same one GPU host is refused naming the first Process; the same manifest on the Alpine image is refused naming the device; and the release that ships it carries one apk, one deb and two ISOs from one `pkgver`.

**M14 Cluster.** The first trigger of 4.6 has fired, more than one machine, and the answer is not Kubernetes yet. A cluster is one kitbashd that owns the surface, Files, members, approvals, Secrets and the Telemetry store, and one or more nodes, each a kitbash host whose kitbashd owns only a runtime, joined to the control host by the same transport members use, an SSH key the control host holds, with `ForceCommand` on the node side too. A node holds no Files and no members of its own; it reports what it has, CPUs, memory, GPUs by index and which are held, images present, volumes present, and it starts, stops, probes and streams logs for Processes the control host places on it. `proc_run` gains placement: a unit goes to the node that satisfies its limits, `gpu` first because it is the scarce one, and a unit that names a node with `node:` goes there or is refused. The image travels with the placement, pushed from the control host's store to the node's, and the four step start of 2.3 runs on the node under a member the node creates from the control host's record. The proxy on the control host routes to the node's port over the cluster's network, Telemetry from a node's Processes lands in the control host's store with a `kitbash.node` attribute, and `proc_list` shows the node. A node that stops answering makes its Processes `unreachable` in `proc_list` and nothing else: the control host does not move them, because a model server that was warm on one GPU is cold on another and the owner decides. Why not Kubernetes on the nodes now, when it has solved every one of these. Because a node is a kitbash host and a kitbash host has seven components: k3s alone brings containerd, a network plugin, a datastore and an ingress, and the host stops being the one described in 4.2. And because the question has been answered before by a system this document already takes as its precedent. What M14 takes from Proxmox VE's cluster: joining is one command against the control host with an SSH key, which is the transport members already use; and the cluster's configuration is one authority, which for PVE is a replicated filesystem and for kitbash is the control host's SQLite, so a node carries no state that is not derived and a broken node is reinstalled rather than repaired. What it takes from Kubernetes: the registration is a desired state and the daemon converges to it, which is what boot restore already does with one field more, the node; the node agent is dumb, it reports what it has and starts, stops, probes and streams what it is told to, and every decision is made on the control host, as the kubelet does; and placement is a filter followed by a score, drop the nodes that cannot hold the unit, its `gpu`, `memory`, `cpu` and `node:`, then take the emptiest of the rest, which is two loops and not a scheduling framework. What it refuses, each one a subsystem not written: a replicated datastore and high availability, because a control host that is down leaves every Process on every node running and takes only new instructions with it, and quorum exists for failover kitbash does not do; overlay networking, because a Process publishes a host port and the control host's proxy forwards to it as it does today; an API machinery of resource kinds and controllers, because the manifest is already the schema; automatic rescheduling, for the reason given above; and autoscaling, which is the second trigger of 4.6 and the day Kubernetes becomes the runner. The one problem Kubernetes solved that a cluster cannot avoid is how an image reaches a node: the control host serves its image store read only on the OCI distribution protocol, over the same authenticated channel, and the node pulls by digest, so a placement is a pull and a start and nothing is copied twice. Kubernetes stays where 4.6 put it, as a run kit for the day the cluster is larger than one organization's machines or needs autoscaling; until then a Process already maps one to one onto a Deployment and the day it is needed the manifest does not change. Done when two hosts, an Alpine VM as control host and an Ubuntu GPU node, run as one cluster, a member on the control host runs the M13 Package and it lands on the node and answers at its address, `proc_list` names the node, `tel_query` by `kitbash.node` returns its spans, and pulling the node's network cable makes `proc_list` say `unreachable` within one probe interval and puts it back when the cable is.

**M15 Model gateway.** Serving many models to many agents means one address and a `model` field, the OpenAI shape, which every agent framework already speaks. That is a Package, not a built in: an `http` Process in `/org` whose permits are `proc_list`, `proc_run`, `proc_stop` and `packages`, which reads which running Processes declare a model, `provides.model: <name>` in the manifest, and forwards each `/v1` request to the Process that serves the model it names. A model no Process serves but a Package declares is started on demand through `proc_run` if the gateway's owner permits it, and a Process idle past the manifest's `idle` duration is stopped, so one GPU serves several models in turn and a GPU host with room serves several at once. The gateway writes one span per request with the four attributes and the model name, so "which model cost what this week" is a `tel_query`. Members reach it under the gateway owner's name as any `http` Process, and a member's own key is the gateway's `secrets` entry the way it is for any service; per member keys, budgets and rate limits are the gateway's business and stay out of kitbashd. Done when an agent with only the gateway's address and one key sends an OCR request and a chat request naming two models, the OCR Process was running and the chat Process was not, both answer, `proc_list` shows the chat Process started by the gateway, it stops after the idle duration, and `tel_query` returns both requests with their model names.

Two things these milestones do not do. They do not put a Windows host in a cluster: the OCR service of 2026-09-20 stays outside until it runs on a Linux node, and a PVE runner kit remains the way to treat a VM as a Process. And they do not add a fifth object: a volume and a node are attributes of a unit and of a placement, and the gateway is a Package.

When M14 begins, the "more than one machine" line of 5.2 and the "any runner other than rootless podman" line stop being true and are removed from 5.2 in that pull request rather than in this one.

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
| kitbashOS | Alpine plus kitbashd; from M13 also Ubuntu LTS plus kitbashd as the GPU image, see 5.6 |
| node | A kitbash host in a cluster that runs Processes for the control host and holds no Files or members of its own, see 5.6 |
| volume | A directory kitbashd keeps for a member outside Files and outside the image, declared by a unit, see 5.6 |
