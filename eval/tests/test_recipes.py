"""The M16 checks, the recipes a row records and the recipe table. No socket, no host."""

import json
import os
import re
import shlex
import shutil
import subprocess
import tempfile
import unittest

from support import FIXTURES  # noqa: F401
from benchlib import checks, report, rows


class Stub:
    """A member's surface and a root shell, both answered here."""

    def __init__(self, processes, answers=None, root_alias="root@host", pre_ids=(), uid=1001):
        self._processes = processes
        self.answers = answers or {}
        self.root_alias = root_alias
        self.uid = uid
        self.domain = "kitbash.example"
        self.member = "bench-x"
        self.pre_ids = set(pre_ids)
        self.calls = []

    def processes(self):
        return self._processes

    def call(self, tool, arguments=None, timeout=None):
        self.calls.append((tool, arguments))
        answer = self.answers.get(tool)
        return answer(arguments) if callable(answer) else (answer or {})

    def url_of(self, process):
        return process.get("url") or "https://%s.%s.%s" % (process.get("name"), self.member, self.domain)


def patch(test, module, name, value):
    original = getattr(module, name)
    setattr(module, name, value)
    test.addCleanup(lambda: setattr(module, name, original))


MODEL = [{"id": "m1", "name": "chat", "package": "/home/bench-x/chat", "state": "running",
          "url": "https://chat.bench-x.kitbash.example"}]


KEY = "k-" + "7" * 30
PROXY_MANIFEST = {"pkg_inspect": {"manifest": {"name": "chat", "deploy": {"units": [
    {"name": "api", "build": "proxy", "expose": "http", "port": 8080, "secrets": ["API_KEY"]},
    {"name": "ollama", "build": "ollama", "environment": {"OLLAMA_HOST": "127.0.0.1:11434"}}]}}}}


class ModelServer:
    """An address that answers by path, and by whether the request carries the key."""

    def __init__(self, keyless, with_key=None, key=KEY, any_bearer=False):
        self.keyless = keyless
        self.with_key = with_key if with_key is not None else {}
        self.key = key
        # A proxy that asks only whether there is a bearer, whatever it says.
        self.any_bearer = any_bearer
        self.sent = []

    def fetch(self, url, method="GET", body=None, headers=None):
        path = url.split(".example", 1)[1]
        auth = (headers or {}).get("Authorization")
        self.sent.append((method, path, auth))
        if auth == "Bearer %s" % self.key or (self.any_bearer and (auth or "").startswith("Bearer ")):
            return self.with_key.get(path, 200), "{}"
        return self.keyless.get(path, 401), "no key"


