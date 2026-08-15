//! tvos-cli: drive the TVOSProvider against a live Apple TV via the Go
//! sidey-tvos-helper (the transport-spike analog for the tvOS provider).

use std::process::ExitCode;

use clap::{Parser, Subcommand};
use device_provider::DeviceProvider;
use tvos_provider::{DeviceSpec, TvosProvider};

#[derive(Parser)]
#[command(name = "tvos-cli", about = "Sidey tvOS provider proof (Phase G)")]
struct Cli {
    /// Path to the sidey-tvos-helper executable
    #[arg(long, default_value = "/usr/local/bin/sidey-tvos-helper")]
    helper: String,

    /// Data root holding per-device dirs
    #[arg(long, default_value = "/var/lib/sidey/tvagent")]
    data_root: String,

    /// Device UDID
    #[arg(long)]
    udid: String,

    /// Device name
    #[arg(long, default_value = "Apple TV")]
    name: String,

    /// Device IP
    #[arg(long)]
    ip: String,

    /// Device port
    #[arg(long, default_value = "49152")]
    port: u16,

    /// RemotePairing record identifier (defaults to --udid)
    #[arg(long)]
    identifier: Option<String>,

    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Ping the device (discover)
    Discover,
    /// Read device info
    Info,
    /// List installed apps (inventory)
    Inventory,
    /// Inspect an IPA's provisioning profile
    Ipa { ipa_path: String },
    /// Sign + install + verify an IPA
    Install {
        ipa_path: String,
        #[arg(long, default_value = "")]
        account: String,
    },
    /// Remove an app record by bundle id
    Remove { bundle_id: String },
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    let spec = DeviceSpec {
        udid: cli.udid,
        name: cli.name,
        ip: cli.ip,
        port: cli.port,
        identifier: cli.identifier,
    };
    let mut p = match TvosProvider::launch(&cli.helper, &spec, &cli.data_root) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("launch: {e}");
            return ExitCode::FAILURE;
        }
    };

    let r: device_provider::Result<String> = match cli.cmd {
        Cmd::Discover => p.discover().map(|s| format!("state={}", s)),
        Cmd::Info => p.info().map(|i| serde_json::to_string_pretty(&i).unwrap()),
        Cmd::Inventory => p
            .installed_apps()
            .map(|apps| serde_json::to_string_pretty(&apps).unwrap()),
        Cmd::Ipa { ipa_path } => p
            .inspect_ipa(&ipa_path)
            .map(|prof| serde_json::to_string_pretty(&prof).unwrap()),
        Cmd::Install { ipa_path, account } => {
            if account.is_empty() {
                eprintln!("--account required (or SIDEY_APPLE_ID on the helper host)");
                return ExitCode::FAILURE;
            }
            let req = device_provider::InstallRequest {
                device_udid: spec.udid.clone(),
                account,
                ipa_path,
                custom_name: None,
                remove_extensions: true,
            };
            p.install_app(&req)
                .map(|o| serde_json::to_string_pretty(&o).unwrap())
        }
        Cmd::Remove { bundle_id } => p
            .remove_app(&bundle_id)
            .map(|_| format!("removed {bundle_id}")),
    };

    match r {
        Ok(out) => {
            println!("{out}");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {e}");
            ExitCode::FAILURE
        }
    }
}
