#!/usr/bin/env python3
"""Bench fixture. A shared board with a JSON API, backed by one file on a mount."""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MARKER = "bench-board"
DATA = os.environ.get("BOARD_DATA", "/data")
ITEMS = os.path.join(DATA, "items.json")
PORT = int(os.environ.get("PORT", "8080"))


def load():
    try:
        with open(ITEMS) as handle:
            return json.load(handle)
    except FileNotFoundError:
        return []
    except Exception:
        return []


def save(items):
    # Written in place on purpose: a read only items.json is one of the faults.
    with open(ITEMS, "w") as handle:
        json.dump(items, handle)


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
        if self.path == "/api/items":
            return self.send(200, json.dumps({"items": load()}))
        if self.path in ("/", "/index.html"):
            items = load()
            rows = "".join("<li>%s</li>" % i.get("text", "") for i in items)
            page = (
                "<!doctype html><html><head><title>%s</title></head><body>"
                "<h1>%s</h1><p>%d items</p><ul>%s</ul></body></html>"
                % (MARKER, MARKER, len(items), rows)
            )
            return self.send(200, page, "text/html; charset=utf-8")
        return self.send(404, json.dumps({"error": "not found"}))

    def do_POST(self):
        if self.path != "/api/items":
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
            items = load()
            items.append({"text": text, "author": body.get("author", "anonymous")})
            save(items)
        except Exception as exc:
            return self.send(500, json.dumps({"error": "cannot write the board: %s" % exc}))
        return self.send(201, json.dumps({"ok": True, "count": len(items)}))


if __name__ == "__main__":
    os.makedirs(DATA, exist_ok=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