class RefusesWithoutKey(unittest.TestCase):
    def use(self, server, stub=None, env="API_KEY=%s\n" % KEY, params=None):
        patch(self, checks, "fetch", server.fetch)
        seen = []

        def as_member(root_alias, member, uid, command, timeout=300):
            seen.append((root_alias, member, uid, command))
            return 0, env, ""

        patch(self, checks.hostops, "as_member", as_member)
        stub = stub or Stub(MODEL, answers=dict(PROXY_MANIFEST))
        return checks.http_refuses_without_key(stub, params or {"status": [401, 403]}), seen

    def test_a_server_that_refuses_every_keyless_request_and_takes_the_key_passes(self):
        server = ModelServer({})
        result, seen = self.use(server)
        self.assertTrue(result["passed"], result)
        keyless = [(m, p) for m, p, auth in server.sent if auth is None]
        for probe in [("GET", "/v1/models"), ("POST", "/v1/chat/completions"), ("GET", "/api/tags"),
                      ("POST", "/api/generate"), ("GET", "/props")]:
            self.assertIn(probe, keyless)
        self.assertEqual(len(result["seen"]), 5)
        self.assertIn(("GET", "/v1/models", "Bearer %s" % KEY), server.sent)
        # Every path that refused no key was asked again with a key nobody set.
        wrong = [(m, p) for m, p, auth in server.sent if auth and auth != "Bearer %s" % KEY]
        self.assertEqual(len(wrong), 5)
        self.assertEqual(len(result["wrong_key"]), 5)
        # The key is read as the member, from the containers of this Process, by the names declared.
        root, member, uid, command = seen[0]
        self.assertEqual((root, member, uid), ("root@host", "bench-x", 1001))
        self.assertIn("label=kitbash.id=m1", command)
        self.assertIn("^(API_KEY)=", command)
        # The value is the member's and never lands in a row.
        self.assertNotIn(KEY, json.dumps(result))

    def test_one_open_endpoint_fails_the_run(self):
        """llama-server with the key in LLAMA_ARG_API_KEY: /v1/models answers everyone."""
        result, _ = self.use(ModelServer({"/v1/models": 200}))
        self.assertFalse(result["passed"])
        self.assertIn("/v1/models answered 200", result["detail"])

    def test_a_proxy_that_guards_v1_and_passes_ollama_through_fails(self):
        result, _ = self.use(ModelServer({"/api/tags": 200, "/api/generate": 200}))
        self.assertFalse(result["passed"])
        self.assertIn("/api/tags answered 200 without a key", result["detail"])

    def test_llama_server_props_answering_everyone_fails(self):
        result, _ = self.use(ModelServer({"/props": 200}))
        self.assertFalse(result["passed"])
        self.assertIn("/props answered 200", result["detail"])

    def test_paths_the_engine_lacks_may_answer_404(self):
        """vLLM with --api-key guards /v1 and has no /api or /props."""
        result, _ = self.use(ModelServer({"/api/tags": 404, "/api/generate": 404, "/props": 404}))
        self.assertTrue(result["passed"], result)

    def test_an_address_that_refuses_nothing_fails(self):
        result, _ = self.use(ModelServer({p: 404 for p in ["/v1/models", "/v1/chat/completions", "/api/tags",
                                                           "/api/generate", "/props"]}))
        self.assertFalse(result["passed"])
        self.assertIn("refused no request", result["detail"])

    def test_a_server_that_refuses_everyone_fails(self):
        server = ModelServer({}, with_key={p: 401 for p in ["/v1/models", "/api/tags", "/props"]})
        result, _ = self.use(server)
        self.assertFalse(result["passed"])
        self.assertIn("refuses everyone", result["detail"])

    def test_a_proxy_that_takes_any_bearer_fails(self):
        result, _ = self.use(ModelServer({}, any_bearer=True))
        self.assertFalse(result["passed"])
        self.assertIn("answered 200 to a key nobody set", result["detail"])

    def test_a_proxy_that_guards_only_the_chat_post_takes_the_key_there(self):
        """Every GET is 404 or 405 with or without a key, so the POST is the request with the key."""
        missing = {"/v1/models": 404, "/api/tags": 404, "/props": 405, "/api/generate": 404}
        server = ModelServer(dict(missing), with_key=dict(missing, **{"/v1/chat/completions": 200}))
        result, _ = self.use(server)
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["key_probe"]["method"], "POST")
        self.assertNotIn("body", result["key_probe"])

    def test_no_post_is_made_with_the_key_when_a_get_answers(self):
        server = ModelServer({}, with_key={"/v1/models": 401, "/api/tags": 401, "/props": 401})
        result, _ = self.use(server)
        self.assertFalse(result["passed"])
        self.assertNotIn(("POST", "/v1/chat/completions", "Bearer %s" % KEY), server.sent)

    def test_a_key_written_as_a_variable_is_used_too(self):
        stub = Stub(MODEL, answers={"pkg_inspect": {"manifest": {"deploy": {"units": [
            {"image": "ghcr.io/ggml-org/llama.cpp", "environment": {"LLAMA_API_KEY": KEY, "LLAMA_ARG_PORT": "8080"}}]}}}})
        result, seen = self.use(ModelServer({}), stub=stub, env="LLAMA_API_KEY=%s\n" % KEY)
        self.assertTrue(result["passed"], result)
        self.assertIn("^(LLAMA_API_KEY)=", seen[0][3])

    def test_no_key_declared_fails_rather_than_passing_on_refusals(self):
        stub = Stub(MODEL, answers={"pkg_inspect": {"manifest": {"deploy": {"units": [{"build": "."}]}}}})
        result, seen = self.use(ModelServer({}), stub=stub)
        self.assertFalse(result["passed"])
        self.assertIn("no unit declares a secret", result["detail"])
        self.assertEqual(seen, [])

    def test_without_a_root_alias_the_key_probe_is_skipped_and_says_why(self):
        server = ModelServer({})
        result, seen = self.use(server, stub=Stub(MODEL, answers=dict(PROXY_MANIFEST), root_alias=None))
        self.assertTrue(result["passed"], result)
        self.assertIn("skipped", result["key_probe"])
        self.assertEqual(seen, [])
        # The wrong key needs no host and is still asked; the real key is not.
        self.assertFalse([s for s in server.sent if s[2] == "Bearer %s" % KEY])
        self.assertEqual(len(result["wrong_key"]), 5)

    def test_a_second_open_process_beside_a_guarded_one_fails(self):
        both = MODEL + [{"id": "m2", "name": "open", "package": "/home/bench-x/open", "state": "running"}]
        patch(self, checks, "fetch", lambda url, method="GET", body=None, headers=None:
              (200 if "open." in url else 401, ""))
        self.assertFalse(checks.http_refuses_without_key(Stub(both), {})["passed"])

    def test_a_run_that_made_nothing_fails(self):
        self.assertFalse(checks.http_refuses_without_key(Stub(MODEL, pre_ids=["m1"]), {})["passed"])


