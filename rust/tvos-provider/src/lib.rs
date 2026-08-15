//! TVOSProvider (ADR-0005): `DeviceProvider` implemented over the Go
//! `sidey-tvos-helper` subprocess JSON protocol.
//!
//! One helper process per device, data dir `<data_root>/devices/<udid>`,
//! requests on stdin as newline-delimited JSON:
//!
//! ```json
//! {"id":"1","op":"deploy","data":{...}}
//! {"id":"1","op":"deploy","ok":true,"result":{...}}
//! ```
//!
//! The helper is single-threaded per device; `DeviceProvider` calls must be
//! serialised (the agent core does this per-device).

use std::io::{BufRead, BufReader, BufWriter, Write};
use std::path::PathBuf;
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};

use device_provider::{
    ConnectionState, DeviceInfo, DeviceProvider, Error, ErrorKind, InstallOutcome, InstallRequest,
    InstalledApp, ProfileInfo, Result, SandboxEntry,
};
use serde_json::{json, Value};

/// Device endpoint metadata from the control plane registration (ADR-0004).
#[derive(Debug, Clone)]
pub struct DeviceSpec {
    pub udid: String,
    pub name: String,
    pub ip: String,
    pub port: u16,
    /// RemotePairing record identifier. Differs from the UDID on the Apple
    /// TV; when empty the helper falls back to the UDID.
    pub identifier: Option<String>,
}

/// `DeviceProvider` over the sidey-tvos-helper JSON protocol.
pub struct TvosProvider {
    spec: DeviceSpec,
    data_dir: PathBuf,
    child: Child,
    stdin: BufWriter<ChildStdin>,
    stdout: BufReader<ChildStdout>,
    counter: u64,
}

/// Raw helper response envelope (ok, result, error, error_stage).
#[derive(Debug, serde::Deserialize)]
pub struct Response {
    #[serde(default)]
    id: String,
    #[serde(default)]
    ok: bool,
    #[serde(default)]
    result: Value,
    #[serde(default)]
    error: String,
    #[serde(default)]
    error_stage: String,
}

impl TvosProvider {
    /// Spawn the helper for one device. `helper_bin` is the
    /// `sidey-tvos-helper` executable; `data_root` holds per-device dirs.
    pub fn launch(helper_bin: &str, spec: &DeviceSpec, data_root: &str) -> Result<Self> {
        let data_dir = PathBuf::from(data_root).join("devices").join(&spec.udid);
        let mut child = Command::new(helper_bin)
            .env("SIDEY_TVOS_DATA_DIR", &data_dir)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| Error::new(ErrorKind::Protocol, format!("spawn {}: {}", helper_bin, e)))?;
        let stdin = BufWriter::new(
            child
                .stdin
                .take()
                .ok_or_else(|| Error::new(ErrorKind::Protocol, "helper stdin closed on spawn"))?,
        );
        let stdout = BufReader::new(
            child
                .stdout
                .take()
                .ok_or_else(|| Error::new(ErrorKind::Protocol, "helper stdout closed on spawn"))?,
        );
        Ok(TvosProvider {
            spec: spec.clone(),
            data_dir,
            child,
            stdin,
            stdout,
            counter: 0,
        })
    }

    /// Send one request, read back the matching response line.
    pub fn request(&mut self, op: &str, data: Value) -> Result<Response> {
        self.counter += 1;
        let id = self.counter.to_string();
        let line = json!({ "id": id, "op": op, "data": data }).to_string();
        self.stdin.write_all(line.as_bytes()).map_err(Error::from)?;
        self.stdin.write_all(b"\n").map_err(Error::from)?;
        self.stdin.flush().map_err(Error::from)?;

        let mut buf = String::new();
        loop {
            buf.clear();
            let n = self
                .stdout
                .read_line(&mut buf)
                .map_err(|_| Error::new(ErrorKind::Offline, "helper stdout read failed"))?;
            if n == 0 {
                return Err(Error::new(ErrorKind::Offline, "helper exited"));
            }
            let line = buf.trim();
            if line.is_empty() || !line.starts_with('{') {
                continue;
            }
            let resp: Response = match serde_json::from_str(line) {
                Ok(r) => r,
                Err(_) => continue, // log noise on stdout, skip
            };
            if !resp.id.is_empty() && resp.id != id {
                continue;
            }
            if !resp.ok {
                return Err(Error::new(
                    kind_for_stage(&resp.error_stage),
                    format!("{}: {}", resp.error_stage, resp.error),
                ));
            }
            return Ok(resp);
        }
    }

    /// The per-device data dir on the host.
    pub fn data_dir(&self) -> &PathBuf {
        &self.data_dir
    }
}

impl Drop for TvosProvider {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl DeviceProvider for TvosProvider {
    fn capability(&self) -> &'static str {
        "tvos"
    }

