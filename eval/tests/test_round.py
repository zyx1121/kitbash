"""The parts of a round that decide nothing about a host.

Only observe is exercised here: it is the one step that reads a result and
must keep it out of the outcome. Everything else in round.py talks to a host
over ssh, which these tests never do.
"""

import unittest

from support import EVAL  # noqa: F401
from benchlib import round as bench_round


class Observe(unittest.TestCase):
    def setUp(self):
        self.round = bench_round.Round.__new__(bench_round.Round)
        self.round.log = lambda line: None
        self.asked = []
        original = bench_round.checks.run
        self.addCleanup(lambda: setattr(bench_round.checks, "run", original))
        bench_round.checks.run = self.answer

    def answer(self, context, spec, **kwargs):
        self.asked.append(spec)
        return {"passed": self.passes, "detail": "the board answered"}

    def test_an_observation_is_recorded_with_its_variables_filled(self):
        self.passes = True
        sentence = {"observe": {"posted": {"name": "http_item_posted",
                                           "params": {"process": "{pkg}"}}}}
        seen = self.round.observe(sentence, {"pkg": "bench-board-class"}, context=None)
        self.assertEqual(seen, {"posted": {"observed": True, "detail": "the board answered"}})
        self.assertEqual(self.asked[0]["params"]["process"], "bench-board-class")

    def test_an_observation_that_did_not_happen_is_false_and_not_an_error(self):
        self.passes = False
        sentence = {"observe": {"posted": {"name": "http_item_posted", "params": {}}}}
        seen = self.round.observe(sentence, {}, context=None)
        self.assertFalse(seen["posted"]["observed"])

    def test_a_sentence_that_observes_nothing_records_nothing(self):
        self.passes = True
        self.assertIsNone(self.round.observe({"id": "todo-board"}, {}, context=None))
        self.assertEqual(self.asked, [])


if __name__ == "__main__":
    unittest.main()
