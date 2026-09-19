#!/usr/bin/env python3
"""Bench fixture. The dependency: a key value store the notes page reads through."""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MARKER = "bench-cache"
PORT = int(os.environ.get("PORT", "8080"))
STORE = {}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        print("%s %s" % (self.command, self.path), flush=True)

    def send(self, status, body, ctype="application/json"):
        raw = body.encode() if isinstance(body, str) else body
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path in ("/", "/healthz"):
            return self.send(200, "%s ok, %d keys" % (MARKER, len(STORE)), "text/plain")
        if self.path.startswith("/kv/"):
            return self.send(200, json.dumps({"value": STORE.get(self.path[4:], "")}))
        return self.send(404, json.dumps({"error": "not found"}))

    def do_POST(self):
        if not self.path.startswith("/kv/"):
            return self.send(404, json.dumps({"error": "not found"}))
        length = int(self.headers.get("Content-Length") or 0)
        STORE[self.path[4:]] = (self.rfile.read(length) or b"").decode()
        return self.send(200, json.dumps({"ok": True}))


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
