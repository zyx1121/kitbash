"""The sentence file is the bench. If it does not hold together nothing else does."""

import json
import os
import re
import unittest

from support import EVAL
from benchlib import checks, fixtures

CLASSES = {"nothing", "repository", "fault"}
INJECT_KINDS = {"podman", "root", "mcp"}


def check_names(spec):
    yield spec["name"]
    for child in (spec.get("params") or {}).get("checks") or []:
        for name in check_names(child):
            yield name


class SentenceFile(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        with open(os.path.join(EVAL, "sentences.json")) as handle:
            cls.document = json.load(handle)
        cls.sentences = cls.document["sentences"]

    def test_fifteen_sentences_in_three_classes(self):
        self.assertEqual(len(self.sentences), 15)
        counted = {}
        for sentence in self.sentences:
            counted[sentence["class"]] = counted.get(sentence["class"], 0) + 1
        self.assertEqual(set(counted), CLASSES)
        self.assertEqual(sorted(counted.values()), [5, 5, 5])

    def test_ids_are_unique(self):
        ids = [s["id"] for s in self.sentences]
        self.assertEqual(len(ids), len(set(ids)))

    def test_every_sentence_has_a_prompt_and_a_check(self):
        for sentence in self.sentences:
            self.assertTrue(sentence["prompt"].strip(), sentence["id"])
            self.assertIn("check", sentence)

    def test_every_check_name_exists(self):
        for sentence in self.sentences:
            for name in check_names(sentence["check"]):
                self.assertIn(name, checks.REGISTRY, "%s names %s" % (sentence["id"], name))

    def test_every_fault_has_an_inject_and_a_fixture_to_break(self):
        faults = [s for s in self.sentences if s["class"] == "fault"]
        self.assertEqual(len(faults), 5)
        for sentence in faults:
            self.assertIn("inject", sentence, sentence["id"])
            self.assertIn(sentence["inject"]["kind"], INJECT_KINDS, sentence["id"])
            self.assertTrue(
                sentence["inject"].get("commands") or sentence["inject"].get("calls"), sentence["id"]
            )
            setup = sentence["setup"]
            folder = os.path.join(fixtures.FIXTURE_ROOT, setup["fixture"])
            self.assertTrue(os.path.isdir(folder), folder)
            self.assertTrue(os.path.exists(os.path.join(folder, "kitbash.yaml.tmpl")))
            self.assertIn("{url}", sentence["prompt"], "a fault names the address the user sees")

    def test_every_setup_names_a_fixture_that_exists(self):
        """A fault is not the only sentence that needs something deployed."""
        for sentence in self.sentences:
            setup = sentence.get("setup")
            if not setup:
                continue
            folder = os.path.join(fixtures.FIXTURE_ROOT, setup["fixture"])
            self.assertTrue(os.path.isdir(folder), "%s names %s" % (sentence["id"], folder))
            self.assertTrue(setup.get("package"), sentence["id"])
            self.assertRegex(setup["package"], r"^[a-z0-9]+(-[a-z0-9]+)*$", sentence["id"])

    def test_no_prompt_names_an_address_outside_the_round(self):
        """The bench writes into the round and nowhere else.

        The M10 weather sentence named a member's own board and four rounds
        posted onto it, which is the bench changing data it does not own. An
        address on a kitbash host belongs to a round's member, so a prompt
        reaches one through {url} or {dep_url} and never by name.
        """
        for sentence in self.sentences:
            for address in re.findall(r"https?://[^\s\"]+", sentence["prompt"]):
                self.assertNotIn(
                    "kitbash", address,
                    "%s names a kitbash address rather than the round's own: %s"
                    % (sentence["id"], address),
                )

    def test_the_scheduled_job_posts_to_the_rounds_own_board(self):
        job = [s for s in self.sentences if s["id"] == "weather-job"][0]
        self.assertEqual(job["setup"]["fixture"], "bench-board")
        self.assertIn("{url}/api/items", job["prompt"])
        names = list(check_names(job["check"]))
        self.assertIn("proc_scheduled", names)
        self.assertIn("http_item_posted", names, "the check reads the item off the board")

    def test_no_sentence_outside_a_fault_injects(self):
        for sentence in self.sentences:
            if sentence["class"] != "fault":
                self.assertNotIn("inject", sentence, sentence["id"])

    def test_fixture_templates_fill(self):
        """Every placeholder a fixture manifest carries is one the round sets."""
        variables = {"pkg": "bench-board-stopped", "member": "bench-x", "dep_host": "c.example.org"}
        for name in sorted(os.listdir(fixtures.FIXTURE_ROOT)):
            files = fixtures.package_files(name, variables)
            manifest = [f for f in files if f["path"] == "kitbash.yaml"]
            self.assertEqual(len(manifest), 1, name)
            left = re.findall(r"\{[a-z_]+\}", manifest[0]["content"])
            self.assertEqual(left, [], "%s still carries %s" % (name, left))


if __name__ == "__main__":
    unittest.main()
