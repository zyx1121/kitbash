"""Questions asked back: the count 4.5 reads, on three answers."""

import unittest

from support import FIXTURES  # noqa: F401
from benchlib import rows

# One answer that finished the work, one that stopped to ask, one that finished
# and asked something small at the end. All three are the shapes M10 saw.
FINISHED = (
    "Your todo app is live and ready to share:\n\n**https://todo.bench.kitbash.zyx.tw**\n\n"
    "Just send classmates that link. Data persists to disk, so it survives restarts."
)
STOPPED = (
    "Before I build this I need to know a few things. Where should this run, on your laptop or "
    "somewhere else? Which provider do you want to deploy to? And do you have an API key for the "
    "weather service?"
)
FINISHED_AND_ASKED = (
    "The PDF service is up at https://pdf.bench.kitbash.zyx.tw and converted a test file. "
    "Want me to add a size limit?"
)


class QuestionsAsked(unittest.TestCase):
    def test_an_answer_that_finished_asks_nothing(self):
        self.assertEqual(rows.questions_asked(FINISHED), 0)
        self.assertFalse(rows.asks_user(FINISHED))

    def test_an_answer_that_stopped_asks_three(self):
        self.assertEqual(rows.questions_asked(STOPPED), 3)
        self.assertTrue(rows.asks_user(STOPPED))

    def test_an_answer_that_finished_and_asked_one(self):
        self.assertEqual(rows.questions_asked(FINISHED_AND_ASKED), 1)
        self.assertTrue(rows.asks_user(FINISHED_AND_ASKED))

    def test_empty_and_odd_answers(self):
        self.assertEqual(rows.questions_asked(""), 0)
        self.assertEqual(rows.questions_asked(None), 0)
        self.assertFalse(rows.asks_user(""))

    def test_a_request_without_a_question_mark_counts_as_handing_back(self):
        text = "I built it locally. Let me know where you want it deployed."
        self.assertEqual(rows.questions_asked(text), 0)
        self.assertTrue(rows.asks_user(text))


if __name__ == "__main__":
    unittest.main()
