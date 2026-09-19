"""Reading a captured transcript. The fixtures are real runs with paths redacted."""

import os
import unittest

from support import FIXTURES
from benchlib import rows

CLAUDE = os.path.join(FIXTURES, "claude-stream.jsonl")
CODEX = os.path.join(FIXTURES, "codex-stream.jsonl")


class ClaudeStream(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.transcript = rows.parse_claude_stream(CLAUDE)

    def test_model_and_turns(self):
        self.assertEqual(self.transcript["model"], "claude-sonnet-5")
        self.assertEqual(self.transcript["turns"], 14)

    def test_tool_counts(self):
        self.assertEqual(self.transcript["tool_calls"], 13)
        self.assertEqual(sum(self.transcript["kitbash_calls"].values()), 8)
        self.assertEqual(self.transcript["kitbash_calls"]["proc_run"], 2)
        self.assertNotIn("mcp__kitbash__proc_run", self.transcript["kitbash_calls"])

    def test_errors_and_cost(self):
        self.assertEqual(self.transcript["tool_errors"], 2)
        self.assertGreater(self.transcript["cost_usd"], 0)
        self.assertEqual(self.transcript["stop_reason"], "success")

    def test_result_text_is_the_agents_last_word(self):
        self.assertIn("todo.loki.kitbash.zyx.tw", self.transcript["result_text"])

    def test_the_fixture_carries_nothing_sensitive(self):
        with open(CLAUDE) as handle:
            text = handle.read()
        for forbidden in ("accessToken", "Bearer ", "sk-ant", "/Users/", "BEGIN OPENSSH"):
            self.assertNotIn(forbidden, text)


class CodexStream(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.transcript = rows.parse_codex_stream(CODEX)

    def test_mcp_calls_are_counted(self):
        self.assertEqual(self.transcript["kitbash_calls"], {"users_me": 1})
        self.assertEqual(self.transcript["tool_calls"], 1)
        self.assertEqual(self.transcript["tool_errors"], 0)

    def test_what_codex_does_not_report(self):
        self.assertIsNone(self.transcript["cost_usd"])
        self.assertIsNotNone(self.transcript["usage"])

    def test_result_text_is_the_last_message(self):
        self.assertIn("bench-demo", self.transcript["result_text"])


if __name__ == "__main__":
    unittest.main()
