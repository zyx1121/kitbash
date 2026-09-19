#!/usr/bin/env python3
"""The M11 bench. One sentence, one agent, one row.

  bench.py run --agent claude --k 1
  bench.py report eval/results/<round>

Standard library only, because it runs on an operator's machine beside the
agents' credentials and has nothing to install.
"""

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from benchlib import adapters, mcp, report, round as roundlib  # noqa: E402

SENTENCES = os.path.join(HERE, "sentences.json")
RESULTS = os.path.join(HERE, "results")


def command_run(options):
    sentences = roundlib.load_sentences(options.sentences)
    if options.only:
        wanted = [name.strip() for name in options.only.split(",") if name.strip()]
        unknown = [name for name in wanted if name not in {s["id"] for s in sentences}]
        if unknown:
            raise SystemExit("no sentence named %s" % ", ".join(unknown))
        sentences = [s for s in sentences if s["id"] in wanted]
    if options.classes:
        classes = {c.strip() for c in options.classes.split(",")}
        sentences = [s for s in sentences if s["class"] in classes]
    current = roundlib.Round(options)
    folder = current.prepare()
    current.log(
        "round %s: %d sentences, k=%d, agent %s, kitbash %s, member %s"
        % (current.state["round"], len(sentences), options.k, options.agent, current.version, current.state["member"])
    )
    adapter = adapters.build(options.agent, model=options.model)
    complete = True
    for sentence in sentences:
        for number in range(1, options.k + 1):
            try:
                current.run_one(sentence, number, adapter)
            except KeyboardInterrupt:
                current.log("interrupted; the round is resumable with the same member")
                current.save_state()
                raise
            except Exception as exc:  # a broken sentence must not end the round
                complete = False
                current.log("%s run %d could not be run: %s: %s" % (sentence["id"], number, type(exc).__name__, exc))
    current.save_state()
    path = report.write(folder, current.state)
    current.log("report written to %s" % path)
    if options.keep_member:
        current.log("keeping member %s as asked" % current.state["member"])
    elif complete and not options.only and not options.classes:
        current.teardown()
    else:
        current.log("not every sentence ran, keeping member %s so the round can be resumed" % current.state["member"])
    report.write(folder, current.state)
    return 0


def command_report(options):
    folder = options.folder
    state = {}
    state_path = os.path.join(folder, "round.json")
    if os.path.exists(state_path):
        with open(state_path) as handle:
            state = json.load(handle)
    path = report.write(folder, state)
    print(path)
    return 0


def command_teardown(options):
    folder = options.folder
    with open(os.path.join(folder, "round.json")) as handle:
        state = json.load(handle)
    name = state.get("member")
    answer = mcp.call_once(options.admin, "users_remove", {"name": name})
    print(json.dumps(answer))
    for suffix in ("", ".pub"):
        path = (state.get("key") or "") + suffix
        if path and os.path.exists(path):
            os.remove(path)
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description="the M11 bench")
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="run a round")
    run.add_argument("--agent", choices=["claude", "codex"], required=True)
    run.add_argument("--host", default="kitbash-mcp", help="ssh alias of a member session on the host")
    run.add_argument("--admin", default="kitbash-mcp", help="ssh alias of an admin's MCP session")
    run.add_argument("--root", default="kitbash", help="ssh alias of a root shell on the host, for injecting faults")
    run.add_argument("--k", type=int, default=1)
    run.add_argument("--only", default="", help="sentence ids, comma separated")
    run.add_argument("--classes", default="", help="nothing, repository, fault")
    run.add_argument("--model", default=None, help="the agent's model, its own default without one")
    run.add_argument("--timeout", type=int, default=1500, help="seconds one run may take")
    run.add_argument("--sentences", default=SENTENCES)
    run.add_argument("--results", default=RESULTS)
    run.add_argument("--round", default="", help="resume this round folder name")
    run.add_argument("--keep-member", action="store_true", help="do not remove the member at the end")
    run.set_defaults(function=command_run)

    render = sub.add_parser("report", help="write README.md for a round")
    render.add_argument("folder")
    render.set_defaults(function=command_report)

    remove = sub.add_parser("teardown", help="remove a round's member by hand")
    remove.add_argument("folder")
    remove.add_argument("--admin", default="kitbash-mcp")
    remove.set_defaults(function=command_teardown)

    options = parser.parse_args(argv)
    return options.function(options)


if __name__ == "__main__":
    raise SystemExit(main())
