#!/usr/bin/env python3
"""Sidey device service (ADR-0008).

The first-party service that owns devices on a host and executes the control
plane's intents (jobs) for them: install, verify, refresh, inventory.

Transport (default is the same-host Unix socket, no credentials):
  - Same host: HTTP over SIDEY_DEVICE_SOCKET (/run/sidey/device.sock) with no
    bearer key. The socket's filesystem permissions are the authentication.
  - Remote node (optional, multi-site): SIDEY_API_URL + agent key, enrolled
    once from SIDEY_ENROLMENT_TOKEN. Heartbeats keep the node visible.

Stdlib-only. Run as root when executing the install wrappers (wireless
install needs root for the TUN/tunnel and tvOS install needs root for RSD).

Intents (jobs) are dispatched by job_type and the job's `parameters`:
  platform      ios (default) | tvos
  device_udid   target device UDID (defaults to env/configured hints)
  device_ip     device tailnet/RP address
  device_port   tvOS RSD port (default 49152)
  artifact_id   signed artifact to download before executing (optional; jobs
                that carry it bind the download to the held job)

CLI: run without arguments as the daemon; `sidey-device.py <verb> [args]`
executes one verb and exits (health, inventory, install, refresh, verify,
uninstall, tunnel).
"""
import http.client
import json
import os
import platform
import re
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

DEVICE_SOCKET = os.environ.get("SIDEY_DEVICE_SOCKET", "/run/sidey/device.sock")
API_URL = os.environ.get("SIDEY_API_URL", "").rstrip("/")
AGENT_KEY = os.environ.get("SIDEY_AGENT_KEY", "")
ENROLMENT_TOKEN = os.environ.get("SIDEY_ENROLMENT_TOKEN", "")
STATE_DIR = Path(os.environ.get("SIDEY_AGENT_STATE_DIR", "/var/lib/sidey/device-service"))
REPO_DIR = Path(__file__).resolve().parent.parent
VENV_PY = os.environ.get("SIDEY_VENV_PY", "/opt/sidey/venv-pmd3/bin/python3")
IPA_DIR = Path(os.environ.get("SIDEY_IPA_DIR", "/var/lib/sidey/device-service/ipas"))
POLL_SECONDS = int(os.environ.get("SIDEY_POLL_SECONDS", "30"))
HEARTBEAT_SECONDS = int(os.environ.get("SIDEY_HEARTBEAT_SECONDS", "60"))
# The tunnel can die mid-transfer without surfacing an error; the deadline
# kills the wrapper and fails the job so the retry loop re-queues it.
JOB_TIMEOUT_SECONDS = int(os.environ.get("SIDEY_JOB_TIMEOUT", "1200"))

KEY_FILE = STATE_DIR / "api_key"
AGENT_FILE = STATE_DIR / "agent_id"

# Configured device hints, reported into inventory when live probing is
# unavailable. Per-job values override these.
DEVICE_UDID = os.environ.get("SIDEY_DEVICE_UDID", "")
DEVICE_NAME = os.environ.get("SIDEY_DEVICE_NAME", socket.gethostname())
DEVICE_IP = os.environ.get("SIDEY_DEVICE_IP", "")
TVOS_UDID = os.environ.get("SIDEY_TVOS_UDID", "")
TVOS_NAME = os.environ.get("SIDEY_TVOS_NAME", "Apple TV")
TVOS_IP = os.environ.get("SIDEY_TVOS_IP", "")
TVOS_PORT = os.environ.get("SIDEY_TVOS_PORT", "49152")

IOS_INSTALL_WRAPPER = os.environ.get(
    "SIDEY_IOS_INSTALL_WRAPPER", str(REPO_DIR / "scripts/wireless-install.sh"))
TVOS_INSTALL_WRAPPER = os.environ.get(
    "SIDEY_TVOS_INSTALL_WRAPPER", str(REPO_DIR / "scripts/tvos-install.sh"))
TVOS_UNINSTALL_WRAPPER = os.environ.get(
    "SIDEY_TVOS_UNINSTALL_WRAPPER", str(REPO_DIR / "scripts/tvos-uninstall.sh"))
TUNNEL_SCRIPT = os.environ.get(
    "SIDEY_WIRELESS_TUNNEL", str(REPO_DIR / "scripts/wireless-tunnel.py"))


def log(msg):
    print(f"[device-service] {msg}", file=sys.stderr, flush=True)


