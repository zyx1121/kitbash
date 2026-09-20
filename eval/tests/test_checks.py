"""Checks against a stub surface. Nothing here opens a socket."""

import unittest

from support import FIXTURES  # noqa: F401
from benchlib import checks


class StubContext:
    """What a check is given, with the answers written here instead of a host."""

    def __init__(self, processes, detailed=None, domain="kitbash.example", member="bench-x", pre_ids=()):
        self._processes = processes
        self._detailed = detailed or {}
        self.domain = domain
        self.member = member
        self.pre_ids = set(pre_ids)
        self.calls = []

    def processes(self):
        return self._processes

    def call(self, tool, arguments=None):
        self.calls.append((tool, arguments))
        if tool == "proc_list" and (arguments or {}).get("package"):
            return {"processes": self._detailed.get(arguments["package"], [])}
        return {"processes": self._processes}

    def url_of(self, process):
        return process.get("url") or "https://%s.%s.%s" % (process.get("name"), self.member, self.domain)


class ProcScheduled(unittest.TestCase):
    def test_a_line_that_carries_the_cron_is_read_off_one_call(self):
        """What issue #154 fixed: the line carries schedule and nextRun."""
        context = StubContext(
            [{"id": "1", "name": "weather-poster", "package": "/home/bench-x/weather",
              "state": "scheduled", "schedule": "0 8 * * *", "nextRun": "2026-09-20T08:00:00Z"}]
        )
        result = checks.proc_scheduled(context, {"scope": "new"})
        self.assertTrue(result["passed"])
        self.assertEqual(result["seen"][0]["schedule"], "0 8 * * *")
        # And no second call: the listing answered the question by itself.
        self.assertEqual(context.calls, [])

    def test_a_one_line_listing_carries_the_state_and_not_the_cron(self):
        """A host that predates the fix: the second call is the fallback."""
        context = StubContext(
            [{"id": "1", "name": "weather-poster", "package": "/home/bench-x/weather", "state": "scheduled"}],
            detailed={
                "/home/bench-x/weather": [
                    {
                        "id": "1",
                        "name": "weather-poster",
                        "state": "scheduled",
                        "schedule": "0 0 * * *",
                        "nextRun": "2026-09-20T00:00:00Z",
                    }
                ]
            },
        )
        result = checks.proc_scheduled(context, {"scope": "new"})
        self.assertTrue(result["passed"])
        self.assertEqual(result["seen"][0]["schedule"], "0 0 * * *")
        self.assertIn(("proc_list", {"package": "/home/bench-x/weather"}), context.calls)

    def test_a_running_web_process_is_not_a_job(self):
        context = StubContext([{"id": "1", "name": "todo", "package": "/home/bench-x/todo", "state": "running"}])
        self.assertFalse(checks.proc_scheduled(context, {"scope": "new"})["passed"])

    def test_a_process_that_was_there_before_the_run_does_not_count(self):
        context = StubContext(
            [{"id": "1", "name": "old", "package": "/home/bench-x/old", "state": "scheduled", "schedule": "0 8 * * *"}],
            pre_ids=["1"],
        )
        self.assertFalse(checks.proc_scheduled(context, {"scope": "new"})["passed"])


class ItemPosted(unittest.TestCase):
    """The board is read over HTTP, so the fetch is the one thing stubbed."""

    def board(self, body, status=200):
        original = checks.fetch
        checks.fetch = lambda url, **kwargs: (status, body)
        self.addCleanup(lambda: setattr(checks, "fetch", original))

    def context(self):
        return StubContext([{"id": "1", "name": "bench-board-class", "state": "running",
                             "url": "https://bench-board-class.bench-x.kitbash.example"}])

    def test_an_item_by_the_author_the_sentence_named_passes(self):
        self.board('{"items": [{"text": "Hsinchu: 28C, showers", "author": "weather-bot"}]}')
        result = checks.http_item_posted(
            self.context(), {"process": "bench-board-class", "path": "/api/items", "author": "weather-bot"})
        self.assertTrue(result["passed"], result["detail"])
        self.assertEqual(result["seen"]["author"], "weather-bot")

    def test_a_board_holding_somebody_elses_items_does_not_pass(self):
        self.board('{"items": [{"text": "buy milk", "author": "bench"}]}')
        result = checks.http_item_posted(
            self.context(), {"process": "bench-board-class", "author": "weather-bot"})
        self.assertFalse(result["passed"])
        self.assertIn("weather-bot", result["detail"])

    def test_an_empty_board_does_not_pass(self):
        self.board('{"items": []}')
        self.assertFalse(checks.http_item_posted(self.context(), {"process": "bench-board-class"})["passed"])

    def test_an_item_with_no_text_is_not_an_item(self):
        self.board('{"items": [{"text": "   ", "author": "weather-bot"}]}')
        result = checks.http_item_posted(self.context(), {"process": "bench-board-class", "author": "weather-bot"})
        self.assertFalse(result["passed"])

    def test_a_board_that_is_not_JSON_fails_rather_than_raising(self):
        self.board("<html>the board is down</html>", status=502)
        result = checks.http_item_posted(self.context(), {"process": "bench-board-class"})
        self.assertFalse(result["passed"])
        self.assertEqual(result["tried"][0]["status"], 502)


class Candidates(unittest.TestCase):
    def test_a_named_process_that_is_gone_is_still_an_address(self):
        context = StubContext([])
        candidates = checks._candidates(context, {"process": "bench-board-image"})
        self.assertEqual(candidates[0]["name"], "bench-board-image")
        self.assertEqual(context.url_of(candidates[0]), "https://bench-board-image.bench-x.kitbash.example")

    def test_only_processes_this_run_made_are_candidates(self):
        context = StubContext(
            [
                {"id": "old", "name": "a", "state": "running", "url": "https://a"},
                {"id": "new", "name": "b", "state": "running", "url": "https://b"},
            ],
            pre_ids=["old"],
        )
        names = [p["name"] for p in checks._candidates(context, {"scope": "new"})]
        self.assertEqual(names, ["b"])


class Registry(unittest.TestCase):
    def test_an_unknown_check_name_fails_rather_than_raising(self):
        result = checks.run(StubContext([]), {"name": "no-such-check"})
        self.assertFalse(result["passed"])
        self.assertIn("no check named", result["detail"])

    def test_all_of_stops_at_the_first_failure(self):
        context = StubContext([])
        spec = {
            "name": "all_of",
            "params": {"checks": [{"name": "proc_running", "params": {"process": "x"}}, {"name": "no-such-check"}]},
        }
        result = checks.run(context, spec)
        self.assertFalse(result["passed"])
        self.assertIn("proc_running", result["detail"])
        self.assertEqual(len(result["parts"]), 1)


if __name__ == "__main__":
    unittest.main()
