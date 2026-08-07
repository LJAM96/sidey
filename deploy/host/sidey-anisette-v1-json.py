#!/usr/bin/env python3
"""Sidey anisette v1 JSON shim.

omnisette-server's v1 endpoint (GET /) returns the anisette headers as a
JSON document but with a text/plain Content-Type, which AltServer-Linux's
cpprestsdk extract_json() rejects ("Incorrect Content-Type: must be textual
to extract_string, JSON to extract_json"). This shim proxies requests to
omnisette and rewrites the Content-Type to application/json for the v1 root.

Listens on 127.0.0.1:6969 (the port AltServer is configured with) and
forwards to omnisette on 127.0.0.1:6970. Only plain HTTP is relayed;
WebSocket upgrades (used by the v3 protocol) are not supported here —
clients like isideload should talk to omnisette directly on 6970.
"""

import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_BASE = "http://127.0.0.1:6970"
LISTEN = ("127.0.0.1", 6969)
UA = "Xcode"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("Upgrade", "").lower() == "websocket":
            self.send_response(404)
            self.end_headers()
            return
        url = UPSTREAM_BASE + self.path
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        try:
            with urllib.request.urlopen(req, timeout=20) as up:
                body = up.read()
                status = up.status
                upstream_ct = up.headers.get("Content-Type", "")
        except Exception as e:
            self.send_response(502)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(str(e).encode())
            return
        if self.path == "/":
            content_type = "application/json"
        else:
            content_type = upstream_ct or "application/octet-stream"
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    try:
        ThreadingHTTPServer(LISTEN, Handler).serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)
