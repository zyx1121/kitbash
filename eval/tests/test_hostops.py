"""Root commands on a member's paths, run for real on a folder of the test's standing in for /home."""

import argparse
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest

from support import EVAL  # noqa: F401
from benchlib import hostops
from benchlib import round as bench_round

MEMBER = "bench-x"


class Host(unittest.TestCase):
    """Both ways the bench reaches the host, the root alias and as_member, run here.

    Every command has /home/ moved under a folder of the test's. chattr and
    podman are stand ins that write down what they were asked, because
    setting an immutable flag needs root and this runs as nobody special.
    """

    def setUp(self):
        self.top = os.path.realpath(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.top, ignore_errors=True)
        self.home = os.path.join(self.top, "home", MEMBER)
        self.data = os.path.join(self.home, "bench-board-mount-data")
        os.makedirs(self.data)
        # A file of root's, outside the member's home, that nothing may touch.
        self.outside = os.path.join(self.top, "etc", "items.json")
        os.makedirs(os.path.dirname(self.outside))
        with open(self.outside, "w") as handle:
            handle.write("root's own\n")
        os.chmod(self.outside, 0o644)
        self.bin = os.path.join(self.top, "bin")
        os.makedirs(self.bin)
        self.log = os.path.join(self.top, "chattr.log")
        self.tool("chattr", 'echo "$*" >> %s' % self.log)
        self.tool("podman", '[ "$1" = unshare ] && { shift; exec "$@"; }; exit 0')
        self.ran = []

        def run(who):
            def execute(command):
                command = command.replace("/home/", os.path.join(self.top, "home") + "/")
                self.ran.append((who, command))
                env = dict(os.environ, PATH=self.bin + ":" + os.environ["PATH"])
                done = subprocess.run(["sh", "-c", command], capture_output=True, text=True, env=env)
                return done.returncode, done.stdout, done.stderr
            return execute

        self.patch(hostops, "ssh", lambda alias, command, timeout=300, check=False: run("root")(command))
        self.patch(hostops, "as_member",
                   lambda root_alias, member, uid, command, timeout=300: run("member")(command))

    def patch(self, module, name, value):
        original = getattr(module, name)
        setattr(module, name, value)
        self.addCleanup(lambda: setattr(module, name, original))

    def tool(self, name, body):
        path = os.path.join(self.bin, name)
        with open(path, "w") as handle:
            handle.write("#!/bin/sh\n%s\n" % body)
        os.chmod(path, 0o755)

    def chattr(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log) as handle:
            return [line.strip() for line in handle if line.strip()]

    def untouched(self):
        with open(self.outside) as handle:
            self.assertEqual(handle.read(), "root's own\n")
        self.assertEqual(stat.S_IMODE(os.stat(self.outside).st_mode), 0o644)
        self.assertFalse([line for line in self.chattr() if "etc" in line], self.chattr())

    def inject(self):
        """The mount fault as the round injects it, from sentences.json."""
        with open(os.path.join(os.path.dirname(os.path.dirname(__file__)), "sentences.json")) as handle:
            sentence = [s for s in json.load(handle)["sentences"] if s["id"] == "fault-mount"][0]
        options = argparse.Namespace(root="root@host", admin="admin@host", host="kitbash-mcp")
        bench = bench_round.Round(options)
        bench.state = {"member": MEMBER, "uid": 1001}
        bench.domain = "kitbash.example"
        return bench.inject(sentence, bench.variables(sentence))

    def items(self):
        return os.path.join(self.data, "items.json")


class MountFault(Host):
    def test_a_link_to_a_file_outside_the_home_is_left_untouched(self):
        os.symlink(self.outside, self.items())
        done = self.inject()
        self.untouched()
        self.assertTrue(os.path.islink(self.items()))
        self.assertTrue(done[0]["code"], done)

    def test_a_folder_that_is_a_link_outside_the_home_is_left_untouched(self):
        shutil.rmtree(self.data)
        os.symlink(os.path.dirname(self.outside), self.data)
        self.inject()
        self.untouched()

    def test_a_regular_file_is_made_immutable(self):
        with open(self.items(), "w") as handle:
            handle.write("[]\n")
        done = self.inject()
        self.assertEqual(done[0]["code"], 0, done)
        self.assertEqual(stat.S_IMODE(os.stat(self.items()).st_mode), 0o444)
        self.assertEqual(self.chattr(), ["+i %s" % self.items()])
        # Only the flag is set as root; the mode and the file are the member's.
        root = [c for who, c in self.ran if who == "root"]
        self.assertEqual(len(root), 1)
        self.assertNotIn("chmod", root[0])

    def test_a_missing_file_is_created_by_the_member(self):
        done = self.inject()
        self.assertEqual(done[0]["code"], 0, done)
        with open(self.items()) as handle:
            self.assertEqual(handle.read(), "[]\n")

    def test_clearing_the_fault_does_not_follow_a_link(self):
        os.symlink(self.outside, self.items())
        os.chmod(self.outside, 0o644)
        hostops.clear_immutable("root@host", MEMBER, 1001, "/home/%s/bench-board-mount-data/items.json" % MEMBER)
        self.untouched()

    def test_the_home_that_is_a_link_is_not_cleared(self):
        shutil.rmtree(os.path.join(self.top, "home"))
        os.makedirs(os.path.join(self.top, "home"))
        os.symlink(os.path.dirname(self.outside), self.home)
        hostops.clear_home_flags("root@host", MEMBER)
        self.assertEqual(self.chattr(), [])

    def test_the_home_itself_is_cleared(self):
        hostops.clear_home_flags("root@host", MEMBER)
        self.assertEqual(self.chattr(), ["-R -i %s" % self.home])

    def test_a_path_not_plainly_below_the_home_is_refused_before_the_host(self):
        for path in ("/home/bench-x/../../etc/items.json", "/etc/items.json", "/home/bench-xy/items.json",
                     "/home/bench-x/./items.json"):
            with self.assertRaises(hostops.UnsafePath, msg=path):
                hostops.make_immutable("root@host", MEMBER, 1001, path)
        self.assertEqual(self.ran, [])


if __name__ == "__main__":
    unittest.main()