class Guestbook:
    """An application at one address that keeps entries in memory, or on disk."""

    def __init__(self, post_path="/sign", read_path="/", persistent=True, in_postgres=True):
        self.entries = []
        self.post_path = post_path
        self.read_path = read_path
        self.persistent = persistent
        # Where the entries are kept: rows of a Postgres, or a file beside the application.
        self.in_postgres = in_postgres

    def fetch(self, url, method="GET", body=None, headers=None):
        path = url.split(".example", 1)[1] or "/"
        if method == "POST":
            if path != self.post_path:
                return 404, "not found"
            self.entries.append((body or {}).get("message", ""))
            return 201, "ok"
        if path == self.read_path:
            return 200, "<ul>%s</ul>" % "".join("<li>%s</li>" % e for e in self.entries)
        return 404, "not found"

    def form(self, url, fields):
        return self.fetch(url, method="POST", body=fields)

    def restart(self, arguments):
        if not self.persistent:
            self.entries = []
        return {"state": "running"}


class EntrySurvivesRestart(unittest.TestCase):
    def run_with(self, book):
        patch(self, checks, "fetch", book.fetch)
        patch(self, checks, "fetch_form", book.form)
        patch(self, checks.time, "sleep", lambda seconds: None)
        context = Stub(MODEL, answers={"proc_stop": {"state": "stopped"}, "proc_run": book.restart})
        return checks.entry_survives_restart(context, {"restart_wait": 1}), context

    def test_an_entry_kept_across_a_restart_passes_whatever_path_it_took(self):
        result, context = self.run_with(Guestbook(post_path="/sign", read_path="/"))
        self.assertTrue(result["passed"], result)
        self.assertEqual(result["seen"]["post"], "/sign")
        tools = [tool for tool, _ in context.calls]
        self.assertEqual(tools, ["proc_stop", "proc_run"])
        self.assertEqual(context.calls[1][1], {"package": "/home/bench-x/chat", "name": "chat"})

    def test_an_entry_lost_on_restart_fails(self):
        result, _ = self.run_with(Guestbook(persistent=False))
        self.assertFalse(result["passed"])
        self.assertIn("gone after the restart", result["detail"])

    def test_an_application_that_takes_no_entry_fails_before_any_restart(self):
        result, context = self.run_with(Guestbook(post_path="/nowhere-the-check-tries"))
        self.assertFalse(result["passed"])
        self.assertEqual(context.calls, [])


