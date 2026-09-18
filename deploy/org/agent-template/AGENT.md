# agent-template

A Process that is an agent. Its one tool, `run`, takes a task in words, calls
the tools of your own MCP surface until the task is done, and answers with the
result. The tools it calls are the ones this Package permits and nothing else,
so what the agent can do is a line in a manifest you can read.

This file is the prompt the kit declares, which means the agent reads it at the
start of every run, after its own standing paragraph and before the `system`
text of the call. Edit it in your copy of the folder and you have changed what
the agent knows.

## Running it

The key comes first, because a Process with no key answers a problem rather than
a result:

1. `secrets_set` with `{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-..."}`.
   The value lives with kitbashd, as a root owned file outside the database, and
   never in Files, in this image or in the manifest. It is resolved into this
   container's environment at every start and nowhere else, PLAN.md 2.3.
2. `proc_run` with `{"package": "/org/agent-template"}`.
3. `agent-template_run` with `{"task": "..."}`.

A key set after the Process started is not in its environment: `proc_stop` and
`proc_run` again, which is what the problem says as well. Rotating the key is
the same two calls.

```json
{"task": "Read /home/you/notes/inbox.md, group what is in it by project, and write the groups to /home/you/notes/by-project.md."}
```

The answer:

```json
{"answer": "Wrote four groups...", "stopReason": "end_turn",
 "steps": [{"tool": "fs_read", "durationMs": 41, "ok": true},
           {"tool": "fs_write", "durationMs": 66, "ok": true}],
 "usage": {"inputTokens": 24680, "outputTokens": 1204}, "model": "claude-opus-5"}
```

`steps` is what it called, in order. `ok` is false for a call that answered a
problem: the agent was told and carried on, so a false there is not a failed
run. `stopReason` is `end_turn` when it answered, `max_iterations` when it ran
out of turns before it did, `max_tokens` when the final message was cut, and
`refusal` when the model declined, which leaves `answer` empty.

**A run is synchronous.** One call can take minutes, because a turn is an API
request and a hard task is many turns. Nothing polls and nothing resumes: a task
too large for one run is two tasks, and a task whose result another run needs is
a file the first one wrote. `AGENT_MAX_ITERATIONS` is the ceiling on the turns
one run may take, 32 by default and 256 at the most, and hitting it is
`max_iterations` with whatever had been said by then.

## What it may call

`provides.permits` in `kitbash.yaml` is the whole answer, and the surface
refuses anything outside it before the call runs:

- `fs_list`, `fs_read`, `fs_write`, `fs_history` under `/org` and `/home/*`. It
  reads and writes Files, and `/org` is read only for everyone.
- `pkg_list`, `pkg_inspect`, `proc_list`, `proc_logs`, `tel_query`. It sees what
  is installed, what is running and what any of it did.
- `packages`, which is every tool your running Processes publish. The agent
  composes your other kits; it calls no built in that is not in this list.

**It does not delegate to itself.** `packages` is every tool of every Process
you have running, this one included, so the surface hands this Package its own
`run` back. The kit drops it from what the model is given: an agent that could
call itself would start a whole new loop inside one of its own turns, as deep as
it liked, and one call would have to wait for all of them. Another agent Process
you started is a different Package and stays on the list, so delegating to one
is a Package you run, not a loop inside this one.

It cannot `proc_run`, `pkg_build`, `secrets_set`, `users_*` or
`approvals_approve`. An agent that could start Processes, build images and
approve its own writes to `/org` would be an admin by default, and this one is a
template that anyone may copy. If you want those, copy the folder and add them,
knowing what you are adding.

## Making your own

The folder is the template. Nothing here is special to `/org`:

1. `fs_read` each file of `/org/agent-template` and `fs_write` it under your
   home, say `/home/you/researcher`, changing `name` in `kitbash.yaml`.
2. Edit this file. It is where the agent's standing instructions belong: what
   this agent is for, which folders it works in, what it must never touch, the
   shape you want its answers in.
3. Edit the unit's `env` for the model and the loop. `AGENT_MODEL` defaults to
   `claude-opus-5`; the other current models are `claude-sonnet-5`, which is
   faster and cheaper, and `claude-fable-5-1`. `AGENT_EFFORT` is `low`,
   `medium`, `high`, `xhigh` or `max` and defaults to `high`.
   `AGENT_MAX_ITERATIONS` defaults to 32 and is read between 1 and 256; a value
   outside that, or one this kit cannot read at all, is ignored and the default
   runs, which it says on stderr. Rename the folder, `name` in `kitbash.yaml`
   and `name` in `package.json` together: the last is how the Process knows
   which tool on the surface is its own.
4. Edit `provides.permits` for what this agent may reach. Narrow is the point:
   an agent that only reads needs `fs_read` and `fs_list`, and one that drives
   your other kits needs `packages`.
5. `pkg_build` the folder, then `proc_run` it. Its tool joins the surface as
   `<name>_run`, so two agents of different names run side by side.

`secrets` stays `[ANTHROPIC_API_KEY]`, and the value is already yours: every
Process you run of every copy reads the same one.

## What it costs and what it says

`usage` is the whole run, every turn summed, cache reads and cache writes
counted as input. The system prompt is cached, so a second run of the same
Process reads most of its prefix from the cache.

Nothing this kit writes carries the key, this Process's token or a request body,
in a problem or in a log line. `proc_logs` shows one line per run with the stop
reason, the counts and the timing, and `tel_query` shows every call the agent
made as a span carrying `kitbash.caller` with this Process's id, so what the
agent did on your behalf is one query.