def _write_0600(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    os.chmod(path, 0o600)


def _read_key_file():
    if not KEY_FILE.exists():
        return None
    try:
        os.chmod(KEY_FILE, 0o600)
    except OSError:
        pass
    return KEY_FILE.read_text().strip()


class ControlPlane:
    """REST client over either the Unix socket (same-host) or HTTPS/HTTP
    (remote node). Presents one surface to the daemon."""

    def __init__(self):
        self.remote = bool(API_URL)
        self.sock_path = None if self.remote else DEVICE_SOCKET
        self._key = None
        self._agent_id = None
        if self.remote:
            self._key = AGENT_KEY or _read_key_file()

    # -- local helpers -----------------------------------------------------

    def _resolve(self):
        """Remote nodes need an API key before any control-plane call:
        resolve the persisted key or enrol once from the token."""
        if self.remote and not self._key:
            self._enrol()
        return self._key

    def _path_for(self, intent):
        # Same logical operation, two transports: the socket channel trusts
        # the local peer and needs no auth.
        if not self.remote:
            if intent == "health":
                return "GET", "/api/v1/device/health", None
            if intent == "me":
                return "GET", "/api/v1/device/me", None
            if intent == "heartbeat":
                return "POST", "/api/v1/device/me/heartbeat", None
            if intent == "report_devices":
                return "POST", "/api/v1/device/me/devices", None
            if intent == "claim":
                return "POST", "/api/v1/device/jobs/claim", None
            if intent == "status":
                return "POST", "/api/v1/device/jobs/{id}/status", None
            if intent == "download":
                return "GET", "/api/v1/device/artifacts/{id}/download", None
        if intent == "health":
            return "GET", "/api/v1/healthz", None
        if intent == "heartbeat":
            return "POST", "/api/v1/agents/me/heartbeat", self._key
        if intent == "report_devices":
            return "POST", "/api/v1/agents/me/devices", self._key
        if intent == "claim":
            return "POST", "/api/v1/jobs/claim", self._key
        if intent == "status":
            return "POST", "/api/v1/jobs/{id}/status", self._key
        if intent == "download":
            return "GET", "/api/v1/agents/artifacts/{id}/download", self._key
        if intent == "enrol":
            return "POST", "/api/v1/agents/enrol", None
        raise RuntimeError(f"unknown control plane operation: {intent}")

    def request(self, intent, body=None, sub=None, timeout=60, raw=False):
        self._resolve()
        method, path, bearer = self._path_for(intent)
        if sub is not None:
            path = path.replace("{id}", str(sub))
        payload = None
        if body is not None:
            payload = json.dumps(body).encode()
        status, resp = self._do(method, path, payload, bearer=bearer, timeout=timeout, raw=raw)
        if status >= 400:
            detail = ""
            if resp is not None and not isinstance(resp, bytes):
                detail = f": {resp}"
            raise RuntimeError(f"{method} {path} -> {status}{detail}")
        if raw or isinstance(resp, bytes):
            return status, resp
        return status, resp

    def _do(self, method, path, payload, bearer, timeout, raw=False):
        headers = {}
        if payload is not None:
            headers["Content-Type"] = "application/json"
        if self.remote:
            req = urllib.request.Request(API_URL + path, data=payload, method=method,
                                         headers=headers)
            if bearer:
                req.add_header("Authorization", f"Bearer {bearer}")
            try:
                with urllib.request.urlopen(req, timeout=timeout) as resp:
                    data = resp.read()
                    return resp.status, (data if raw else _json(body=data))
            except urllib.error.HTTPError as exc:
                body = exc.read()
                return exc.code, (body if raw else _json(body=body))
        conn = _UnixHTTPConnection(self.sock_path, timeout=timeout)
        if not Path(self.sock_path).exists():
            raise RuntimeError(f"device socket not present: {self.sock_path}")
        try:
            conn.request(method, path, body=payload, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
            return resp.status, (data if raw else _json(body=data))
        except (ConnectionRefusedError, FileNotFoundError) as exc:
            raise RuntimeError(f"device socket unreachable ({self.sock_path}): {exc}")
        finally:
            conn.close()

    # -- control plane operations ------------------------------------------

    def enrol(self):
        body = {
            "name": "device-service",
            "architecture": platform.machine(),
            "operating_system": "linux",
            "software_version": "1.0",
            "tailnet_identity": socket.gethostname(),
            "capabilities": {"install": True, "verify": True, "refresh": True},
        }
        _, resp = self.request("enrol", body=body, timeout=30)
        self._key = resp["api_key"]
        self._agent_id = resp["agent_id"]
        _write_0600(KEY_FILE, self._key)
        _write_0600(AGENT_FILE, str(self._agent_id))
        log(f"enrolled as device service node {self._agent_id}")
        return self._key

    def _enrol(self):
        if not ENROLMENT_TOKEN:
            raise RuntimeError(
                "remote mode needs SIDEY_AGENT_KEY or SIDEY_ENROLMENT_TOKEN")
        bearer = ENROLMENT_TOKEN
        # Enrolment takes the token in the Authorization header directly over
        # the remote path, then consumes it.
        method, path, _ = self._path_for("enrol")
        payload = json.dumps({
            "name": "device-service",
            "architecture": platform.machine(),
            "operating_system": "linux",
            "software_version": "1.0",
            "tailnet_identity": socket.gethostname(),
            "capabilities": {"install": True, "verify": True, "refresh": True},
        }).encode()
        status, resp = self._do(method, path, payload, bearer=bearer, timeout=30)
        if status != 201:
            raise RuntimeError(f"enrol failed ({status}): {resp}")
        self._key = resp["api_key"]
        self._agent_id = resp["agent_id"]
        _write_0600(KEY_FILE, self._key)
        _write_0600(AGENT_FILE, str(self._agent_id))
        log(f"enrolled as device service node {self._agent_id}")

    def me(self):
        _, resp = self.request("me", timeout=15)
        return resp

    def heartbeat(self, capabilities=None):
        self.request("heartbeat", body={"capabilities": capabilities or {}}, timeout=15)

    def report_devices(self, devices):
        _, resp = self.request("report_devices", body={"devices": devices}, timeout=30)
        return resp.get("devices", [])

    def claim(self, device_ids=None, job_types=None, limit=5):
        _, resp = self.request("claim", body={
            "device_ids": device_ids or [], "job_types": job_types or [], "limit": limit},
            timeout=30)
        return resp.get("jobs", [])

    def update(self, job_id, state, progress=None, error_category=None,
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
        self.request("status", body=body, sub=job_id, timeout=30)

    def download(self, artifact_id):
        _, blob = self.request("download", sub=artifact_id, timeout=600, raw=True)
        return blob


class _UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path, timeout=30):
        super().__init__("localhost", timeout=timeout)
        self._socket_path = socket_path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self._socket_path)


def _json(body):
    if not body:
        return None
    return json.loads(body.decode())


# --------------------------------------------------------------------------
# Inventory
# --------------------------------------------------------------------------

def probe_ios_devices():
    """Ask pymobiledevice3 (via the venv) for connected/paired devices."""
    if not Path(VENV_PY).exists():
        return []
    candidates = [
        [VENV_PY, "-m", "pymobiledevice3", "list", "--json"],
        [VENV_PY, "-m", "pymobiledevice3", "list"],
    ]
    out = ""
    for cand in candidates:
        proc = subprocess.run(cand, capture_output=True, text=True, timeout=60)
        if proc.returncode == 0 and (proc.stdout or "").strip():
            out = proc.stdout.strip()
            break
    if not out or out.startswith("usage"):
        return []
    devices = []
    try:
        entries = json.loads(out)
    except json.JSONDecodeError:
        return []
    for entry in entries:
        udid = entry.get("udid") or entry.get("serial_number")
        if udid:
            devices.append({
                "udid": udid,
                "platform": "ios",
                "device_name": entry.get("name", DEVICE_NAME),
                "model": entry.get("model", ""),
                "os_version": entry.get("os_version", ""),
                "pairing_status": "paired",
                "developer_mode_enabled": True,
            })
    return devices


def build_inventory(cp):
    devices = probe_ios_devices()
    if DEVICE_UDID:
        devices.append({
            "udid": DEVICE_UDID, "platform": "ios", "device_name": DEVICE_NAME,
            "model": "iPhone", "os_version": "", "pairing_status": "paired",
        })
    if TVOS_UDID:
        devices.append({
            "udid": TVOS_UDID, "platform": "tvos", "device_name": TVOS_NAME,
            "model": "Apple TV", "os_version": "", "pairing_status": "paired",
        })
    return cp.report_devices(devices)


# --------------------------------------------------------------------------
# Intent (job) execution
# --------------------------------------------------------------------------

def _job_target(job):
    """Resolve platform/device/ipa for a job from parameters + env hints."""
    params = job.get("parameters") or {}
    if isinstance(params, str):
        params = json.loads(params)
    platform_name = params.get("platform", "ios")
    udid = params.get("device_udid") or (TVOS_UDID if platform_name == "tvos" else DEVICE_UDID)
    ip = params.get("device_ip") or (TVOS_IP if platform_name == "tvos" else DEVICE_IP)
    port = params.get("device_port") or (TVOS_PORT if platform_name == "tvos" else "")
    return platform_name, udid, ip, port, params


def _wrapper_for(platform_name, wrapper, ipa, udid, ip, port):
    env = dict(os.environ)
    cmd = [wrapper]
    if ipa:
        cmd.append(ipa)
    if platform_name == "tvos":
        env["DEVICE_UDID"] = udid
        env["DEVICE_IDENTIFIER"] = udid
        env["DEVICE_IP"] = ip
        env["DEVICE_PORT"] = port or TVOS_PORT
    else:
        env["DEVICE_UDID"] = udid or DEVICE_UDID
        env["DEVICE_IP"] = ip or DEVICE_IP
    return cmd, env


def _resolve_ipa(cp, job, cache_dir):
    """Download the signed artifact when the job carries artifact_id,
    otherwise return None (the wrapper falls back to its configured IPA)."""
    params = job.get("parameters") or {}
    if isinstance(params, str):
        params = json.loads(params)
    artifact_id = params.get("artifact_id") or params.get("signed_artifact_id")
    if not artifact_id:
        return None
    cache_dir.mkdir(parents=True, exist_ok=True)
    blob = cp.download(artifact_id)
    path = cache_dir / f"{artifact_id}.ipa"
    tmp = path.with_suffix(".tmp")
    tmp.write_bytes(blob)
    os.replace(tmp, path)
    return str(path)


def _extract_progress(line):
    m = re.search(r"(\d+)%", line)
    return int(m.group(1)) if m else None


def _extract_expiry(captured):
    for line in reversed(captured):
        stripped = line.strip()
        if not stripped.startswith("{"):
            continue
        try:
            doc = json.loads(stripped)
        except json.JSONDecodeError:
            continue
        if doc.get("status") == "ok" and doc.get("profile_expiry_at"):
            return doc["profile_expiry_at"]
    return None


def run_intent(cp, job, cache_dir):
    job_id = job["id"]
    job_type = job.get("job_type", "")
    platform_name, udid, ip, port, params = _job_target(job)

    if job_type == "inventory":
        cp.update(job_id, "in_progress", progress=0)
        build_inventory(cp)
        cp.update(job_id, "completed", progress=100, result={"inventory": True})
        return

    if job_type not in ("install", "verify", "refresh", "uninstall"):
        cp.update(job_id, "failed", error_category="unsupported_job_type",
                  error_details=f"device service cannot execute {job_type} jobs")
        return

    # uninstall is tvOS-only today (tvos-uninstall.sh); iOS has no
    # uninstaller wrapper yet.
    if job_type == "uninstall":
        if platform_name != "tvos":
            cp.update(job_id, "failed", error_category="unsupported_job_type",
                      error_details="uninstall is not implemented for ios yet")
            return
        cp.update(job_id, "in_progress", progress=0)
        cmd, env = _wrapper_for(platform_name, TVOS_UNINSTALL_WRAPPER, "", udid, ip, port)
        rc, captured, duration, timed_out = _run_wrapper(cp, job_id, cmd, env)
        _report(cp, job_id, rc, captured, duration, timed_out)
        return

    log(f"job {job_id}: starting {job_type} ({platform_name} udid={udid or '?'})")
    cp.update(job_id, "in_progress", progress=0)

    try:
        ipa = _resolve_ipa(cp, job, cache_dir)
        if job_type in ("install", "verify") and ipa is None:
            cfg_ipa = params.get("ipa_path") or ""
            ipa = cfg_ipa or None
        cmd, env = _wrapper_for(platform_name,
                                IOS_INSTALL_WRAPPER if platform_name == "ios" else TVOS_INSTALL_WRAPPER,
                                ipa or "", udid, ip, port)
        rc, captured, duration, timed_out = _run_wrapper(cp, job_id, cmd, env)
    except Exception as exc:
        log(f"job {job_id}: execution failed: {exc}")
        cp.update(job_id, "failed", error_category="install_failed",
                  error_details=str(exc))
        return
    _report(cp, job_id, rc, captured, duration, timed_out)


def _run_wrapper(cp, job_id, cmd, env):
    """Run an install wrapper with a lease-keeping heartbeat and a hard
    per-job deadline, streaming progress back."""
    progress = {"value": 0}
    timed_out = {"value": False}
    started = time.time()
    stop = threading.Event()

    def heartbeat():
        while not stop.wait(HEARTBEAT_SECONDS):
            try:
                cp.update(job_id, "in_progress", progress=progress["value"])
            except Exception as exc:
                log(f"heartbeat failed: {exc}")
            if time.time() - started > JOB_TIMEOUT_SECONDS:
                timed_out["value"] = True
                log(f"job {job_id}: timeout after {JOB_TIMEOUT_SECONDS}s, killing wrapper")
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except (ProcessLookupError, PermissionError):
                    pass

    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True, start_new_session=True, env=env)
    thread = threading.Thread(target=heartbeat, daemon=True)
    thread.start()

    captured = []
    try:
        for line in proc.stdout:
            line = line.rstrip()
            if line:
                log(f"  {line}")
                captured.append(line)
                pct = _extract_progress(line)
                if pct is not None:
                    progress["value"] = pct
        rc = proc.wait()
    except Exception as exc:
        rc = 1
        captured.append(f"wrapper error: {exc}")
    finally:
        stop.set()
        thread.join(timeout=2)

    return rc, captured, int(time.time() - started), timed_out["value"]