GUESTBOOK = [
    {"id": "g1", "name": "guestbook", "package": "/home/bench-x/guestbook", "state": "running",
     "url": "https://guestbook.bench-x.kitbash.example"},
]


class EntryInDatabase(unittest.TestCase):
    def run_with(self, book, stub=None):
        patch(self, checks, "fetch", book.fetch)
        patch(self, checks, "fetch_form", book.form)
        patch(self, checks.time, "sleep", lambda seconds: None)
        commands = []

        def as_member(root_alias, member, uid, command, timeout=300):
            """pg_dumpall in the containers of one Process, piped to grep -c for the marker."""
            commands.append((member, uid, command))
            marker = re.findall(r"bench-[0-9a-f]{12}", command)[-1]
            count = sum(1 for e in book.entries if e == marker) if book.in_postgres else 0
            return (0 if count else 1), "%d\n" % count, ""

        patch(self, checks.hostops, "as_member", as_member)
        return checks.entry_in_database(stub or Stub(GUESTBOOK), {"wait": 0}), commands

    def test_an_entry_the_application_wrote_into_postgres_passes(self):
        result, commands = self.run_with(Guestbook())
        self.assertTrue(result["passed"], result)
        member, uid, command = commands[0]
        self.assertEqual((member, uid), ("bench-x", 1001))
        self.assertIn("label=kitbash.id=g1", command)
        self.assertIn("pg_dumpall --data-only", command)
        self.assertIn(result["seen"]["marker"], command)

    def test_an_entry_kept_in_a_file_fails_though_it_survives_a_restart(self):
        """The control run of the round without recipes: one Node unit writing data/entries.json."""
        book = Guestbook(in_postgres=False)
        result, _ = self.run_with(book)
        self.assertFalse(result["passed"])
        self.assertIn("in no Postgres of this run", result["detail"])

    def test_without_a_root_alias_it_says_so(self):
        result, commands = self.run_with(Guestbook(), stub=Stub(GUESTBOOK, root_alias=None))
        self.assertFalse(result["passed"])
        self.assertIn("root alias", result["detail"])
        self.assertEqual(commands, [])


class UnitRunsImage(unittest.TestCase):
    def test_a_pinned_postgres_unit_passes(self):
        context = Stub(MODEL, answers={"pkg_inspect": {"units": [
            {"name": "web", "build": "."},
            {"name": "db", "image": "docker.io/library/postgres@sha256:" + "a" * 64}]}})
        self.assertTrue(checks.unit_runs_image(context, {"contains": "postgres"})["passed"])

    def test_a_unit_built_from_postgres_passes(self):
        context = Stub(MODEL, answers={
            "pkg_inspect": {"units": [{"name": "db", "build": "db"}]},
            "fs_read": lambda a: {"text": "FROM docker.io/library/postgres:17-alpine\n"} if a["path"] == "/home/bench-x/chat/db/Dockerfile" else {"text": ""},
        })
        self.assertTrue(checks.unit_runs_image(context, {"contains": "postgres"})["passed"])

    def test_sqlite_in_the_application_is_not_a_postgres_unit(self):
        context = Stub(MODEL, answers={"pkg_inspect": {"units": [{"name": "web", "build": "."}]},
                                       "fs_read": {"text": "FROM node:22-alpine\n"}})
        self.assertFalse(checks.unit_runs_image(context, {"contains": "postgres"})["passed"])


def inner(command):
    """The command as the member's shell runs it: _as_member wraps it in sh -c."""
    words = shlex.split(command)
    assert words[:2] == ["sh", "-c"], command
    return words[2]


