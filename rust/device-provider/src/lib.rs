//! Sidey device provider boundary (ADR-0005).
//!
//! `DeviceProvider` is the only interface the device agent core knows.
//! Implementations: `TVOSProvider` (Go helper wrapper), `IOSProvider` (native
//! Rust, Phase H).
//!
//! All methods are blocking; the agent core wraps calls in `spawn_blocking`.

use std::fmt;

pub type Result<T> = std::result::Result<T, Error>;

/// Connection capability state of a device, distinct from install failures
/// (ADR-0005). Lets the scheduler and dashboard distinguish pairing, signing
/// and installation issues.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConnectionState {
    Connected,
    PairedButUnreachable,
    PairingInvalid,
    DeviceLocked,
    DeveloperModeRequired,
    Offline,
}

impl fmt::Display for ConnectionState {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = match self {
            ConnectionState::Connected => "connected",
            ConnectionState::PairedButUnreachable => "paired_but_unreachable",
            ConnectionState::PairingInvalid => "pairing_invalid",
            ConnectionState::DeviceLocked => "device_locked",
            ConnectionState::DeveloperModeRequired => "developer_mode_required",
            ConnectionState::Offline => "offline",
        };
        f.write_str(s)
    }
}

/// Failure stage of a provider operation, so the control plane can tell
/// signing errors from installation errors (agent claims, jobs, device
/// reports carry this).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    Offline,
    Unreachable,
    PairingFailed,
    SigningFailed,
    ProvisioningFailed,
    InstallationFailed,
    DeploymentFailed,
    Unsupported,
    Protocol,
    InvalidArgument,
}

/// Provider error carrying the stage; error messages are opaque to the core.
#[derive(Debug, Clone)]
pub struct Error {
    pub kind: ErrorKind,
    pub message: String,
}

impl Error {
    pub fn new(kind: ErrorKind, message: impl Into<String>) -> Self {
        Error { kind, message: message.into() }
    }

    pub fn kind(&self) -> ErrorKind {
        self.kind
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:?}: {}", self.kind, self.message)
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::new(ErrorKind::Protocol, e.to_string())
    }
}

/// Stable device identity reported to the control plane.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct DeviceInfo {
    pub udid: String,
    pub platform: String,
    pub os_version: String,
    pub model: String,
    pub ip: String,
    pub name: String,
}

/// An installed application record (central inventory, devices/<udid>/apps).
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct InstalledApp {
    pub bundle_id: String,
    pub name: String,
    pub version: String,
    pub install_date: Option<String>,
}

/// Signed provisioning profile attached to an installed bundle.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct ProfileInfo {
    pub bundle_ids: Vec<String>,
    pub expiry: Option<String>,
    /// Embedded udids the profile is valid for (empty = universal).
    pub allowed_devices: Vec<String>,
    pub team_id: Option<String>,
    pub app_id: Option<String>,
}

/// Outcome of `install`, used for post-installation verification.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct InstallOutcome {
    pub bundle_id: String,
    pub installed_version: String,
    pub verified: bool,
    pub profile: Option<ProfileInfo>,
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct InstallRequest {
    pub device_udid: String,
    /// Account used to sign the build (must be enrolled with the provider).
    pub account: String,
    pub ipa_path: String,
    pub custom_name: Option<String>,
    pub remove_extensions: bool,
}

/// One document in an app's sandbox containers (House Arrest browse).
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct SandboxEntry {
    pub path: String,
    pub is_dir: bool,
    pub size: u64,
}

/// The one trait every provider must implement (ADR-0005).
///
/// Concurrency: implementations must be safe to call from a single agent
/// task at a time; the core serialises per-device work.
pub trait DeviceProvider {
    /// Capability name reported to the control plane ("tvos", "ios").
    fn capability(&self) -> &'static str;

    /// Probe the device and report the connection capability state.
    fn discover(&mut self) -> Result<ConnectionState>;

    /// Validate pairing and read the device identity.
    fn info(&mut self) -> Result<DeviceInfo>;

    /// Central inventory: apps installed on the device.
    fn installed_apps(&mut self) -> Result<Vec<InstalledApp>>;

    /// Provisioning profile embedded in a local IPA (before install).
    fn inspect_ipa(&mut self, ipa_path: &str) -> Result<ProfileInfo>;

    /// Stage the IPA with the signing account, install (or upgrade) and
    /// verify the bundle is present afterwards.
    fn install_app(&mut self, req: &InstallRequest) -> Result<InstallOutcome>;

    /// Uninstall an application by bundle id.
    fn remove_app(&mut self, bundle_id: &str) -> Result<()>;

    /// House arrest access: browse a vector path inside the app's sandbox.
    fn sandbox_list(&self, bundle_id: &str, path: &str) -> Result<Vec<SandboxEntry>>;

    /// Tail system log lines (Apple TV). Returns a limited recent slice.
    fn system_log(&self, lines: usize) -> Result<Vec<String>>;
}