    fn discover(&mut self) -> Result<ConnectionState> {
        match self.request("ping", json!({})) {
            Ok(_) => Ok(ConnectionState::Connected),
            Err(_) => Ok(ConnectionState::Offline),
        }
    }

    fn info(&mut self) -> Result<DeviceInfo> {
        let resp = self.request("device_info", json!({ "udid": self.spec.udid }))?;
        Ok(DeviceInfo {
            udid: str_of(&resp.result, "udid").unwrap_or_else(|| self.spec.udid.clone()),
            platform: "tvos".into(),
            os_version: str_of(&resp.result, "product_version").unwrap_or_default(),
            model: str_of(&resp.result, "product_type").unwrap_or_default(),
            ip: str_of(&resp.result, "ip").unwrap_or_else(|| self.spec.ip.clone()),
            name: str_of(&resp.result, "name").unwrap_or_else(|| self.spec.name.clone()),
        })
    }

    fn installed_apps(&mut self) -> Result<Vec<InstalledApp>> {
        let resp = self.request("inventory", json!({}))?;
        let apps: Vec<InstalledApp> = resp
            .result
            .get("apps")
            .cloned()
            .and_then(|a| serde_json::from_value(a).ok())
            .unwrap_or_default();
        Ok(apps)
    }

    fn inspect_ipa(&mut self, ipa_path: &str) -> Result<ProfileInfo> {
        let resp = self.request("ipa", json!({ "ipa_path": ipa_path }))?;
        let bundle_ids = match str_of(&resp.result, "bundle_identifier") {
            Some(id) if !id.is_empty() => vec![id],
            _ => Vec::new(),
        };
        let allowed: Vec<String> = resp
            .result
            .get("allowed_devices")
            .and_then(|v| match v {
                Value::Array(a) => Some(
                    a.iter()
                        .filter_map(|x| x.as_str().map(String::from))
                        .collect(),
                ),
                _ => None,
            })
            .unwrap_or_default();
        Ok(ProfileInfo {
            bundle_ids,
            expiry: str_of(&resp.result, "expiration"),
            allowed_devices: allowed,
            team_id: str_of(&resp.result, "team_id"),
            app_id: str_of(&resp.result, "app_id"),
        })
    }

    fn install_app(&mut self, req: &InstallRequest) -> Result<InstallOutcome> {
        let resp = self.request(
            "deploy",
            json!({
                "udid": self.spec.udid,
                "identifier": self.spec.identifier,
                "name": self.spec.name,
                "ip": self.spec.ip,
                "port": self.spec.port,
                "ipa_path": req.ipa_path,
                "ipa_name": req.ipa_path.rsplit('/').next().unwrap_or("app.ipa"),
                "account": req.account,
                "custom_name": req.custom_name,
                "remove_extensions": req.remove_extensions,
                "refresh": false,
            }),
        )?;

        let bundle_id = str_of(&resp.result, "bundle_identifier").unwrap_or_default();
        let version = str_of(&resp.result, "version").unwrap_or_default();
        let installed = InstallOutcome {
            bundle_id: bundle_id.clone(),
            installed_version: version,
            verified: false,
            profile: None,
        };

        // Post-install verification: the record must exist with a valid
        // expiry and the device must answer an AFC probe (when reachable).
        let verify = self.request(
            "verify",
            json!({
                "udid": self.spec.udid,
                "ip": self.spec.ip,
                "port": self.spec.port,
                "bundle_identifier": bundle_id,
            }),
        );
        let outcome = match verify {
            Ok(v) => InstallOutcome {
                verified: v
                    .result
                    .get("device_available")
                    .and_then(|x| x.as_bool())
                    .unwrap_or(true),
                ..installed
            },
            Err(_) => installed,
        };
        Ok(outcome)
    }

    fn remove_app(&mut self, bundle_id: &str) -> Result<()> {
        self.request(
            "uninstall",
            json!({ "udid": self.spec.udid, "bundle_identifier": bundle_id }),
        )?;
        Ok(())
    }

    fn sandbox_list(&self, _bundle_id: &str, _path: &str) -> Result<Vec<SandboxEntry>> {
        Err(Error::new(
            ErrorKind::Unsupported,
            "house arrest access not exposed by the helper yet (ADR-0006)",
        ))
    }

    fn system_log(&self, _lines: usize) -> Result<Vec<String>> {
        Err(Error::new(
            ErrorKind::Unsupported,
            "system log collection not exposed by the helper yet",
        ))
    }
}

/// Map the helper's `error_stage` to the trait's failure kind.
fn kind_for_stage(stage: &str) -> ErrorKind {
    match stage {
        "signing" | "account" => ErrorKind::SigningFailed,
        "pairing" | "deployment" => ErrorKind::PairingFailed,
        "installation" => ErrorKind::InstallationFailed,
        _ => ErrorKind::Protocol,
    }
}

fn str_of(result: &Value, key: &str) -> Option<String> {
    result.get(key).and_then(|v| v.as_str()).map(String::from)
}
