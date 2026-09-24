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
# A uid no process on the test machine runs as, so the member is idle. The
# test's own uid stands for a member with something still running.
IDLE_UID = 4242424


class Session:
    """The member's surface: Processes to stop and start, every call written down."""

    def __init__(self, events, processes):
        self.events = events
        self.processes = processes

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def call(self, tool, arguments=None):
        self.events.append(("member-surface", tool, arguments))
        if tool == "proc_list":
            return {"processes": self.processes}
        return {}


@unittest.skipUnless(os.path.isdir("/proc/self"), "reads /proc")
class Host(unittest.TestCase):
    """Both ways the bench reaches the host, the root alias and as_member, run here.

    Every command has /home/ moved under a folder of the test's. chattr and
    podman are stand ins that write down what they were asked, because
    setting an immutable flag needs root and this runs as nobody special.
    The idle check is the real one, reading /proc.
    """

    uid = IDLE_UID

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
        self.runtime = os.path.join(self.top, "run")
        os.makedirs(self.bin)
        os.makedirs(self.runtime)
        self.log = os.path.join(self.top, "chattr.log")
        self.tool("chattr", 'echo "$*" >> %s' % self.log)
        self.tool("podman", '[ "$1" = unshare ] && { shift; exec "$@"; }; exit 0')
        self.ran = []
        self.events = []

        def run(who):
            def execute(command):
                command = command.replace("/home/", os.path.join(self.top, "home") + "/")
                self.ran.append((who, command))
                if "chattr" in command:
                    self.events.append((who, "chattr", command))
                env = dict(os.environ, PATH=self.bin + ":" + os.environ["PATH"], XDG_RUNTIME_DIR=self.runtime)
                done = subprocess.run(["sh", "-c", command], capture_output=True, text=True, env=env)
                return done.returncode, done.stdout, done.stderr
            return execute

        self.patch(hostops, "ssh", lambda alias, command, timeout=300, check=False: run("root")(command))
        self.patch(hostops, "as_member",
                   lambda root_alias, member, uid, command, timeout=300: run("member")(command))
        self.patch(bench_round, "IDLE_SECONDS", 0)
        self.patch(bench_round.fixtures, "wait_healthy", lambda *a, **k: {"passed": True})
        self.processes = [{"id": "p1", "name": "bench-board-mount", "package": "/home/bench-x/bench-board-mount",
                           "state": "running"},
                          {"id": "p2", "name": "earlier", "package": "/home/bench-x/earlier", "state": "running"}]

    def patch(self, module, name, value):
        missing = object()
        original = getattr(module, name, missing)
        setattr(module, name, value)
        self.addCleanup(lambda: delattr(module, name) if original is missing else setattr(module, name, original))

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

    def bench(self, flagged=()):
        options = argparse.Namespace(root="root@host", admin="admin@host", host="kitbash-mcp")
        bench = bench_round.Round(options)
        bench.folder = self.top
        bench.alias = "bench-member"
        bench.state = {"member": MEMBER, "uid": self.uid, "flagged": list(flagged), "ssh_config": "ssh_config"}
        bench.domain = "kitbash.example"
        bench.session = lambda: Session(self.events, self.processes)
        bench.log = lambda line: None
        return bench

    def sentence(self):
        with open(os.path.join(os.path.dirname(os.path.dirname(__file__)), "sentences.json")) as handle:
            return [s for s in json.load(handle)["sentences"] if s["id"] == "fault-mount"][0]

    def inject(self):
        """The mount fault as the round injects it, from sentences.json."""
        bench = self.bench()
        sentence = self.sentence()
        return bench.inject(sentence, bench.variables(sentence)), bench

    def items(self):
        return os.path.join(self.data, "items.json")

    def recorded(self):
        return "/home/%s/bench-board-mount-data/items.json" % MEMBER


