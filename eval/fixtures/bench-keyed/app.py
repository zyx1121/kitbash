#!/usr/bin/env python3
"""Bench fixture. A service that refuses to start without its API key."""
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MARKER = "bench-keyed"
PORT = int(os.environ.get("PORT", "8080"))
KEY = os.environ.get("BENCH_API_KEY", "")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        print("%s %s" % (self.command, self.path), flush=True)

    def do_GET(self):
        body = ("%s ok, key of %d characters" % (MARKER, len(KEY))).encode()
        if self.path not in ("/", "/healthz"):
            body = b"not found"
            self.send_response(404)
        else:
            self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    if not KEY:
        sys.stderr.write("BENCH_API_KEY is not set\n")
        sys.exit(1)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
