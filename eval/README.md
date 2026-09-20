# eval

What M10 measured by hand is a fixture here. Fifteen sentences in three
classes, one agent, one member the round creates and removes, one row per run.
PLAN.md 5.3 M11 is the milestone this folder is; 4.5 is what its numbers are
read against.

The bench runs on an operator's machine, because it needs the agents'
credentials. Nothing here runs in CI except the unit tests under `tests/`,
which touch no network and no host.

## A round

```sh
python3 eval/bench.py run --agent claude --k 1
python3 eval/bench.py run --agent codex --k 1 --only todo-board,fault-stopped
python3 eval/bench.py report eval/results/20260919-claude-0.13.1
```

`run` takes:

| Option | What it names |
|---|---|
| `--agent` | `claude` or `codex` |
| `--host` | the ssh alias of a member session on the host, `kitbash-mcp` by default |
| `--admin` | the ssh alias of an admin's MCP session, which creates and removes the round's member |
| `--root` | the ssh alias of a root shell on the host, which is where a fault is injected |
| `--k` | how many times each sentence runs |
| `--only` | sentence ids, comma separated |
| `--classes` | `nothing`, `repository`, `fault` |
| `--timeout` | seconds one run may take, 1500 by default |
| `--keep-member` | leave the member in place at the end |

What a round does, in order:

1. Reads the host's kitbash version and domain over `--root`.
2. Creates `bench-<yyyymmdd-hhmm>` through `users_create` as the admin, with a
   fresh ed25519 key it generates in the round folder at mode 0600, and writes
   an ssh config beside it that reaches the host as that member. The jump host
   is written out too, because a `ProxyJump` naming an alias is resolved by a
   second ssh that does not read this file.
3. Runs every sentence k times as that member. Each run gets a fresh agent
   configuration directory, one MCP server and no other, and a tool list.
4. Verifies each run with the sentence's check, which reads the outcome on the
   host and never the transcript.
5. Stops the member's Processes, clears the immutable flag the mount fault
   sets, removes the member with `users_remove`, which takes their Processes,
   files and secrets with them, and deletes the key. The two steps before the
   removal are there because of what the first round found: a removal of a
   member holding seventeen Processes outruns `kitbash-mcp`'s client deadline,
   and the archive it makes chowns the home, which an immutable file refuses.
   A removal that answers an error is believed only after `users_list` is
   asked, because the call can fail once the account is already gone.

A round is resumable. A run whose row is already on disk is skipped, and the
member and its key are named in `round.json`, so a round cut off by a usage
limit continues with the same member. The member is removed only when every
sentence of the round has run; with `--only` or after a failure it stays.

## The sentences

`sentences.json` holds fifteen, five to a class.

- **From nothing.** A web application, a scheduled job, a service, a link
  shortener and a notes wiki. The first three are M10's own sentences word for
  word, except the address the job posts to: M10 named a member's own board and
  the rounds wrote onto it, so the job now posts to a `bench-board` the round
  deploys for it. The job passes on the registration, because its first tick is
  the next morning; the board is read as an observation, so a row records
  whether that agent proved the job by posting to it by hand.
- **From a repository.** Five real GitHub addresses and "get it running for my
  classmates", the shape EnvBench and SetupBench measure. Three are the ones
  M10 tried by hand. Two were added for what they need beyond a clone:
  `postgresql-pgadmin` needs two credentials in the environment and a volume,
  and `filebrowser` is one container that is useless without a data volume.
- **From a fault.** A container stopped, a mount made unwritable, a secret
  revoked, an image removed and a dependency down, the shape AIOpsLab and
  ITBench measure. Each is injected by the harness against a Process the round
  deployed itself first, so a fault sentence is self contained: the fixture
  Packages are under `fixtures/`, the round writes, builds and runs them as the
  member, waits until the address answers, and only then breaks it. A sentence
  outside this class may name a `setup` too, which is the same deploy without
  the fault: the scheduled job posts onto a board deployed that way.

Every sentence carries a `check`, a small function keyed by name in
`benchlib/checks.py` with its parameters written beside it in the sentence
file:

| Check | What it reads |
|---|---|
| `http_ok` | the status and body of the address a Process of this run is served at |
| `http_post_roundtrip` | an item posted to one path comes back from another |
| `http_item_posted` | an item the run put on the round's own board is on it, by author |
| `proc_running` | `proc_list` says a Process is up |
| `proc_scheduled` | `proc_list` says a job is registered with a cron expression |
| `tel_schedule` | a `kitbash.schedule` record exists since the round started |
| `all_of` | every check in the list, the first failure being the reason |

A sentence may also carry `observe`, a mapping of name to check that is read
after the outcome and decides nothing. It lands on the row as
`observed: {name: {observed, detail}}`, beside `passed` and never part of it,
which is how a round reports something a run cannot be failed for, such as the
weather job having posted to its board within the run.

A check on a sentence the member wrote freely looks only at Processes that did
not exist before the run, so the previous sentence's service cannot pass this
one. A check on a fault names the Process by name, and asks its address even
when the listing no longer has it, because the address is what the user holds.

## A row

One JSON file per run, `<id>-<n>.json`, in the round folder.

| Field | What it means |
|---|---|
| `id`, `class`, `run` | which sentence, which class, which of the k runs |
| `agent`, `model` | the agent and the model it reported |
| `kitbash_version` | the host's kitbashd, from `apk list -I kitbashd` |
| `member` | the member the round created |
| `turns`, `tool_calls` | what the transcript counted |
| `kitbash_calls`, `kitbash_calls_by_tool` | calls to the kitbash surface, and which tools |
| `tool_errors` | tool results that came back an error |
| `questions_asked` | sentences ending in a question mark in the agent's last answer |
| `asks_user` | whether that answer hands a decision back, by a question or a phrase |
| `cost_usd` | what the client charged, null for an agent that reports no price |
| `wall_ms` | how long the run took |
| `passed`, `check` | the check's answer and its detail |
| `time_to_mitigate_ms` | for a fault, from the injection to the first passing check, polled every 15 seconds |
| `inject_verified` | whether the check failed after the injection, so the fault took |
| `usage`, `tools`, `result_text` | the rest of the accounting, and the agent's last answer |

`report` writes `README.md` in the round folder: one table per class, pass^k
per sentence, totals, and the three readings of 4.5 restated with the round's
numbers. pass^k is the sentence every one of k runs passed.

The transcripts stay in `runs/<id>-<n>/` beside the rows. The agents'
configuration directories are deleted when a run ends, because they hold
credentials, and `.gitignore` names them as well as the round's key.

## The agents

Both are behind one interface in `benchlib/adapters.py`, with a fake that
replays a captured transcript for the tests.

**Claude Code.** `claude -p <sentence> --output-format stream-json --verbose
--strict-mcp-config --mcp-config <one server> --setting-sources ""
--permission-mode acceptEdits --max-turns 80 --allowedTools <list>`, with
`CLAUDE_CONFIG_DIR` fresh per run and the OAuth access token read from the
Keychain, which is never written to a file the round keeps.

**Codex CLI.** `codex exec --json --skip-git-repo-check --ignore-rules -C
<workdir> --approve-for-me -c sandbox_workspace_write.network_access=true
--output-last-message <file> <sentence>`, with `CODEX_HOME` fresh per run
holding a copy of the machine's `auth.json` and a `config.toml` naming one
stdio MCP server, `ssh <alias>`. Two things have to be said out loud:

- Codex reads its credentials from `CODEX_HOME` alone, so a clean home without
  a copy of `auth.json` is a logged out agent.
- With `approval_policy = "never"` every MCP tool call answers "requires
  approval, but approval policy is never" and the agent never reaches the host.
  `--approve-for-me` reviews each request automatically inside the
  workspace-write sandbox, which is what makes a headless round possible.

What cannot be read from Codex: a price, which it does not report, so
`cost_usd` is null and the token counts stand in its place; the model, which is
not in the event stream and is read from the rollout file under `CODEX_HOME`,
dropping the automatic reviewer's own; and a turn count of Claude's shape, so
`turns` is counted as the model's messages plus its tool calls. The comparable
column across the two agents is `tool_calls`.

## The tests

```sh
python3 -m unittest discover -s eval/tests -t eval/tests
```

No network, no host, no agent. They cover the sentence file (ids unique, every
check name exists, every fault has an injection and a fixture to break, no
prompt names an address outside the round, no observation is part of a check),
stream
parsing on a captured transcript with its paths redacted, row computation,
report rendering on fixture rows, and the questions asked heuristic on three
answers.
