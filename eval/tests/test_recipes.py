"""The M16 checks, the recipes a row records and the recipe table. No socket, no host."""

import json
import os
import tempfile
import unittest

from support import FIXTURES  # noqa: F401
from benchlib import checks, report, rows


class Stub:
    """A member's surface and a root shell, both answered here."""

    def __init__(self, processes, answers=None, root_alias="root@host", pre_ids=()):
        self._processes = processes
        self.answers = answers or {}
        self.root_alias = root_alias
        self.domain = "kitbash.example"
        self.member = "bench-x"
        self.pre_ids = set(pre_ids)
        self.calls = []

    def processes(self):
        return self._processes

    def call(self, tool, arguments=None):
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


class RefusesWithoutKey(unittest.TestCase):
    def test_a_server_that_refuses_every_keyless_request_passes(self):
        patch(self, checks, "fetch", lambda url, method="GET", body=None, headers=None: (401, "no key"))
        result = checks.http_refuses_without_key(Stub(MODEL), {"status": [401, 403]})
        self.assertTrue(result["passed"], result)
        self.assertEqual(len(result["seen"]), 2)

    def test_one_open_endpoint_fails_the_run(self):
        """llama-server with the key in LLAMA_ARG_API_KEY: /v1/models answers everyone."""
        answers = {"/v1/models": 200, "/v1/chat/completions": 401}
        patch(self, checks, "fetch", lambda url, method="GET", body=None, headers=None:
              (answers[url.split(".example", 1)[1]], "{}"))
        result = checks.http_refuses_without_key(Stub(MODEL), {})
        self.assertFalse(result["passed"])
        self.assertIn("/v1/models answered 200", result["detail"])

    def test_a_second_open_process_beside_a_guarded_one_fails(self):
        both = MODEL + [{"id": "m2", "name": "open", "package": "/home/bench-x/open", "state": "running"}]
        patch(self, checks, "fetch", lambda url, method="GET", body=None, headers=None:
              (200 if "open." in url else 401, ""))
        self.assertFalse(checks.http_refuses_without_key(Stub(both), {})["passed"])

    def test_a_run_that_made_nothing_fails(self):
        self.assertFalse(checks.http_refuses_without_key(Stub(MODEL, pre_ids=["m1"]), {})["passed"])


class Guestbook:
    """An application at one address that keeps entries in memory, or on disk."""

    def __init__(self, post_path="/sign", read_path="/", persistent=True):
        self.entries = []
        self.post_path = post_path
        self.read_path = read_path
        self.persistent = persistent

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


class NothingCommitted(unittest.TestCase):
    def ls_files(self, listing, code=0):
        seen = []

        def ssh(alias, command, timeout=300, check=False):
            seen.append((alias, command))
            return code, listing, "fatal: nope" if code else ""

        patch(self, checks.hostops, "ssh", ssh)
        return seen

    def test_the_ignore_file_alone_passes(self):
        seen = self.ls_files("kitbash.yaml\nDockerfile\nserver.js\npgdata/.gitignore\n")
        result = checks.nothing_committed(Stub(MODEL), {})
        self.assertTrue(result["passed"], result)
        self.assertIn("safe.directory", seen[0][1])
        self.assertIn("/home/bench-x/chat", seen[0][1])

    def test_a_committed_data_directory_fails(self):
        self.ls_files("kitbash.yaml\npgdata/pgdata/PG_VERSION\npgdata/pgdata/base/1/112\n")
        result = checks.nothing_committed(Stub(MODEL), {})
        self.assertFalse(result["passed"])
        self.assertEqual(len(result["tracked"]), 2)

    def test_a_committed_sqlite_file_fails(self):
        self.ls_files("app.py\ndata/guestbook.db\n")
        self.assertFalse(checks.nothing_committed(Stub(MODEL), {})["passed"])

    def test_without_a_root_alias_it_says_so(self):
        result = checks.nothing_committed(Stub(MODEL, root_alias=None), {})
        self.assertFalse(result["passed"])
        self.assertIn("root alias", result["detail"])


class RecipesRead(unittest.TestCase):
    def stream(self, lines):
        handle = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
        for line in lines:
            handle.write(json.dumps(line) + "\n")
        handle.close()
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def use(self, tool, path):
        return {"type": "assistant", "message": {"content": [
            {"type": "tool_use", "name": "mcp__kitbash__" + tool, "input": {"path": path}}]}}

    def test_reads_under_org_skills_are_recorded_once_in_order(self):
        path = self.stream([
            self.use("fs_list", "/org/skills"),
            self.use("fs_read", "/org/skills/serve-ollama/SKILL.md"),
            self.use("fs_read", "/org/handbook/README.md"),
            self.use("fs_read", "/org/skills/serve-ollama/SKILL.md"),
            self.use("fs_read", "/org/skills/postgres/SKILL.md"),
        ])
        transcript = rows.parse_claude_stream(path)
        self.assertEqual(transcript["recipes_read"],
                         ["/org/skills/serve-ollama/SKILL.md", "/org/skills/postgres/SKILL.md"])
        row = rows.build_row({"id": "chat-model-private", "class": "recipe"}, transcript, {"passed": True}, {})
        self.assertEqual(len(row["recipes_read"]), 2)

    def test_codex_arguments_as_a_string_are_read_too(self):
        path = self.stream([{"type": "item.completed", "item": {
            "type": "mcp_tool_call", "server": "kitbash", "tool": "fs_read", "status": "completed",
            "arguments": json.dumps({"path": "/org/skills/serve-llamacpp/SKILL.md"})}}])
        self.assertEqual(rows.parse_codex_stream(path)["recipes_read"], ["/org/skills/serve-llamacpp/SKILL.md"])


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


if __name__ == "__main__":
    unittest.main()