class NothingCommitted(unittest.TestCase):
    def ls_files(self, listing, code=0, status=None):
        """git as the member: ls-files answers the listing, status --porcelain answers by folder."""
        seen = []
        status = status or {}

        def as_member(root_alias, member, uid, command, timeout=300):
            command = inner(command)
            seen.append((member, uid, command))
            if " ls-files" in command:
                return code, listing, "fatal: nope" if code else ""
            folder = shlex.split(command.split(" -C ", 1)[1])[0]
            return 0, status.get(folder, ""), ""

        def ssh(alias, command, timeout=300, check=False):
            raise AssertionError("git ran as root: %s" % command)

        patch(self, checks.hostops, "as_member", as_member)
        patch(self, checks.hostops, "ssh", ssh)
        return seen

    def test_the_ignore_file_alone_passes(self):
        seen = self.ls_files("kitbash.yaml\nDockerfile\nserver.js\npgdata/.gitignore\n")
        result = checks.nothing_committed(Stub(MODEL), {})
        self.assertTrue(result["passed"], result)
        self.assertEqual(seen[0][:2], ("bench-x", 1001))
        self.assertIn("core.fsmonitor=false", seen[0][2])
        self.assertIn("/home/bench-x/chat", seen[0][2])
        self.assertIn("status --porcelain", seen[1][2])

    def test_a_committed_data_directory_fails(self):
        self.ls_files("kitbash.yaml\npgdata/pgdata/PG_VERSION\npgdata/pgdata/base/1/112\n")
        result = checks.nothing_committed(Stub(MODEL), {})
        self.assertFalse(result["passed"])
        self.assertEqual(len(result["tracked"]), 2)

    def test_a_committed_sqlite_file_fails(self):
        self.ls_files("app.py\ndata/guestbook.db\n")
        self.assertFalse(checks.nothing_committed(Stub(MODEL), {})["passed"])

    def test_untracked_files_in_the_package_fail(self):
        """The control round's guestbook: data/entries.json written through a mount with no .gitignore."""
        self.ls_files("kitbash.yaml\nserver.js\n", status={"/home/bench-x/chat": "?? data/\n"})
        result = checks.nothing_committed(Stub(MODEL), {})
        self.assertFalse(result["passed"])
        self.assertIn("pkg_build refuses", result["detail"])
        self.assertEqual(result["uncommitted"], ["/home/bench-x/chat: ?? data/"])

    def test_a_dirty_mounted_folder_outside_the_package_is_left_to_pkg_builds(self):
        detail = {"processes": [{"id": "m1", "mounts": [
            {"source": "/home/bench-x/chat-data", "target": "/var/lib/postgresql/data", "mode": "rw"}]}]}
        seen = self.ls_files("kitbash.yaml\n", status={"/home/bench-x/chat-data": "?? PG_VERSION\n"})
        result = checks.nothing_committed(Stub(MODEL, answers={"proc_list": detail}), {})
        self.assertTrue(result["passed"], result)
        self.assertFalse([c for _, _, c in seen if "chat-data" in c])

    def test_without_a_root_alias_it_says_so(self):
        result = checks.nothing_committed(Stub(MODEL, root_alias=None), {})
        self.assertFalse(result["passed"])
        self.assertIn("root alias", result["detail"])


