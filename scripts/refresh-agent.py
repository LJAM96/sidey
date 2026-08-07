#!/usr/bin/env python3
"""Sidey refresh agent.

Claims refresh jobs from the control plane for the configured device and
executes the wireless-install.sh wrapper (re-sign + install over the RSD
tunnel), reporting progress back and keeping the job lease alive while the
install runs.

Stdlib-only. Run as root (the install wrapper needs sudo/root and the creds
file is root-only).

Env:
  SIDEY_API_URL          control plane base URL (default http://127.0.0.1:8080)
  SIDEY_AGENT_KEY        agent API key (persisted to state dir after enrol)
  SIDEY_ENROLMENT_TOKEN  one-time token to enrol on first start
  SIDEY_AGENT_STATE_DIR  state dir for key/agent id (default /var/lib/sidey/refresh-agent)
  SIDEY_DEVICE_UDID      device to refresh (default 00008120-001E11211184C01E)
  SIDEY_DEVICE_NAME      reported device name (default ACU Covert Camera)
  SIDEY_REFRESH_WRAPPER  install wrapper script (default <repo>/scripts/wireless-install.sh)
  SIDEY_POLL_SECONDS     claim poll interval (default 30)
  SIDEY_HEARTBEAT_SECONDS  in_progress heartbeat interval (default 60)
"""
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

API_URL = os.environ.get("SIDEY_API_URL", "http://127.0.0.1:8080").rstrip("/")
AGENT_KEY = os.environ.get("SIDEY_AGENT_KEY", "")
ENROLMENT_TOKEN = os.environ.get("SIDEY_ENROLMENT_TOKEN", "")
STATE_DIR = Path(os.environ.get("SIDEY_AGENT_STATE_DIR", "/var/lib/sidey/refresh-agent"))
DEVICE_UDID = os.environ.get("SIDEY_DEVICE_UDID", "00008120-001E11211184C01E")
DEVICE_NAME = os.environ.get("SIDEY_DEVICE_NAME", "ACU Covert Camera")
REPO_DIR = Path(__file__).resolve().parent.parent
WRAPPER = os.environ.get("SIDEY_REFRESH_WRAPPER", str(REPO_DIR / "scripts/wireless-install.sh"))
POLL_SECONDS = int(os.environ.get("SIDEY_POLL_SECONDS", "30"))
HEARTBEAT_SECONDS = int(os.environ.get("SIDEY_HEARTBEAT_SECONDS", "60"))
# The tunnel can die mid-transfer without any error surfacing (no socket
# timeouts in the installer), which would otherwise hang a job forever while
# the heartbeat keeps its lease alive. The timeout kills the wrapper and
# fails the job so the retry loop can re-queue it.
JOB_TIMEOUT_SECONDS = int(os.environ.get("SIDEY_JOB_TIMEOUT", "900"))

KEY_FILE = STATE_DIR / "api_key"
AGENT_FILE = STATE_DIR / "agent_id"


def log(msg):
    print(f"[refresh-agent] {msg}", file=sys.stderr, flush=True)


def request(method, path, body=None, bearer=None, timeout=30):
    req = urllib.request.Request(API_URL + path, method=method)
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    if bearer:
        req.add_header("Authorization", f"Bearer {bearer}")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = resp.read()
        return resp.status, json.loads(data) if data else None


def ensure_api_key():
    """Use the persisted agent key, or enrol once and persist it."""
    if AGENT_KEY:
        return AGENT_KEY
    if KEY_FILE.exists():
        return KEY_FILE.read_text().strip()
    if not ENROLMENT_TOKEN:
        raise SystemExit(
            "no agent key (SIDEY_AGENT_KEY or %s) and no SIDEY_ENROLMENT_TOKEN" % KEY_FILE)
    log("enrolling agent...")
    body = {
        "name": "refresh-agent",
        "architecture": "arm64",
        "operating_system": "linux",
        "software_version": "1.0",
        "tailnet_identity": "100.100.12.70",
        "capabilities": {"refresh": True},
    }
    status, resp = request("POST", "/api/v1/agents/enrol", body, bearer=ENROLMENT_TOKEN)
    if status != 201:
        raise SystemExit(f"enrol failed ({status}): {resp}")
    key = resp["api_key"]
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    KEY_FILE.write_text(key)
    AGENT_FILE.write_text(str(resp["agent_id"]))
    log(f"enrolled as agent {resp['agent_id']}")
    return key


def wait_for_api(key):
    while True:
        try:
            request("POST", "/api/v1/agents/me/heartbeat", {}, bearer=key, timeout=10)
            return
        except Exception as exc:
            log(f"API not ready ({exc}); retrying in 10s...")
            time.sleep(10)