def _report(cp, job_id, rc, captured, duration, timed_out):
    tail = "\n".join(captured[-25:])[-2000:]
    if timed_out:
        tail = (tail + f"\n[timed out after {JOB_TIMEOUT_SECONDS}s]").strip()[-2000:]
    if rc == 0:
        expiry = _extract_expiry(captured)
        log(f"job {job_id}: completed in {duration}s"
            + (f"; profile expiry {expiry}" if expiry else ""))
        cp.update(job_id, "completed", progress=100,
                  result={"verified": True, "duration_seconds": duration, "profile_expiry_at": expiry})
    else:
        log(f"job {job_id}: FAILED (rc={rc})")
        cp.update(job_id, "failed",
                  error_category="install_timeout" if timed_out else "install_failed",
                  error_details=tail or f"wrapper exited {rc}")


# --------------------------------------------------------------------------
# Daemon
# --------------------------------------------------------------------------

def _wait_for_control_plane(cp):
    while True:
        try:
            if cp.remote:
                cp.heartbeat()
            else:
                cp.me()
            return
        except Exception as exc:
            log(f"control plane not ready ({exc}); retrying in 10s...")
            time.sleep(10)


def daemon_loop(cp):
    _wait_for_control_plane(cp)
    build_inventory(cp)
    log(f"polling intents every {POLL_SECONDS}s")
    while True:
        try:
            cp.heartbeat()
            for job in cp.claim(limit=5):
                run_intent(cp, job, IPA_DIR)
        except Exception as exc:
            log(f"poll error: {exc}")
        time.sleep(POLL_SECONDS)


