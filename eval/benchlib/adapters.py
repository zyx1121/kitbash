"""One interface, two agents, and a fake for the tests.

An adapter starts an agent on one sentence with one MCP server configured and
nothing else, and reads its transcript back. Everything an adapter needs to
know about an agent's flags lives here.
"""

import getpass
import glob
import json
import os
import re
import shutil
import signal
import subprocess
import time

from . import rows

# The tools a clean agent is given. The list is M10's plus git, because the
# repository class starts with a clone.
CLAUDE_ALLOWED_TOOLS = ",".join(
    [
        "mcp__kitbash__*",
        "Read",
        "Write",
        "Edit",
        "Glob",
        "Grep",
        "WebFetch",
        "Bash(git:*)",
        "Bash(npm:*)",
        "Bash(npx:*)",
        "Bash(node:*)",
        "Bash(bun:*)",
        "Bash(ls:*)",
        "Bash(mkdir:*)",
        "Bash(cat:*)",
        "Bash(curl:*)",
        "Bash(python3:*)",
        "Bash(pip:*)",
        "Bash(pip3:*)",
        "Bash(uv:*)",
        "Bash(which:*)",
        "Bash(unzip:*)",
        "Bash(tar:*)",
    ]
)

MAX_TURNS = 80


class Handle:
    """A started agent: the process, where it writes, and when it began."""

    def __init__(self, process, workdir, started):
        self.process = process
        self.workdir = workdir
        self.started = started


class Adapter:
    name = "adapter"

    def start(self, prompt, workdir, ssh_command):
        raise NotImplementedError

    def finish(self, handle, timeout):
        """Wait for the agent, kill it at the timeout, answer what happened."""
        timed_out = False
        while True:
            code = handle.process.poll()
            if code is not None:
                break
            if time.time() - handle.started > timeout:
                timed_out = True
                self.kill(handle)
                code = handle.process.poll()
                break
            time.sleep(1)
        return {
            "exit_code": code,
            "timed_out": timed_out,
            "wall_ms": int((time.time() - handle.started) * 1000),
        }

    def kill(self, handle):
        try:
            os.killpg(os.getpgid(handle.process.pid), signal.SIGTERM)
        except Exception:
            handle.process.terminate()
        try:
            handle.process.wait(timeout=20)
        except Exception:
            try:
                os.killpg(os.getpgid(handle.process.pid), signal.SIGKILL)
            except Exception:
                handle.process.kill()

    def parse(self, workdir):
        raise NotImplementedError


class ClaudeAdapter(Adapter):
    """Claude Code, stream-json, a fresh configuration directory per run."""

    name = "claude"

    def __init__(self, model=None, account=None):
        self.model = model
        self.account = account or getpass.getuser()

    def token(self):
        """The OAuth access token from the Keychain. It is never written down."""
        raw = subprocess.run(
            ["security", "find-generic-password", "-s", "Claude Code-credentials", "-a", self.account, "-w"],
            capture_output=True,
            text=True,
            check=True,
        ).stdout
        return (json.loads(raw).get("claudeAiOauth") or {}).get("accessToken") or ""

    def start(self, prompt, workdir, ssh_command):
        os.makedirs(workdir, exist_ok=True)
        config = os.path.join(workdir, "mcp.json")
        with open(config, "w") as handle:
            json.dump({"mcpServers": {"kitbash": {"command": ssh_command[0], "args": ssh_command[1:]}}}, handle)
        with open(os.path.join(workdir, "prompt.txt"), "w") as handle:
            handle.write(prompt + "\n")
        environment = dict(os.environ)
        environment["CLAUDE_CODE_OAUTH_TOKEN"] = self.token()
        environment["CLAUDE_CONFIG_DIR"] = os.path.join(workdir, "cfg")
        environment.pop("ANTHROPIC_API_KEY", None)
        os.makedirs(environment["CLAUDE_CONFIG_DIR"], exist_ok=True)
        command = [
            shutil.which("claude") or "claude",
            "-p",
            prompt,
            "--output-format",
            "stream-json",
            "--verbose",
            "--strict-mcp-config",
            "--mcp-config",
            config,
            "--setting-sources",
            "",
            "--permission-mode",
            "acceptEdits",
            "--max-turns",
            str(MAX_TURNS),
            "--allowedTools",
            CLAUDE_ALLOWED_TOOLS,
        ]
        if self.model:
            command += ["--model", self.model]
        stream = open(os.path.join(workdir, "stream.jsonl"), "w")
        errors = open(os.path.join(workdir, "stderr.log"), "w")
        process = subprocess.Popen(
            command,
            cwd=workdir,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=stream,
            stderr=errors,
            start_new_session=True,
        )
        return Handle(process, workdir, time.time())

    def parse(self, workdir):
        return rows.parse_claude_stream(os.path.join(workdir, "stream.jsonl"))


