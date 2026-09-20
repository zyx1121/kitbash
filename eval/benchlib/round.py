"""One round: a member the round creates, fifteen sentences, one row per run.

The round is resumable. A run whose row is already on disk is skipped, so a
round cut off by a usage limit continues with the same member, and the member
is removed only when every row exists.
"""

import datetime
import json
import os
import shutil
import threading
import time

from . import adapters, checks, fixtures, hostops, mcp, rows

POLL_SECONDS = 15
CHECK_ATTEMPTS = 3
CHECK_DELAY = 10
HEALTHY_SECONDS = 300


class UsageLimit(Exception):
    """The agent's account is out of allowance. Nothing was measured."""


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def load_sentences(path):
    with open(path) as handle:
        return json.load(handle)["sentences"]


class Round:
    def __init__(self, options):
        self.options = options
        self.root_alias = options.root
        self.admin_alias = options.admin
        self.host_alias = options.host
        self.log_handle = None

    # ---------------------------------------------------------------- setup

    def prepare(self):
        self.version = hostops.kitbash_version(self.root_alias)
        self.domain = hostops.domain(self.root_alias)
        stamp = datetime.datetime.now().strftime("%Y%m%d")
        name = self.options.round or "%s-%s-%s" % (stamp, self.options.agent, self.version)
        self.folder = os.path.join(self.options.results, name)
        os.makedirs(os.path.join(self.folder, "runs"), exist_ok=True)
        self.log_handle = open(os.path.join(self.folder, "round.log"), "a")
        state_path = os.path.join(self.folder, "round.json")
        self.state = {}
        if os.path.exists(state_path):
            with open(state_path) as handle:
                self.state = json.load(handle)
        self.state.setdefault("round", name)
        self.state.setdefault("agent", self.options.agent)
        self.state.setdefault("k", self.options.k)
        self.state.setdefault("started_at", now())
        self.state["kitbash_version"] = self.version
        self.state["domain"] = self.domain
        self.state["host"] = self.host_alias
        self.state["admin"] = self.admin_alias
        self.member()
        self.save_state()
        return self.folder

    def log(self, line):
        text = "%s %s" % (now(), line)
        print(text, flush=True)
        if self.log_handle:
            self.log_handle.write(text + "\n")
            self.log_handle.flush()

    def save_state(self):
        with open(os.path.join(self.folder, "round.json"), "w") as handle:
            json.dump(self.state, handle, indent=1, sort_keys=True)

    def member(self):
        """The round's member: created once, reused on a resume, removed at the end."""
        name = self.state.get("member")
        key = self.state.get("key") or os.path.join(self.folder, "id_ed25519")
        config = os.path.join(self.folder, "ssh_config")
        if name and self.member_exists(name) and os.path.exists(key):
            self.log("member %s is still there, reusing it" % name)
        else:
            name = "bench-%s" % datetime.datetime.now().strftime("%Y%m%d-%H%M")
            public = hostops.generate_key(key, "kitbash bench %s" % name)
            self.log("creating member %s" % name)
            answer = self.admin_call("users_create", {"name": name, "sshKey": public})
            self.state["uid"] = answer.get("uid")
            self.state["member"] = name
            self.state["key"] = key
            self.state["member_created_at"] = now()
        self.state["member"] = name
        self.state["key"] = key
        self.state["ssh_config"] = config
        self.alias = hostops.write_ssh_config(config, self.host_alias, name, key)
        self.state["alias"] = self.alias
        if not self.state.get("uid"):
            self.state["uid"] = self.uid_of(name)
        return name

    def member_exists(self, name):
        try:
            answer = self.admin_call("users_list", {})
        except mcp.McpError:
            return False
        return any(u.get("user") == name for u in answer.get("users") or [])

    def uid_of(self, name):
        answer = self.admin_call("users_list", {})
        for user in answer.get("users") or []:
            if user.get("user") == name:
                return user.get("uid")
        return None

    def admin_call(self, tool, arguments=None):
        return mcp.call_once(self.admin_alias, tool, arguments)

    def session(self):
        return mcp.Session(mcp.ssh_command(self.alias, self.state["ssh_config"]))

    def context(self, pre_ids=None, since=None, variables=None):
        return checks.Context(
            self.alias,
            self.state["ssh_config"],
            self.state["member"],
            self.domain,
            pre_ids=pre_ids,
            since=since,
            variables=variables,
        )

    # ------------------------------------------------------------- one run

    def variables(self, sentence):
        member = self.state["member"]
        setup = sentence.get("setup") or {}
        package = setup.get("package")
        depends = setup.get("depends") or {}
        variables = {"member": member, "domain": self.domain}
        if package:
            variables["pkg"] = package
            variables["url"] = "https://%s.%s.%s" % (package, member, self.domain)
        if depends.get("package"):
            variables["dep_pkg"] = depends["package"]
            variables["dep_host"] = "%s.%s.%s" % (depends["package"], member, self.domain)
            variables["dep_url"] = "https://%s" % variables["dep_host"]
        return variables

    def resolve(self, value, variables):
        if isinstance(value, str):
            return fixtures.fill(value, variables)
        if isinstance(value, list):
            return [self.resolve(v, variables) for v in value]
        if isinstance(value, dict):
            return {k: self.resolve(v, variables) for k, v in value.items()}
        return value

    def setup_fault(self, sentence, variables):
        """Deploy what the fault will break, heal it, and wait until it answers."""
        setup = sentence["setup"]
        member = self.state["member"]
        report = {"package": setup["package"], "fixture": setup["fixture"]}
        with self.session() as session:
            for name, value in (setup.get("secrets") or {}).items():
                session.call("secrets_set", {"name": name, "value": value})
            for command in self.resolve(setup.get("reset") or [], variables):
                hostops.ssh(self.root_alias, command)
            depends = setup.get("depends")
            if depends:
                self.log("  deploying dependency %s" % depends["package"])
                fixtures.deploy(session, member, depends["fixture"], depends["package"], variables, self.log)
            self.log("  deploying %s" % setup["package"])
            fixtures.deploy(session, member, setup["fixture"], setup["package"], variables, self.log)
        context = self.context(variables=variables)
        spec = self.resolve(sentence["check"], variables)
        healthy = fixtures.wait_healthy(context, spec, HEALTHY_SECONDS)
        if not healthy.get("passed"):
            # A Process can be running and answer 502, which the first Codex
            # round hit, so it is stopped and run again once before the round
            # gives up on breaking something that worked.
            self.log("  the fixture does not answer yet, running it again: %s" % healthy.get("detail"))
            home = "/home/%s" % member
            with self.session() as session:
                for package in [(setup.get("depends") or {}).get("package"), setup["package"]]:
                    if package:
                        fixtures.start(session, "%s/%s" % (home, package), package, self.log)
            healthy = fixtures.wait_healthy(context, spec, HEALTHY_SECONDS)
        report["healthy_before"] = healthy
        if not healthy.get("passed"):
            self.log("  the fixture does not answer before the fault: %s" % healthy.get("detail"))
        return report

    def setup_fixture(self, sentence, variables):
        """Deploy what a sentence posts to, with nothing broken afterwards.

        A fault deploys a fixture so there is something to break; this is the
        same deploy without the fault, so a sentence that names an address
        names one inside the round. Nothing else about the run changes: the
        Process is registered before the agent starts, so it is not one of the
        Processes the run made.
        """
        setup = sentence["setup"]
        report = {"package": setup["package"], "fixture": setup["fixture"]}
        self.log("  deploying %s" % setup["package"])
        with self.session() as session:
            fixtures.deploy(session, self.state["member"], setup["fixture"], setup["package"],
                            variables, self.log)
        spec = {"name": "http_ok", "params": {"process": setup["package"], "path": "/", "status": 200}}
        healthy = fixtures.wait_healthy(self.context(variables=variables), spec, HEALTHY_SECONDS)
        report["healthy_before"] = healthy
        if not healthy.get("passed"):
            self.log("  the fixture does not answer before the run: %s" % healthy.get("detail"))
        return report

    def inject(self, sentence, variables):
        """Break it, over ssh or over the member's own surface."""
        inject = sentence["inject"]
        kind = inject.get("kind")
        member = self.state["member"]
        uid = self.state["uid"]
        done = []
        if kind in ("podman", "root"):
            for command in self.resolve(inject.get("commands") or [], variables):
                if kind == "podman":
                    code, out, err = hostops.as_member(self.root_alias, member, uid, command)
                else:
                    code, out, err = hostops.ssh(self.root_alias, command)
                done.append({"command": command, "code": code, "out": (out or "").strip()[:200],
                             "err": (err or "").strip()[:200]})
        elif kind == "mcp":
            with self.session() as session:
                for call in self.resolve(inject.get("calls") or [], variables):
                    arguments = dict(call.get("arguments") or {})
                    if "process" in arguments:
                        process = fixtures.find(session, arguments.pop("process"))
                        if process:
                            arguments["id"] = process["id"]
                    try:
                        answer = session.call(call["tool"], arguments)
                        done.append({"tool": call["tool"], "answer": str(answer)[:200]})
                    except mcp.McpError as exc:
                        done.append({"tool": call["tool"], "refused": exc.detail[:200],
                                     "expected": bool(call.get("expect_error"))})
                        if not call.get("expect_error"):
                            raise
        else:
            raise SystemExit("no inject kind named %s" % kind)
        return done

    def run_one(self, sentence, number, adapter):
        row_path = os.path.join(self.folder, "%s-%d.json" % (sentence["id"], number))
        if os.path.exists(row_path):
            self.log("%s run %d already has a row, skipping" % (sentence["id"], number))
            return json.load(open(row_path))
        variables = self.variables(sentence)
        prompt = self.resolve(sentence["prompt"], variables)
        check_spec = self.resolve(sentence["check"], variables)
        workdir = os.path.join(self.folder, "runs", "%s-%d" % (sentence["id"], number))
        os.makedirs(workdir, exist_ok=True)
        meta = {
            "run": number,
            "member": self.state["member"],
            "kitbash_version": self.version,
            "prompt": prompt,
            "started_at": now(),
        }
        self.log("%s run %d: %s" % (sentence["id"], number, prompt[:90]))

        setup_report = None
        injected_at = None
        if sentence["class"] != "fault" and sentence.get("setup"):
            setup_report = self.setup_fixture(sentence, variables)
        if sentence["class"] == "fault":
            setup_report = self.setup_fault(sentence, variables)
            done = self.inject(sentence, variables)
            injected_at = time.time()
            setup_report["injected"] = done
            broken = checks.run(self.context(variables=variables), check_spec)
            setup_report["broken_after_inject"] = not broken.get("passed")
            setup_report["broken_detail"] = broken.get("detail")
            if broken.get("passed"):
                self.log("  the fault did not take: the check still passes")
        meta["setup"] = setup_report
        meta["injected_at"] = now() if injected_at else None
        meta["inject_verified"] = (setup_report or {}).get("broken_after_inject")

        with self.session() as session:
            pre = [p.get("id") for p in session.call("proc_list", {}).get("processes") or []]
        since = now()
        context = self.context(pre_ids=pre, since=since, variables=variables)

        handle = adapter.start(prompt, workdir, mcp.ssh_command(self.alias, self.state["ssh_config"]))
        mitigation = {"first_pass": None}
        stop = threading.Event()
        watcher = None
        if injected_at:
            watcher = threading.Thread(
                target=self.watch, args=(context, check_spec, injected_at, mitigation, stop, handle), daemon=True
            )
            watcher.start()
        outcome_meta = adapter.finish(handle, self.options.timeout)
        stop.set()
        if watcher:
            watcher.join(timeout=60)
        meta.update(outcome_meta)
        meta["ended_at"] = now()

        outcome = checks.run(context, check_spec, attempts=CHECK_ATTEMPTS, delay=CHECK_DELAY)
        meta["observed"] = self.observe(sentence, variables, context)
        if injected_at and mitigation["first_pass"] is None and outcome.get("passed"):
            mitigation["first_pass"] = time.time()
        if injected_at and mitigation["first_pass"]:
            meta["time_to_mitigate_ms"] = int((mitigation["first_pass"] - injected_at) * 1000)
        transcript = adapter.parse(workdir)
        if transcript.get("rate_limited") and not transcript.get("tool_calls"):
            # No row: a run that never reached the host is not a result, and
            # writing one would make the round unresumable at this sentence.
            raise UsageLimit(
                "%s run %d: %s" % (sentence["id"], number, (transcript.get("error") or "")[:200])
            )
        row = rows.build_row(sentence, transcript, outcome, meta)
        with open(row_path, "w") as out:
            json.dump(row, out, indent=1, sort_keys=True)
        self.clean_workdir(workdir)
        self.log(
            "  %s: %s in %.0fs, %s calls, %s kitbash calls%s"
            % (
                "passed" if row["passed"] else "failed",
                sentence["id"],
                (meta.get("wall_ms") or 0) / 1000.0,
                row["tool_calls"],
                row["kitbash_calls"],
                "" if not row.get("time_to_mitigate_ms") else ", mitigated in %.0fs" % (row["time_to_mitigate_ms"] / 1000.0),
            )
        )
        if not row["passed"]:
            self.log("  check said: %s" % str(outcome.get("detail"))[:300])
        return row

    def observe(self, sentence, variables, context):
        """What a run showed beside the bar it is measured against.

        An observation is a check that decides nothing: the weather job cannot
        post inside a run, because its first tick is the next morning, so the
        board is read to record whether the agent proved the job by hand
        rather than to pass or fail the row. It runs once, after the outcome,
        and a refusal is a false reading rather than an error.
        """
        watched = sentence.get("observe") or {}
        if not watched:
            return None
        seen = {}
        for name, spec in watched.items():
            result = checks.run(context, self.resolve(spec, variables))
            seen[name] = {"observed": bool(result.get("passed")), "detail": result.get("detail")}
            self.log("  observed %s: %s" % (name, seen[name]["observed"]))
        return seen

    def watch(self, context, check_spec, injected_at, mitigation, stop, handle):
        """Poll the check while the agent works, so mitigation has a time."""
        while not stop.wait(POLL_SECONDS):
            try:
                result = checks.run(context, check_spec)
            except Exception:
                continue
            if result.get("passed"):
                mitigation["first_pass"] = time.time()
                return

    def clean_workdir(self, workdir):
        """The transcript is kept and nothing else.

        The agent's configuration directory holds credentials, and what it
        wrote on this machine is a clone and a scaffold that belong on the host
        rather than in this repository, so a run keeps its stream, its prompt
        and its errors.
        """
        keep = {"stream.jsonl", "stderr.log", "prompt.txt", "last-message.txt", "mcp.json"}
        for name in os.listdir(workdir):
            if name in keep:
                continue
            path = os.path.join(workdir, name)
            if os.path.isdir(path):
                shutil.rmtree(path, ignore_errors=True)
            else:
                os.remove(path)

    # ------------------------------------------------------------ teardown

    def teardown(self):
        name = self.state.get("member")
        if not name:
            return
        # The mount fault makes one file immutable, and users_remove archives
        # the home by chowning it, which an immutable file refuses. The flag
        # the bench set is the bench's to clear.
        hostops.ssh(self.root_alias, "chattr -R -i /home/%s 2>/dev/null; true" % name)
        # A removal of a member holding a round's worth of Processes takes
        # longer than kitbash-mcp's client deadline, and the cancellation stops
        # userdel halfway, so the Processes are stopped over the surface first.
        try:
            with self.session() as session:
                for process in session.call("proc_list", {}).get("processes") or []:
                    try:
                        session.call("proc_stop", {"id": process["id"]})
                    except mcp.McpError:
                        pass
        except Exception as exc:
            self.log("could not stop the member's Processes first: %s" % str(exc)[:200])
        self.log("removing member %s, which takes its Processes, files and secrets with it" % name)
        try:
            answer = self.admin_call("users_remove", {"name": name})
            self.state["removed"] = answer
        except mcp.McpError as exc:
            self.log("users_remove refused: %s" % exc.detail[:300])
            # The call can fail after the account is already gone, so what the
            # listing says is what is believed.
            if self.member_exists(name):
                self.state["removal_failed"] = exc.detail[:300]
                self.save_state()
                return
            self.log("the listing no longer has %s, so the removal did happen" % name)
            self.state["removed"] = {"user": name, "note": "users_remove answered an error after the fact"}
        for suffix in ("", ".pub"):
            path = (self.state.get("key") or "") + suffix
            if path and os.path.exists(path):
                os.remove(path)
        self.state["member_removed_at"] = now()
        self.save_state()