class MountFault(Host):
    def test_a_link_to_a_file_outside_the_home_is_left_untouched(self):
        os.symlink(self.outside, self.items())
        done, bench = self.inject()
        self.untouched()
        self.assertTrue(os.path.islink(self.items()))
        self.assertFalse(done[0]["injected"], done)
        self.assertEqual(bench.state["flagged"], [])

    def test_a_folder_that_is_a_link_outside_the_home_is_left_untouched(self):
        shutil.rmtree(self.data)
        os.symlink(os.path.dirname(self.outside), self.data)
        self.inject()
        self.untouched()

    def test_a_regular_file_is_made_immutable_and_recorded(self):
        with open(self.items(), "w") as handle:
            handle.write("[]\n")
        done, bench = self.inject()
        self.assertTrue(done[0]["injected"], done)
        self.assertEqual(stat.S_IMODE(os.stat(self.items()).st_mode), 0o444)
        self.assertEqual(self.chattr(), ["+i %s" % self.items()])
        self.assertEqual(bench.state["flagged"], [self.recorded()])
        # Only the flag is set as root; the mode and the file are the member's.
        root = [c for who, c in self.ran if who == "root" and "chattr" in c]
        self.assertEqual(len(root), 1)
        self.assertNotIn("chmod", root[0])

    def test_every_process_is_stopped_before_root_acts_and_started_after(self):
        with open(self.items(), "w") as handle:
            handle.write("[]\n")
        self.inject()
        steps = [(who, what) for who, what, _ in self.events]
        chattr = steps.index(("root", "chattr"))
        stops = [i for i, step in enumerate(steps) if step == ("member-surface", "proc_stop")]
        runs = [i for i, step in enumerate(steps) if step == ("member-surface", "proc_run")]
        self.assertEqual(len(stops), 2)
        self.assertEqual(len(runs), 2)
        self.assertLess(max(stops), chattr)
        self.assertGreater(min(runs), chattr)

    def test_a_missing_file_is_created_by_the_member(self):
        done, _ = self.inject()
        self.assertTrue(done[0]["injected"], done)
        with open(self.items()) as handle:
            self.assertEqual(handle.read(), "[]\n")

    def test_clearing_the_fault_does_not_follow_a_link(self):
        os.symlink(self.outside, self.items())
        hostops.clear_immutable("root@host", MEMBER, self.uid, self.recorded())
        self.untouched()

    def test_a_path_not_plainly_below_the_home_is_refused_before_the_host(self):
        for path in ("/home/bench-x/../../etc/items.json", "/etc/items.json", "/home/bench-xy/items.json",
                     "/home/bench-x/./items.json"):
            with self.assertRaises(hostops.UnsafePath, msg=path):
                hostops.make_immutable("root@host", MEMBER, self.uid, path)
        self.assertEqual(self.ran, [])


class WhileTheMemberRuns(Host):
    """The test's own uid is the member's: something of theirs runs, as this test does."""

    uid = os.getuid()

    def test_the_fault_is_refused_and_recorded_as_not_injected(self):
        with open(self.items(), "w") as handle:
            handle.write("[]\n")
        os.chmod(self.items(), 0o644)
        done, bench = self.inject()
        self.assertFalse(done[0]["injected"], done)
        self.assertIn(str(os.getpid()), done[0]["busy"])
        self.assertEqual(self.chattr(), [])
        self.assertEqual(stat.S_IMODE(os.stat(self.items()).st_mode), 0o644)
        self.assertEqual(bench.state["flagged"], [])
        # What was stopped is started again, so the round goes on.
        self.assertEqual(len([e for e in self.events if e[1] == "proc_run"]), 2)

    def test_the_helper_refuses_on_its_own_too(self):
        with open(self.items(), "w") as handle:
            handle.write("[]\n")
        code, _, err = hostops.make_immutable("root@host", MEMBER, self.uid, self.recorded())
        self.assertEqual(code, 3)
        self.assertIn("busy", err)
        self.assertEqual(self.chattr(), [])

    def test_teardown_clears_nothing(self):
        bench = self.bench(flagged=[self.recorded()])
        bench.admin_call = lambda tool, arguments=None: {"user": MEMBER}
        bench.teardown()
        self.assertEqual(self.chattr(), [])


class Teardown(Host):
    def setUp(self):
        super().setUp()
        self.other = os.path.join(self.home, "notes.txt")
        for path in (self.items(), self.other):
            with open(path, "w") as handle:
                handle.write("x\n")

    def test_only_the_files_the_bench_flagged_are_cleared_after_the_processes_stop(self):
        bench = self.bench(flagged=[self.recorded()])
        removed = []
        bench.admin_call = lambda tool, arguments=None: removed.append(tool) or {"user": MEMBER}
        bench.teardown()
        self.assertEqual(self.chattr(), ["-i %s" % self.items()])
        steps = [(who, what) for who, what, _ in self.events]
        self.assertLess(max(i for i, s in enumerate(steps) if s == ("member-surface", "proc_stop")),
                        steps.index(("root", "chattr")))
        self.assertEqual(removed, ["users_remove"])
        self.assertEqual(bench.state["flagged"], [])

    def test_nothing_flagged_means_no_root_step(self):
        bench = self.bench()
        bench.admin_call = lambda tool, arguments=None: {"user": MEMBER}
        bench.teardown()
        self.assertEqual(self.chattr(), [])

    def test_a_reset_clears_only_a_recorded_file(self):
        bench = self.bench(flagged=[])
        sentence = self.sentence()
        self.patch(bench_round.fixtures, "deploy", lambda *a, **k: None)
        bench.setup_fault(sentence, bench.variables(sentence))
        self.assertEqual(self.chattr(), [])
        bench = self.bench(flagged=[self.recorded()])
        bench.setup_fault(sentence, bench.variables(sentence))
        self.assertEqual(self.chattr(), ["-i %s" % self.items()])


if __name__ == "__main__":
    unittest.main()