# --------------------------------------------------------------------------
# CLI verbs: `sidey-device.py <verb> [args]` one-shot, or daemon by default
# --------------------------------------------------------------------------

def verb_health(cp, _):
    print(cp.request("health", timeout=15))


def verb_inventory(cp, _):
    resolved = build_inventory(cp)
    print(json.dumps({"devices": resolved}, indent=2))


def verb_install(cp, args):
    """install <signed_artifact_id> [platform=ios|tvos] -- one-shot install."""
    if not args:
        raise SystemExit("usage: install <artifact_id> [--platform ios|tvos]")
    artifact_id = args[0]
    job = {"id": "manual", "job_type": "install",
           "parameters": {"artifact_id": artifact_id}}
    run_intent(cp, job, IPA_DIR)


def verb_tunnel(cp, _):
    udid = DEVICE_UDID or TVOS_UDID
    ip = DEVICE_IP or TVOS_IP
    if not (_ip := ip):
        raise SystemExit("no device IP configured (SIDEY_DEVICE_IP / SIDEY_TVOS_IP)")
    cmd = [VENV_PY, TUNNEL_SCRIPT, "--udid", udid or "", "--address",
           _ip, "--endpoint-file", "/run/sidey/rsd-endpoint"]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=900)
    print(proc.stdout, file=sys.stderr)
    if proc.returncode != 0:
        raise SystemExit(f"tunnel failed ({proc.returncode}): {proc.stderr}")


def main():
    cp = ControlPlane()
    args = sys.argv[1:]
    verbs = {
        "health": verb_health,
        "inventory": verb_inventory,
        "install": verb_install,
        "tunnel": verb_tunnel,
    }
    if args and args[0] in verbs:
        verbs[args[0]](cp, args[1:])
        return
    if args:
        raise SystemExit(f"unknown verb: {args[0]}")
    daemon_loop(cp)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