@unittest.skipUnless(shutil.which("git"), "needs git")
class GitIsNotRunAsRoot(unittest.TestCase):
    """A Package whose .git/config names an fsmonitor: whatever it runs must not run as root.

    Both ways the bench reaches the host run the command here for real, root
    as ssh to the root alias and the member through as_member, and the script
    writes down which of the two ran it.
    """

    def setUp(self):
        self.home = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.repo = os.path.join(self.home, "guestbook")
        self.marker = os.path.join(self.home, "ran")
        os.makedirs(self.repo)
        git = ["git", "-C", self.repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"]
        subprocess.run(["git", "init", "-q", self.repo], check=True)
        with open(os.path.join(self.repo, "kitbash.yaml"), "w") as handle:
            handle.write("name: guestbook\n")
        subprocess.run(git + ["add", "."], check=True)
        subprocess.run(git + ["commit", "-q", "-m", "one"], check=True)
        script = os.path.join(self.home, "planted.sh")
        with open(script, "w") as handle:
            handle.write("#!/bin/sh\necho \"$BENCH_AS\" >> %s\n" % self.marker)
        os.chmod(script, 0o755)
        for key, value in (("core.fsmonitor", script), ("core.pager", script), ("diff.external", script)):
            subprocess.run(["git", "-C", self.repo, "config", key, value], check=True)
        hooks = os.path.join(self.repo, ".git", "hooks")
        os.makedirs(hooks, exist_ok=True)
        for hook in ("post-index-change", "reference-transaction"):
            shutil.copy(script, os.path.join(hooks, hook))

        def run_as(who):
            def run(*args, **kwargs):
                command = args[-1] if who == "root" else args[3]
                env = dict(os.environ, BENCH_AS=who)
                done = subprocess.run(["sh", "-c", command], capture_output=True, text=True, env=env)
                return done.returncode, done.stdout, done.stderr
            return run

        patch(self, checks.hostops, "ssh", lambda alias, command, timeout=300, check=False: run_as("root")(command))
        patch(self, checks.hostops, "as_member", run_as("member"))

    def ran(self):
        if not os.path.exists(self.marker):
            return []
        with open(self.marker) as handle:
            return handle.read().split()

    def test_nothing_the_repository_names_runs_as_root(self):
        processes = [{"id": "g1", "name": "guestbook", "package": self.repo, "state": "running",
                      "url": "https://guestbook.bench-x.kitbash.example"}]
        result = checks.nothing_committed(Stub(processes), {})
        self.assertTrue(result["passed"], result)
        self.assertNotIn("root", self.ran())


class PkgBuilds(unittest.TestCase):
    def test_a_package_that_builds_passes(self):
        stub = Stub(MODEL, answers={"pkg_build": {"digest": "sha256:" + "b" * 64, "commit": "abc"}})
        result = checks.pkg_builds(stub, {})
        self.assertTrue(result["passed"], result)
        self.assertEqual(stub.calls, [("pkg_build", {"path": "/home/bench-x/chat"})])

    def test_a_conflict_fails(self):
        def refuse(arguments):
            raise checks.mcp.McpError("pkg_build", "uncommitted changes under /home/bench-x/chat; kitbash builds from a commit")

        result = checks.pkg_builds(Stub(MODEL, answers={"pkg_build": refuse}), {})
        self.assertFalse(result["passed"])
        self.assertIn("uncommitted changes", result["detail"])

    def test_the_guestbook_sentence_builds_after_the_restart(self):
        with open(os.path.join(os.path.dirname(FIXTURES), "..", "sentences.json")) as handle:
            sentences = {s["id"]: s for s in json.load(handle)["sentences"]}
        names = [c["name"] for c in sentences["guestbook-restart"]["check"]["params"]["checks"]]
        self.assertLess(names.index("entry_survives_restart"), names.index("pkg_builds"))
        self.assertLess(names.index("entry_survives_restart"), names.index("nothing_committed"))
        self.assertIn("entry_in_database", names)


class RecipesRead(unittest.TestCase):
    def stream(self, lines):
        handle = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
        for line in lines:
            handle.write(json.dumps(line) + "\n")
        handle.close()
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def use(self, tool, path, ident=None):
        return {"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": ident or path, "name": "mcp__kitbash__" + tool, "input": {"path": path}}]}}

    def answer(self, ident, error=False):
        return {"type": "user", "message": {"content": [
            {"type": "tool_result", "tool_use_id": ident, "is_error": error, "content": "..."}]}}

    def read(self, path, ident=None, error=False):
        return [self.use("fs_read", path, ident), self.answer(ident or path, error)]

    def test_reads_under_org_skills_are_recorded_once_in_order(self):
        path = self.stream(
            [self.use("fs_list", "/org/skills"), self.answer("/org/skills")]
            + self.read("/org/skills/serve-ollama/SKILL.md", "a")
            + self.read("/org/handbook/README.md")
            + self.read("/org/skills/serve-ollama/SKILL.md", "b")
            + self.read("/org/skills/postgres/SKILL.md")
        )
        transcript = rows.parse_claude_stream(path)
        self.assertEqual(transcript["recipes_read"],
                         ["/org/skills/serve-ollama/SKILL.md", "/org/skills/postgres/SKILL.md"])
        row = rows.build_row({"id": "chat-model-private", "class": "recipe"}, transcript, {"passed": True}, {})
        self.assertEqual(len(row["recipes_read"]), 2)

    def test_a_read_that_answered_an_error_is_not_a_recipe_read(self):
        path = self.stream(
            self.read("/org/skills/serve-ollama/README.md", error=True)
            + self.read("/org/skills/serve-ollama/SKILL.md")
            + [self.use("fs_read", "/org/skills/postgres/SKILL.md")]
        )
        transcript = rows.parse_claude_stream(path)
        self.assertEqual(transcript["recipes_read"], ["/org/skills/serve-ollama/SKILL.md"])
        self.assertEqual(transcript["tool_errors"], 1)

    def codex(self, path, **fields):
        item = {"type": "mcp_tool_call", "server": "kitbash", "tool": "fs_read", "status": "completed",
                "arguments": json.dumps({"path": path})}
        item.update(fields)
        return {"type": "item.completed", "item": item}

    def test_codex_arguments_as_a_string_are_read_too(self):
        path = self.stream([self.codex("/org/skills/serve-llamacpp/SKILL.md")])
        self.assertEqual(rows.parse_codex_stream(path)["recipes_read"], ["/org/skills/serve-llamacpp/SKILL.md"])

    def test_a_codex_read_that_failed_is_not_a_recipe_read(self):
        path = self.stream([
            self.codex("/org/skills/a/SKILL.md", status="failed", error={"message": "not found"}),
            self.codex("/org/skills/b/SKILL.md", result={"content": [], "isError": True}),
            self.codex("/org/skills/c/SKILL.md"),
        ])
        self.assertEqual(rows.parse_codex_stream(path)["recipes_read"], ["/org/skills/c/SKILL.md"])


