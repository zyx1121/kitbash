"""One transcript plus one check answer is one row."""

import os
import unittest

from support import FIXTURES
from benchlib import rows

CLAUDE = os.path.join(FIXTURES, "claude-stream.jsonl")


class BuildRow(unittest.TestCase):
    def setUp(self):
        self.sentence = {"id": "todo-board", "class": "nothing", "prompt": "Make a todo web app."}
        self.transcript = rows.parse_claude_stream(CLAUDE)

    def test_a_passing_row(self):
        outcome = {"passed": True, "detail": "the address answered 200", "check": "http_ok"}
        meta = {
            "run": 1,
            "member": "bench-20260919-2300",
            "kitbash_version": "0.13.1",
            "wall_ms": 93000,
            "started_at": "2026-09-19T23:00:00+00:00",
            "exit_code": 0,
        }
        row = rows.build_row(self.sentence, self.transcript, outcome, meta)
        self.assertEqual(row["id"], "todo-board")
        self.assertEqual(row["class"], "nothing")
        self.assertEqual(row["agent"], "claude")
        self.assertEqual(row["model"], "claude-sonnet-5")
        self.assertEqual(row["kitbash_version"], "0.13.1")
        self.assertEqual(row["turns"], 14)
        self.assertEqual(row["tool_calls"], 13)
        self.assertEqual(row["kitbash_calls"], 8)
        self.assertEqual(row["kitbash_calls_by_tool"]["fs_write"], 2)
        self.assertEqual(row["tool_errors"], 2)
        self.assertEqual(row["wall_ms"], 93000)
        self.assertTrue(row["passed"])
        self.assertIsNone(row["time_to_mitigate_ms"])
        self.assertEqual(row["questions_asked"], 0)

    def test_a_failing_row_keeps_the_checks_reason(self):
        outcome = {"passed": False, "detail": "no address answered as asked", "check": "http_ok"}
        row = rows.build_row(self.sentence, self.transcript, outcome, {"run": 2})
        self.assertFalse(row["passed"])
        self.assertEqual(row["check"]["detail"], "no address answered as asked")

    def test_a_fault_row_carries_the_time_to_mitigate(self):
        sentence = {"id": "fault-stopped", "class": "fault", "prompt": "The board is down."}
        meta = {"run": 1, "time_to_mitigate_ms": 165000, "injected_at": "2026-09-19T23:10:00+00:00",
                "inject_verified": True}
        row = rows.build_row(sentence, self.transcript, {"passed": True, "detail": "back"}, meta)
        self.assertEqual(row["time_to_mitigate_ms"], 165000)
        self.assertTrue(row["inject_verified"])

    def test_the_result_text_is_cut(self):
        transcript = dict(self.transcript, result_text="x" * 5000)
        row = rows.build_row(self.sentence, transcript, {"passed": True}, {"run": 1})
        self.assertEqual(len(row["result_text"]), 2000)


if __name__ == "__main__":
    unittest.main()
