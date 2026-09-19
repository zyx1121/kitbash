"""A minimal MCP stdio client over ssh, enough to call the kitbash surface.

The harness is a member and an admin of the host like any other client: it
speaks the same protocol an agent speaks, over the same ForceCommand session.
"""

import json
import subprocess
import threading

PROTOCOL = "2025-06-18"


class McpError(Exception):
    """A tool answered isError, or the transport did."""

    def __init__(self, tool, detail):
        super().__init__("%s: %s" % (tool, detail))
        self.tool = tool
        self.detail = detail


class Session:
    """One MCP session. Open it, call tools, close it."""

    def __init__(self, ssh_command, timeout=120):
        self.ssh_command = list(ssh_command)
        self.timeout = timeout
        self.proc = None
        self.next_id = 1
        self.lock = threading.Lock()

    def __enter__(self):
        self.open()
        return self

    def __exit__(self, *exc):
        self.close()
        return False

    def open(self):
        self.proc = subprocess.Popen(
            self.ssh_command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
        answer = self._request(
            "initialize",
            {
                "protocolVersion": PROTOCOL,
                "capabilities": {},
                "clientInfo": {"name": "kitbash-bench", "version": "1"},
            },
        )
        self._notify("notifications/initialized")
        return answer

    def close(self):
        if not self.proc:
            return
        try:
            self.proc.stdin.close()
        except Exception:
            pass
        try:
            self.proc.wait(timeout=10)
        except Exception:
            self.proc.kill()
        self.proc = None

    def _send(self, message):
        self.proc.stdin.write(json.dumps(message) + "\n")
        self.proc.stdin.flush()

    def _notify(self, method, params=None):
        self._send({"jsonrpc": "2.0", "method": method, "params": params or {}})

    def _request(self, method, params):
        with self.lock:
            ident = self.next_id
            self.next_id += 1
            self._send({"jsonrpc": "2.0", "id": ident, "method": method, "params": params})
            while True:
                line = self.proc.stdout.readline()
                if not line:
                    stderr = self.proc.stderr.read() if self.proc.stderr else ""
                    raise McpError(method, "the session ended: %s" % stderr.strip()[:400])
                try:
                    message = json.loads(line)
                except ValueError:
                    continue
                if message.get("id") == ident:
                    return message

    def instructions(self):
        return ""

    def call(self, tool, arguments=None):
        """Call a tool and answer its structured content, or raise McpError."""
        message = self._request("tools/call", {"name": tool, "arguments": arguments or {}})
        if "error" in message:
            raise McpError(tool, json.dumps(message["error"])[:400])
        result = message.get("result") or {}
        text = ""
        for block in result.get("content") or []:
            if block.get("type") == "text":
                text += block.get("text") or ""
        if result.get("isError"):
            raise McpError(tool, text[:600])
        if result.get("structuredContent") is not None:
            return result["structuredContent"]
        try:
            return json.loads(text)
        except ValueError:
            return {"text": text}

    def tools(self):
        message = self._request("tools/list", {})
        return [t["name"] for t in (message.get("result") or {}).get("tools", [])]


def ssh_command(alias, config=None):
    """The command an MCP client runs to reach one member's session."""
    command = ["ssh", "-o", "BatchMode=yes"]
    if config:
        command += ["-F", config]
    command.append(alias)
    return command


def call_once(alias, tool, arguments=None, config=None, timeout=120):
    with Session(ssh_command(alias, config), timeout=timeout) as session:
        return session.call(tool, arguments)