class RecipeTable(unittest.TestCase):
    def test_the_recipe_table_names_the_recipes_each_sentence_read(self):
        folder = tempfile.mkdtemp()
        self.addCleanup(lambda: [os.unlink(os.path.join(folder, f)) for f in os.listdir(folder)] and os.rmdir(folder))
        for sentence, read in (("chat-model-private", ["/org/skills/serve-ollama/SKILL.md"]), ("guestbook-restart", [])):
            with open(os.path.join(folder, "%s-1.json" % sentence), "w") as handle:
                json.dump({"id": sentence, "class": "recipe", "run": 1, "passed": bool(read), "turns": 10,
                           "tool_calls": 12, "kitbash_calls": 8, "tool_errors": 0, "questions_asked": 0,
                           "cost_usd": 0.3, "wall_ms": 60000, "recipes_read": read}, handle)
        text = report.render(folder, {})
        self.assertIn("## Answered by a recipe", text)
        self.assertIn("recipes read |", text)
        self.assertIn("`serve-ollama`", text)
        self.assertIn("| none |", text)
        # A round of one class says nothing about the classes it did not run.
        self.assertNotIn("from a fault", text.lower())
        self.assertNotIn("does not report", text)
        # Nor about M10's three sentences, which are from nothing, or a fault's time to mitigate.
        self.assertNotIn("M10 measured", text)
        self.assertNotIn("mitigated", text)


if __name__ == "__main__":
    unittest.main()
