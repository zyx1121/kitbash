# The surface

Everything on this machine is 25 built in tools in seven families, plus the tools
of the Processes the member is running. Names are `<family>_<verb>`. Input and
output are JSON Schema 2020-12, timestamps are RFC 3339, and identifiers are
UUIDv7 unless they are git SHAs or OCI digests.

## fs

Files. Every top level folder under `/org` and the member's home is a git
repository.

| Tool | What it does |
|------|-------------|
| `fs_list` | Visible folders and the files in one path, or the roots when no path is given. A root answers path, name and description per folder and no files; a folder answers its subfolders and its files as a name and a size |
| `fs_read` | One file: text as text, PNG and JPEG as an image, PDF as extracted text. A folder is `invalid-path` with a fix naming `fs_list` |
| `fs_write` | Create or replace one file, or up to 64 of them in `files`, and commit them to the enclosing repository as one commit. A path that names a folder is `invalid-path`: name the file |
| `fs_history` | The commits that touched a path, newest first |

Write every file of a Package in one `fs_write`: `files` takes a list of
`{path, content}` under one top level folder with one `message`, and the whole
list is one commit. A list is refused whole, so one path the caller may not
write leaves nothing behind. `expectedSha` is the optimistic lock, read against
the file for one path and against the repository's head for a list.

## pkg

Packages. A folder with a `deploy` block, built to or imported as an OCI image.

| Tool | What it does |
|------|-------------|
| `pkg_build` | Build the Package at a path from its current commit. Returns the digest, the commit and the last 20 lines of the build log |
| `pkg_import` | Wrap something external as a Package folder through a running import kit. Writes the folder, does not build |
| `pkg_list` | Visible Packages: path, name and the digest of the latest build |
| `pkg_inspect` | The manifest and the build history of one Package |

## proc

Processes. A Package running as a rootless container under the caller.

| Tool | What it does |
|------|-------------|
| `proc_run` | Start a Process from a digest, or converge an existing one to it. With `expose: mcp` its tools join the surface |
| `proc_list` | The caller's Processes, running or stopped. With a `package`, that Package's Processes in full; without one, a line each, carrying `url` for an http Process and `schedule` and `nextRun` for a job |
| `proc_stop` | Stop a Process. It stays known, and its tools leave the surface |
| `proc_logs` | The recent stdout and stderr of a Process, interleaved as the runtime wrote them |

A unit with `expose: http` gets an address when the host has a domain:
`proc_run` answers `url` and `proc_list` shows it, `https://<name>.<member>.<domain>`,
where the member's label is the attribution. kitbashd is the reverse proxy in
front of it and routes by that name to the port the Process's own container
publishes. A unit may declare `hostname` in `deploy.units[]` instead, one name
outside the host's domain whose DNS record points at this host, and
`pkg_inspect` shows it with the rest of the manifest. Under the domain there
are derived names and nothing else, so a `hostname` there is
`invalid-manifest`; one another Process already serves is `conflict`; and one
that does not resolve to this host, checked again at every start, is
`not-permitted` naming what it resolved to. A host with no domain answers no `url` and
the Process keeps its internal port.

A unit with `expose: none` may declare `schedule` in `deploy.units[]`, five cron
fields read in UTC, and then it is a job: `proc_run` registers it, starts
nothing, and answers `state: scheduled` with the `nextRun` kitbashd will start
it at. A line of `proc_list` carries `schedule` and `nextRun`, so confirming a
job is one call, and asking by `package` adds the `lastRun` of a job that has
run.
kitbashd starts the container at each tick as the owner, with the same
environment, secrets, mounts and ceiling any start gets, and every tick writes
one `kitbash.schedule` record, so "did it run this morning" is a `tel_query`.
A tick that arrives while the previous run is still going is skipped and
recorded, a host runs at most 8 scheduled containers at once and one member at
most 3 of those, a tick beyond either is skipped as busy, a run longer than 6
hours is stopped and recorded with exit code 137, ticks missed while the host
was down are not made up, and a member holds at most 32 jobs. Every run is given
a token of its own, revoked when the container exits. `proc_logs` reads the last run, whose container is
kept until the next tick replaces it, and `proc_stop` unregisters the schedule
and removes it. A `schedule` on any other `expose`, or beside `health`,
`restart` or `subscriptions`, is `invalid-manifest`.

## tel

Telemetry. OTLP in, queryable out.

| Tool | What it does |
|------|-------------|
| `tel_query` | Spans, logs or metrics by attribute and time range, newest first |
| `tel_retention` | Read the window per signal; an admin sets it |

## users

Members. Linux users with SSH keys. Admin only except `users_me`.

| Tool | What it does |
|------|-------------|
| `users_me` | Who the caller is and which groups they are in |
| `users_create` | Create a member with a key, a private home and a subordinate id range |
| `users_list` | Every member with uid, admin flag, key count and Process count, and how far each removal got |
| `users_add_key` | Add an SSH public key to a member |
| `users_remove` | Start removing a member: kitbashd stops their Processes, archives their home and deletes the account on its own time |

## approvals

The replacement for sudo.

| Tool | What it does |
|------|-------------|
| `approvals_list` | Approvals by state. Members see their own, admins see all |
| `approvals_approve` | Approve and execute a queued call. Admin only |
| `approvals_reject` | Reject a queued call with a reason the requester reads |

## secrets

The values a member gives their own Processes. A unit declares the names it
needs in `deploy.units[].secrets`, and kitbashd resolves each one to the
owner's current value at every start. They are the member's own: an admin holds
their own set and reads nobody else's.

