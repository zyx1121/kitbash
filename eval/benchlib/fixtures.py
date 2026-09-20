"""The Processes a sentence is about before the agent starts.

A fault sentence is only a fault if there is something to break, so the round
deploys the thing itself, as the member, over the same surface an agent uses. A
sentence that names an address gets the same deploy without the fault, so what
it names is inside the round: the scheduled job posts onto a board deployed
this way rather than onto somebody's own Process. The fixtures are four small
Packages under eval/fixtures.
"""

import os
import time

from . import checks, mcp

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE_ROOT = os.path.join(os.path.dirname(HERE), "fixtures")


def fill(text, variables):
    for key, value in variables.items():
        text = text.replace("{%s}" % key, str(value))
    return text


def package_files(fixture, variables):
    """The files of one fixture Package, ready for one fs_write."""
    folder = os.path.join(FIXTURE_ROOT, fixture)
    files = []
    for name in sorted(os.listdir(folder)):
        if name.startswith("data."):
            continue
        with open(os.path.join(folder, name)) as handle:
            content = handle.read()
        target = "kitbash.yaml" if name == "kitbash.yaml.tmpl" else name
        files.append({"path": target, "content": fill(content, variables)})
    return files


def data_manifest(fixture, variables):
    path = os.path.join(FIXTURE_ROOT, fixture, "data.kitbash.yaml.tmpl")
    if not os.path.exists(path):
        return None
    with open(path) as handle:
        return fill(handle.read(), variables)


def deploy(session, member, fixture, package, variables, log=print):
    """Write, build and run one fixture Package as the member. Idempotent."""
    variables = dict(variables, pkg=package, member=member)
    home = "/home/%s" % member
    root = "%s/%s" % (home, package)
    files = [
        {"path": "%s/%s" % (root, f["path"]), "content": f["content"]}
        for f in package_files(fixture, variables)
    ]
    data = data_manifest(fixture, variables)
    if data:
        _write(session, [{"path": "%s/%s-data/kitbash.yaml" % (home, package), "content": data}],
               "Add the %s-data folder" % package, log)
    _write(session, files, "Add the %s bench fixture" % package, log)
    log("  build %s" % root)
    session.call("pkg_build", {"path": root})
    return start(session, root, package, log)


def start(session, path, name, log=print):
    """Run the Process, or stop and run it again if it is already registered."""
    try:
        return session.call("proc_run", {"package": path, "name": name})
    except mcp.McpError as exc:
        log("  proc_run said %s, stopping and running again" % exc.detail[:120])
        process = find(session, name)
        if process:
            try:
                session.call("proc_stop", {"id": process["id"]})
            except mcp.McpError:
                pass
        return session.call("proc_run", {"package": path, "name": name})


def _write(session, files, message, log):
    """One commit. A write that is refused stops the deploy, because a fixture
    nobody can read is not a fault anybody can fix."""
    session.call("fs_write", {"files": files, "message": message})


def find(session, name):
    for process in session.call("proc_list", {}).get("processes") or []:
        if process.get("name") == name:
            return process
    return None


def wait_healthy(context, spec, seconds=240, every=10):
    """Wait until the fixture answers, so a fault breaks something that worked."""
    deadline = time.time() + seconds
    result = {"passed": False, "detail": "not checked"}
    while time.time() < deadline:
        result = checks.run(context, spec)
        if result.get("passed"):
            return result
        time.sleep(every)
    return result