def find_device_uuid(key):
    """Report the device and resolve its database UUID."""
    body = {
        "devices": [{
            "udid": DEVICE_UDID,
            "device_name": DEVICE_NAME,
            "model": "iPhone 15 Pro",
            "os_version": "27.0",
            "platform": "ios",
            "pairing_status": "paired",
            "developer_mode_enabled": True,
        }]
    }
    status, resp = request("POST", "/api/v1/agents/me/devices", body, bearer=key)
    if status != 200:
        raise RuntimeError(f"device report failed ({status}): {resp}")
    for d in resp.get("devices", []):
        if d.get("udid") == DEVICE_UDID:
            return d["id"]
    raise RuntimeError(f"device {DEVICE_UDID} not returned by report: {resp}")


def claim(key, device_uuid):
    status, resp = request(
        "POST", "/api/v1/jobs/claim",
        {"device_ids": [device_uuid], "job_types": ["refresh"], "limit": 1},
        bearer=key, timeout=30)
    if status != 200:
        raise RuntimeError(f"claim failed ({status}): {resp}")
    return resp.get("jobs", [])


def post_status(key, job_id, state, progress=None, error_category=None,
                error_details=None, result=None):
    body = {"state": state}
    if progress is not None:
        body["progress"] = progress
    if error_category is not None:
        body["error_category"] = error_category
    if error_details is not None:
        body["error_details"] = error_details
    if result is not None:
        body["result"] = result
    status, resp = request(
        "POST", f"/api/v1/jobs/{job_id}/status", body, bearer=key, timeout=30)
    if status == 409:
        raise RuntimeError(f"update {state} rejected (lease lost?): {resp}")
    if status != 200:
        raise RuntimeError(f"update {state} failed ({status}): {resp}")
    return resp


def extract_progress(line):
    m = re.search(r"(\d+)%", line)
    return int(m.group(1)) if m else None


def run_job(key, job):
    job_id = job["id"]
    job_type = job.get("job_type")
    if job_type != "refresh":
        log(f"job {job_id}: unsupported job type {job_type}, failing")
        post_status(key, job_id, "failed",
                    error_category="unsupported_job_type",
                    error_details=f"refresh agent cannot execute {job_type} jobs")
        return
    log(f"job {job_id}: starting refresh")
    post_status(key, job_id, "in_progress", progress=0)

    progress = {"value": 0}
    timed_out = {"value": False}
    started = time.time()
    stop = threading.Event()

    def heartbeat():
        while not stop.wait(HEARTBEAT_SECONDS):
            try:
                post_status(key, job_id, "in_progress", progress=progress["value"])
            except Exception as exc:
                log(f"heartbeat failed: {exc}")
            if time.time() - started > JOB_TIMEOUT_SECONDS:
                timed_out["value"] = True
                log(f"job {job_id}: timeout after {JOB_TIMEOUT_SECONDS}s, killing wrapper")
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass

    proc = subprocess.Popen(
        [WRAPPER], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        start_new_session=True)
    thread = threading.Thread(target=heartbeat, daemon=True)
    thread.start()

    captured = []
    try:
        for line in proc.stdout:
            line = line.rstrip()
            if line:
                log(f"  {line}")
                captured.append(line)
                pct = extract_progress(line)
                if pct is not None:
                    progress["value"] = pct
        rc = proc.wait()
    except Exception as exc:
        rc = 1
        captured.append(f"wrapper error: {exc}")
    finally:
        stop.set()
        thread.join(timeout=2)

    duration = int(time.time() - started)
    tail = "\n".join(captured[-25:])[-2000:]
    if timed_out["value"]:
        tail = (tail + f"\n[timed out after {JOB_TIMEOUT_SECONDS}s]").strip()[-2000:]
    if rc == 0:
        log(f"job {job_id}: completed in {duration}s")
        post_status(key, job_id, "completed", progress=100,
                    result={"verified": True, "duration_seconds": duration})
    else:
        log(f"job {job_id}: FAILED (rc={rc})")
        post_status(key, job_id, "failed",
                    error_category="refresh_timeout" if timed_out["value"] else "refresh_failed",
                    error_details=tail or f"wrapper exited {rc}")


def main():
    key = ensure_api_key()
    wait_for_api(key)
    device_uuid = find_device_uuid(key)
    log(f"device {DEVICE_UDID} -> {device_uuid}; polling every {POLL_SECONDS}s")
    while True:
        try:
            request("POST", "/api/v1/agents/me/heartbeat", {}, bearer=key, timeout=10)
            jobs = claim(key, device_uuid)
            for job in jobs:
                run_job(key, job)
        except Exception as exc:
            log(f"poll error: {exc}")
        time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    main()