class CodexAdapter(Adapter):
    """Codex CLI, codex exec --json, a fresh CODEX_HOME per run.

    The clean home carries a copy of the machine's auth.json, because Codex
    reads its credentials from CODEX_HOME and nowhere else. Approvals are the
    one thing that has to be said out loud: with approval_policy never a call
    to an MCP tool answers "requires approval, but approval policy is never"
    and the agent never reaches the host, so the round runs with
    --approve-for-me, which reviews each request automatically inside the
    workspace-write sandbox. Network access is turned on because the
    repository class starts with a clone.
    """

    name = "codex"

    def __init__(self, model=None, auth_source=None):
        self.model = model
        self.auth_source = auth_source or os.path.expanduser("~/.codex/auth.json")

    def start(self, prompt, workdir, ssh_command):
        os.makedirs(workdir, exist_ok=True)
        home = os.path.join(workdir, "codex-home")
        os.makedirs(home, exist_ok=True)
        if os.path.exists(self.auth_source):
            shutil.copyfile(self.auth_source, os.path.join(home, "auth.json"))
            os.chmod(os.path.join(home, "auth.json"), 0o600)
        arguments = ", ".join(json.dumps(part) for part in ssh_command[1:])
        with open(os.path.join(home, "config.toml"), "w") as handle:
            handle.write(
                "# One MCP server and nothing else, the way a clean agent is set up.\n"
                "[mcp_servers.kitbash]\n"
                "command = %s\n" % json.dumps(ssh_command[0])
                + "args = [%s]\n" % arguments
                + "startup_timeout_sec = 60\n"
                "tool_timeout_sec = 900\n"
            )
        with open(os.path.join(workdir, "prompt.txt"), "w") as handle:
            handle.write(prompt + "\n")
        environment = dict(os.environ)
        environment["CODEX_HOME"] = home
        command = [
            shutil.which("codex") or "codex",
            "exec",
            "--json",
            "--skip-git-repo-check",
            "--ignore-rules",
            "-C",
            workdir,
            "--approve-for-me",
            "-c",
            "sandbox_workspace_write.network_access=true",
            "--output-last-message",
            os.path.join(workdir, "last-message.txt"),
        ]
        if self.model:
            command += ["-m", self.model]
        command.append(prompt)
        stream = open(os.path.join(workdir, "stream.jsonl"), "w")
        errors = open(os.path.join(workdir, "stderr.log"), "w")
        process = subprocess.Popen(
            command,
            cwd=workdir,
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=stream,
            stderr=errors,
            start_new_session=True,
        )
        return Handle(process, workdir, time.time())

    def parse(self, workdir):
        transcript = rows.parse_codex_stream(os.path.join(workdir, "stream.jsonl"))
        transcript["model"] = self.model or read_codex_model(os.path.join(workdir, "codex-home"))
        last = os.path.join(workdir, "last-message.txt")
        if os.path.exists(last) and not transcript.get("result_text"):
            with open(last, encoding="utf-8", errors="replace") as handle:
                transcript["result_text"] = handle.read()
        return transcript


def read_codex_model(home):
    """Codex reports no model in its event stream; the rollout files have it.

    The automatic reviewer writes a rollout of its own under the same home, so
    its model is read and dropped: what the row names is the model that did
    the work.
    """
    files = sorted(
        glob.glob(os.path.join(home, "sessions", "**", "rollout-*.jsonl"), recursive=True),
        key=os.path.getmtime,
    )
    for path in files:
        with open(path, encoding="utf-8", errors="replace") as handle:
            for found in re.findall(r'"model":"([^"]+)"', handle.read()):
                if "review" not in found:
                    return found
    return None


class FakeAdapter(Adapter):
    """An agent that copies a captured transcript. The tests run on this."""

    name = "fake"

    def __init__(self, stream_path, kind="claude", delay=0.0):
        self.stream_path = stream_path
        self.kind = kind
        self.delay = delay

    def start(self, prompt, workdir, ssh_command):
        os.makedirs(workdir, exist_ok=True)
        shutil.copyfile(self.stream_path, os.path.join(workdir, "stream.jsonl"))
        with open(os.path.join(workdir, "prompt.txt"), "w") as handle:
            handle.write(prompt + "\n")
        process = subprocess.Popen(
            ["sleep", str(self.delay)], stdin=subprocess.DEVNULL, start_new_session=True
        )
        return Handle(process, workdir, time.time())

    def parse(self, workdir):
        transcript = rows.PARSERS[self.kind](os.path.join(workdir, "stream.jsonl"))
        transcript["agent"] = "fake"
        return transcript


def build(name, model=None):
    if name == "claude":
        return ClaudeAdapter(model=model)
    if name == "codex":
        return CodexAdapter(model=model)
    raise SystemExit("no adapter named %s" % name)
