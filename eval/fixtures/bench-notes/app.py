#!/usr/bin/env python3
"""Bench fixture. A notes page that keeps nothing itself and reads bench-cache."""
import json
import os
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MARKER = "bench-notes"
PORT = int(os.environ.get("PORT", "8080"))
# The cache is another Process of this member, reached through the host proxy.
ORIGIN = os.environ.get("CACHE_ORIGIN", "http://host.containers.internal")
CACHE_HOST = os.environ.get("CACHE_HOST", "")
KEY = "notes"


def cache(method, body=None):
    request = urllib.request.Request(
        "%s/kv/%s" % (ORIGIN, KEY),
        data=body.encode() if body is not None else None,
        method=method,
        headers={"Host": CACHE_HOST} if CACHE_HOST else {},
    )
    with urllib.request.urlopen(request, timeout=5) as answer:
        return answer.read().decode()


def read_notes():
    return json.loads(json.loads(cache("GET")).get("value") or "[]")


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
        if self.path == "/healthz":
            return self.send(200, "ok", "text/plain")
        if self.path not in ("/", "/index.html", "/api/notes"):
            return self.send(404, json.dumps({"error": "not found"}))
        try:
            notes = read_notes()
        except Exception as exc:
            return self.send(
                503,
                json.dumps({"error": "the notes cache at %s is unreachable: %s" % (CACHE_HOST, exc)}),
            )
        if self.path == "/api/notes":
            return self.send(200, json.dumps({"notes": notes}))
        rows = "".join("<li>%s</li>" % n for n in notes)
        page = (
            "<!doctype html><html><head><title>%s</title></head><body>"
            "<h1>%s</h1><p>%d notes</p><ul>%s</ul></body></html>"
            % (MARKER, MARKER, len(notes), rows)
        )
        return self.send(200, page, "text/html; charset=utf-8")

    def do_POST(self):
        if self.path != "/api/notes":
            return self.send(404, json.dumps({"error": "not found"}))
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"{}")
        except Exception:
            return self.send(400, json.dumps({"error": "body is not JSON"}))
        text = str(body.get("text", "")).strip()
        if not text:
            return self.send(400, json.dumps({"error": "text is required"}))
        try:
            notes = read_notes()
            notes.append(text)
            cache("POST", json.dumps(notes))
        except Exception as exc:
            return self.send(
                503,
                json.dumps({"error": "the notes cache at %s is unreachable: %s" % (CACHE_HOST, exc)}),
            )
        return self.send(201, json.dumps({"ok": True, "count": len(notes)}))


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
