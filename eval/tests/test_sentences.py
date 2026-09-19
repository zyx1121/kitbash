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