| Tool | What it does |
|------|-------------|
| `secrets_set` | Set one value. Creating and replacing are the same call, so a rotation is one call; it restarts nothing |
| `secrets_list` | The names the caller holds and when each was last written. Never a value |
| `secrets_remove` | Drop one. Idempotent: a name the caller does not hold answers `removed: false` |

A value is one line: at most 8192 bytes, no NUL and no line break, because the
environment file a Process is given is line based. A name matches
`^[A-Z][A-Z0-9_]{0,63}$`, may not be one the unit's `env` also sets and may not
be a `KITBASH_` name, which kitbashd speaks for; a manifest that does either is
`invalid-manifest`. A declared name the owner has not set is `not-found` at
`proc_run`, naming the secret.

## Paths

Two roots: `/org`, which every member reads, and `/home/<caller>`, which is the
caller's alone. No path outside them is reachable, and no member ever sees
another member's home.

- A folder is visible when it, or a folder between it and its root, carries a
  `kitbash.yaml` with a `name` and a `description`, and the nearest of those
  manifests speaks for it. The folders inside a Package, `src`, `public`,
  `deploy`, need none of their own; a nested `kitbash.yaml` only begins a new
  description where one is wanted. A folder with no manifest anywhere above it
  does not exist as far as this surface is concerned, so a top level folder is
  written manifest first. The same rule decides what a Process may be given as
  a mount.
- Paths are absolute. A path with `..` in it is `invalid-path`.
- Symbolic links are never followed. Every open below a root resolves with
  `RESOLVE_NO_SYMLINKS` and `RESOLVE_BENEATH`, so a link swapped in during a
  call is refused by the kernel.
- Every path component beginning with a dot is refused, which is why `.git` and
  `/org/.archive` cannot be read here.
- Access control is the kernel's. The session runs as the member's own Linux
  user, and sharing between members is Linux groups on folders.

## Errors

A failed call is a tool result with `isError: true` whose single text block is
an RFC 9457 problem: `type`, `title`, `status`, `detail`, `instance` and a
`fix`. The `type` is `https://kitbash.zyx.tw/errors/<slug>`. Read the `fix`
first; it says what to do next.

| Slug | Status | When |
|------|--------|------|
| `not-found` | 404 | Nothing is there |
| `not-visible` | 404 | The folder is there and carries no manifest |
| `not-permitted` | 403 | The caller may not do this. 401 on the Process receiver |
| `bad-request` | 400 | Malformed input, or a Package refused the call |
| `invalid-path` | 400 | Outside the roots, or `..`, a symlink or a dot component |
| `invalid-manifest` | 422 | The `kitbash.yaml` fails `spec/manifest.schema.json` |
| `unsupported-media-type` | 415 | `fs_read` cannot render this media type |
| `too-large` | 413 | Over a limit, such as the 1 MiB one file read |
| `conflict` | 409 | A raced commit, a taken name, an uncommitted change. 429 for too many sessions |
| `queued` | 202 | The call waits in the approval queue; `instance` is the approval id |
| `internal` | 500 | The daemon or the host failed. The cause is in the server log |

## Approvals

A member's `fs_write` or `pkg_import` under `/org` does not fail. It is queued,
and the answer is a `queued` problem at 202 whose `instance` is the approval id.
Nothing else outside the caller's own space is queued; it is simply not
permitted.

1. The member calls the tool and keeps the approval id.
2. An admin reads the queue with `approvals_list`.
3. `approvals_approve` runs the call in the admin's session as the admin's Linux
   user. The commit is authored by the requester with a trailer naming the admin
   who approved.
4. The result, a normal output or a problem, is stored on the approval, so the
   requester reads it back with `approvals_list`. `approvals_reject` stores the
   reason instead.

A member holds at most 64 pending approvals.

## Telemetry

Every call on this surface is one span, opened before the handler runs, so
nothing here is untraced. Builds and Process starts are child spans, and a build
also writes one log record carrying the tail of its log. A Process that reads
its token is a producer too.

Every record carries `user`, `package`, `process` and, where relevant, `path`,
plus `tool` for a surface span. `user` is never taken from a producer: kitbashd
stamps it from the connection the record arrived on.

`tel_query` takes a `signal` of `traces`, `logs` or `metrics` and filters:

| Filter | Answers |
|--------|---------|
| `user` | What a member did. It defaults to the caller; naming another member is admin only |
| `package` | How one Package behaves, across every Process of it |
| `process` | What one running Process did |
| `path` | What happened to a Files path, matched by prefix |
| `tool` | Every call of one surface tool |
| `caller` | What a Process did through `/mcp` on its owner's behalf |
| `producer` | Who wrote the record: a member for a session, a Process id for a Process |
| `eval` | Only the judgments an evaluation kit wrote back |

`since` defaults to 24 hours ago and `until` to now; `limit` is at most 1000 and
the answer says whether it was truncated. Retention is traces 30 days, logs 14
days and metrics 30 days, swept hourly.

## The surface a Process sees

A Process reaches this same surface at `KITBASH_MCP_ENDPOINT`, MCP over
streamable HTTP with its own token as the bearer. It sees exactly what its owner
sees, under the same kernel rules, so a Process is an agent with no code path of
its own. Every span it records carries `kitbash.caller`, which is how one query
separates what a Process did from what the member did.

Sessions there are capped at 8 per Process, a session idle for 10 minutes ends,
and a POST without a session must be an `initialize`. That `initialize` answers
with the same instructions a member's session is given, because the session is a
proxy of the same `kitbash-mcp`.
