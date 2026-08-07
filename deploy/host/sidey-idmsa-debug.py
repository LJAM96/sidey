#!/usr/bin/env python3
"""Sidey idmsa auth diagnostic: replicates AltServer-Linux v0.0.5's AltSign
login flow (GET signin for session cookies, then POST credentials) and prints
Apple's EXACT response, so we can tell a credentials problem from an
old-client/anisette problem.

Usage:
  ANISETTE_ACCOUNT='you@example.com' ANISETTE_PASSWORD='app-specific' \
      python3 sidey-idmsa-debug.py
"""

import json
import os
import sys
import urllib.request
import http.cookiejar

ANISETTE = os.environ.get("ANISETTE_SERVER", "http://127.0.0.1:6969/")
ACCOUNT = os.environ.get("ANISETTE_ACCOUNT", "")
PASSWORD = os.environ.get("ANISETTE_PASSWORD", "")

if not ACCOUNT or not PASSWORD:
    print("Set ANISETTE_ACCOUNT and ANISETTE_PASSWORD env vars", file=sys.stderr)
    sys.exit(2)

with urllib.request.urlopen(ANISETTE, timeout=20) as r:
    anisette = json.load(r)

base_headers = {k: str(v) for k, v in anisette.items()}
base_headers.update({
    "Accept": "application/json",
    "Content-Type": "application/json",
    "User-Agent": "Configurator/2.15 (Macintosh; OS X 10.15.4) AppleWebKit/2603.3.8 (KHTML, like Gecko)",
    "X-Apple-Locale": "en_US",
})

cj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
URL = "https://idmsa.apple.com/appleauth/auth/authorize/signin"

def show(label, resp_or_err, body):
    code = getattr(resp_or_err, "code", "ERR")
    ct = resp_or_err.headers.get("Content-Type") if isinstance(resp_or_err, urllib.error.HTTPError) else getattr(resp_or_err, "headers", {}).get("Content-Type", "n/a")
    print(f"\n=== {label}: HTTP {code} | Content-Type: {ct}")
    print(f"cookies: {[c.name for c in cj]}")
    print(f"body ({len(body)} bytes): {body[:600]}")

req0 = urllib.request.Request(URL, headers=base_headers)
try:
    with opener.open(req0, timeout=30) as r:
        body = r.read()
        show("GET signin", r, body)
except urllib.error.HTTPError as e:
    show("GET signin", e, e.read())
except Exception as e:
    print("GET signin NETWORK ERROR:", type(e).__name__, e)

payload = json.dumps({"accountName": ACCOUNT, "password": PASSWORD, "rememberMe": False}).encode()
req = urllib.request.Request(URL, data=payload, headers=base_headers, method="POST")
try:
    with opener.open(req, timeout=30) as r:
        show("POST signin", r, r.read())
except urllib.error.HTTPError as e:
    show("POST signin", e, e.read())
except Exception as e:
    print("POST signin NETWORK ERROR:", type(e).__name__, e)
