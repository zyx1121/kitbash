"""The report on fixture rows: one table per class, pass^k, and the readings."""

import os
import unittest

from support import FIXTURES
from benchlib import report

ROWS = os.path.join(FIXTURES, "rows")


class Render(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rows = report.load_rows(ROWS)
        cls.text = report.render(ROWS, {"member": "bench-20260919-2300"})

    def test_rows_load(self):
        self.assertEqual(len(self.rows), 6)

    def test_pass_k_needs_every_run(self):
        grouped = report.by_sentence(self.rows)
        self.assertTrue(report.summarize(grouped["todo-board"])["pass_k"])
        self.assertFalse(report.summarize(grouped["repo-node"])["pass_k"])
        self.assertEqual(report.summarize(grouped["repo-node"])["passed"], 1)

    def test_one_table_per_class(self):
        self.assertIn("## From nothing", self.text)
        self.assertIn("## From a repository", self.text)
        self.assertIn("## From a fault", self.text)
        self.assertIn("| `todo-board` | 2/2 |", self.text)
        self.assertIn("| `repo-node` | 1/2 |", self.text)

    def test_the_fault_table_has_a_time_to_mitigate(self):
        fault = self.text.split("## From a fault", 1)[1].split("## Totals", 1)[0]
        self.assertIn("| wall | time to mitigate |", fault)
        self.assertIn("2.3 min", fault)
        lines = [line for line in fault.splitlines() if line.startswith("|")]
        widths = {len(line.split("|")) for line in lines}
        self.assertEqual(len(widths), 1, "every row of the table has the same number of cells")

    def test_totals_and_the_three_readings(self):
        self.assertIn("| **all** | 3 | 2 |", self.text)
        self.assertIn("The three readings of 4.5, restated", self.text)
        self.assertIn("2 of 3 sentences passed every run", self.text)
        self.assertIn("`repo-node`", self.text.split("deployment conversation", 1)[1])

    def test_the_member_is_named_and_said_to_be_gone(self):
        self.assertIn("bench-20260919-2300", self.text)

    def test_an_empty_folder_says_so(self):
        self.assertIn("No rows", report.render(os.path.join(FIXTURES, "rows", "nothing-here")))


if __name__ == "__main__":
    unittest.main()
