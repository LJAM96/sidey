use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use idevice::provider::{IdeviceProvider, UsbmuxdProvider};
use idevice::services::afc::AfcClient;
use idevice::services::afc::opcode::AfcFopenMode;
use idevice::services::house_arrest::HouseArrestClient;
use idevice::services::installation_proxy::InstallationProxyClient;
use idevice::services::lockdown::LockdownClient;
use idevice::services::misagent::MisagentClient;
use idevice::usbmuxd::{UsbmuxdAddr, UsbmuxdConnection, UsbmuxdDevice};
use idevice::IdeviceService;

const LABEL: &str = "sidey-transport-spike";
const LOCKDOWN_PORT: u16 = 62078;
const STAGING_DIR: &str = "PublicStaging";

#[derive(Parser)]
#[command(name = "transport-spike", version, about = "Sidey Phase B transport spike")]
struct Cli {
    #[arg(global = true, long, help = "Device UDID (default: first connected device)")]
    udid: Option<String>,
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// List devices visible to usbmuxd
    List,
    /// Read device information from lockdown
    Info,
    /// List installed user applications
    Apps,
    /// Pair the host with the device (USB path) and save the record to usbmuxd
    Pair,
    /// Validate that a pairing record exists and a session can be started
    Validate,
    /// Push an IPA to the device staging area and install it
    Install {
        #[arg(long)]
        ipa: PathBuf,
    },
    /// Push an IPA to the device staging area and upgrade the installed app
    Upgrade {
        #[arg(long)]
        ipa: PathBuf,
    },
    /// Uninstall an application by bundle identifier
    Uninstall {
        #[arg(long)]
        bundle_id: String,
    },
    /// Verify the installed version and provisioning profile expiry
    Verify {
        #[arg(long)]
        bundle_id: String,
    },
    /// Access an app Documents directory via House Arrest
    Documents {
        #[arg(long)]
        bundle_id: String,
        #[arg(long, help = "Path inside Documents (default: root)")]
        path: Option<String>,
    },
    /// Restart the device (diagnostics relay)
    Restart,
    /// Put the device display to sleep (locks it if passcode/Face ID is set)
    Sleep,
    /// Wait for the device to reappear on usbmuxd (after restart etc.)
    Wait {
        #[arg(long, default_value_t = 300, help = "seconds to wait")]
        timeout: u64,
    },
}

#[tokio::main]
async fn main() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "transport_spike=info,idevice=warn".into()),
        )
        .init();

    let cli = Cli::parse();
    if let Err(e) = run(cli).await {
        tracing::error!("{e:#}");
        std::process::exit(1);
    }
}

async fn run(cli: Cli) -> Result<()> {
    match cli.command {
        Command::List => cmd_list().await,
        Command::Info => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_info(&dev, &provider).await
        }
        Command::Apps => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_apps(&dev, &provider).await
        }
        Command::Pair => cmd_pair(cli.udid.as_deref()).await,
        Command::Validate => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_validate(&dev, &provider).await
        }
        Command::Install { ipa } => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_install_or_upgrade(&dev, &provider, &ipa, true).await
        }
        Command::Upgrade { ipa } => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_install_or_upgrade(&dev, &provider, &ipa, false).await
        }
        Command::Uninstall { bundle_id } => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_uninstall(&dev, &provider, &bundle_id).await
        }
        Command::Verify { bundle_id } => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_verify(&dev, &provider, &bundle_id).await
        }
        Command::Documents { bundle_id, path } => {
            let (dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_documents(&dev, &provider, &bundle_id, path.as_deref()).await
        }
        Command::Restart => {
            let (_dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_restart(&provider).await
        }
        Command::Sleep => {
            let (_dev, provider) = select_provider(cli.udid.as_deref()).await?;
            cmd_sleep(&provider).await
        }
        Command::Wait { timeout } => {
            cmd_wait(cli.udid.as_deref(), timeout).await
        }
    }
}

fn usbmuxd_addr() -> UsbmuxdAddr {
    UsbmuxdAddr::from_env_var().unwrap_or_default()
}

async fn select_device(udid: Option<&str>) -> Result<UsbmuxdDevice> {
    let mut usbmuxd = UsbmuxdConnection::default()
        .await
        .context("unable to connect to usbmuxd (is usbmuxd running?)")?;
    let devs = usbmuxd
        .get_devices()
        .await
        .context("failed to list devices")?;
    if devs.is_empty() {
        bail!("no devices connected");
    }
    match udid {
        Some(u) => devs
            .into_iter()
            .find(|d| d.udid == u)
            .with_context(|| format!("device {u} not found")),
        None => Ok(devs.into_iter().next().expect("checked non-empty")),
    }
}

async fn select_provider(udid: Option<&str>) -> Result<(UsbmuxdDevice, UsbmuxdProvider)> {
    let dev = select_device(udid).await?;
    tracing::info!("device: udid={} connection={:?}", dev.udid, dev.connection_type);
    let provider = dev.to_provider(usbmuxd_addr(), LABEL);
    Ok((dev, provider))
}

/// Raw lockdown connection without a session (required for pairing).
async fn raw_lockdown(dev: &UsbmuxdDevice) -> Result<LockdownClient> {
    let provider = dev.to_provider(usbmuxd_addr(), LABEL);
    let idevice = provider
        .connect(LOCKDOWN_PORT)
        .await
        .context("failed to open raw lockdown connection")?;
    Ok(LockdownClient::new(idevice))
}

async fn cmd_list() -> Result<()> {
    let mut usbmuxd = UsbmuxdConnection::default().await?;
    let devs = usbmuxd.get_devices().await?;
    if devs.is_empty() {
        println!("no devices connected");
        return Ok(());
    }
    for d in &devs {
        println!("udid={} connection={:?}", d.udid, d.connection_type);
    }
    Ok(())
}

fn plist_str(v: &plist::Value) -> String {
    v.as_string().map(|s| s.to_string()).unwrap_or_else(|| format!("{v:?}"))
}

async fn cmd_info(_dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider) -> Result<()> {
    let mut lockdown = LockdownClient::connect(provider)
        .await
        .context("failed to connect to lockdown")?;
    for key in ["DeviceName", "ProductType", "ProductVersion", "BuildVersion", "UniqueDeviceID", "WiFiAddress", "DeviceClass"] {
        match lockdown.get_value(Some(key), None).await {
            Ok(v) => println!("{key}={}", plist_str(&v)),
            Err(e) => tracing::warn!("{key} unavailable: {e}"),
        }
    }
    Ok(())
}

async fn cmd_apps(_dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider) -> Result<()> {
    let mut client = InstallationProxyClient::connect(provider)
        .await
        .context("failed to connect to installation proxy")?;
    let apps = client
        .get_apps(Some("User"), None)
        .await
        .context("failed to list installed applications")?;
    println!("installed_apps={}", apps.len());
    let mut sorted: Vec<_> = apps.into_iter().collect();
    sorted.sort_by(|a, b| a.0.cmp(&b.0));
    for (bundle_id, info) in sorted {
        let d = info.as_dictionary();
        let ver = d.and_then(|d| d.get("CFBundleShortVersionString")).and_then(|v| v.as_string()).unwrap_or("?");
        let build = d.and_then(|d| d.get("CFBundleVersion")).and_then(|v| v.as_string()).unwrap_or("?");
        println!("{bundle_id} version={ver} build={build}");
    }
    Ok(())
}

async fn cmd_pair(udid: Option<&str>) -> Result<()> {
    let dev = select_device(udid).await?;
    tracing::info!("pairing device udid={}", dev.udid);

    let mut usbmuxd = UsbmuxdConnection::default().await?;
    let system_buid = usbmuxd
        .get_buid()
        .await
        .context("failed to read BUID from usbmuxd")?;
    let host_id = uuid::Uuid::new_v4().to_string();
    let host_name = std::env::var("HOSTNAME").ok().or_else(|| Some("sidey".into()));

    let mut lockdown = raw_lockdown(&dev).await?;
    let record = lockdown
        .pair(&host_id, &system_buid, host_name.as_deref())
        .await
        .context("pairing failed (accept the trust dialog on the device)")?;

    let bytes = record
        .serialize()
        .context("failed to serialize pairing record")?;
    usbmuxd
        .save_pair_record(&dev.udid, bytes)
        .await
        .context("failed to save pairing record to usbmuxd")?;
    println!("paired udid={} host_id={host_id}", dev.udid);
    Ok(())
}

async fn cmd_validate(dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider) -> Result<()> {
    let mut usbmuxd = UsbmuxdConnection::default().await?;
    match usbmuxd.get_pair_record(&dev.udid).await {
        Ok(_) => println!("pair_record=present"),
        Err(e) => {
            println!("pair_record=missing error={e}");
            return Ok(());
        }
    }
    let mut lockdown = LockdownClient::connect(provider)
        .await
        .context("failed to start session (pairing record invalid?)")?;
    match lockdown.get_value(Some("ProductVersion"), None).await {
        Ok(v) => println!("session=ok product_version={}", plist_str(&v)),
        Err(e) => println!("session=ok product_version=unavailable error={e}"),
    }
    Ok(())
}

async fn cmd_restart(provider: &dyn IdeviceProvider) -> Result<()> {
    use idevice::services::diagnostics_relay::DiagnosticsRelayClient;
    let mut diag = DiagnosticsRelayClient::connect(provider)
        .await
        .context("failed to connect to diagnostics relay")?;
    diag.restart()
        .await
        .context("restart request failed (device may drop the link mid-request)")?;
    println!("restart=requested");
    Ok(())
}

async fn cmd_sleep(provider: &dyn IdeviceProvider) -> Result<()> {
    use idevice::services::diagnostics_relay::DiagnosticsRelayClient;
    let mut diag = DiagnosticsRelayClient::connect(provider)
        .await
        .context("failed to connect to diagnostics relay")?;
    diag.sleep()
        .await
        .context("sleep request failed")?;
    println!("sleep=requested");
    Ok(())
}

async fn cmd_wait(udid: Option<&str>, timeout: u64) -> Result<()> {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(timeout);
    let mut interval_ms = 2000u64;
    loop {
        // re-list from scratch each time; usbmuxd forgets the device while it
        // is rebooting, so a stale UsbmuxdConnection would never see it again
        match select_device(udid).await {
            Ok(dev) => {
                // the device can appear before lockdown is ready; a quick value
                // read forces lockdown to answer on the fresh connection
                let provider = dev.to_provider(usbmuxd_addr(), LABEL);
                match LockdownClient::connect(&provider).await {
                    Ok(mut l) => match l.get_value(Some("ProductVersion"), None).await {
                        Ok(v) => {
                            println!("wait=ready udid={} product_version={}",
                                     dev.udid, plist_str(&v));
                            return Ok(());
                        }
                        Err(e) => tracing::debug!("lockdown not ready yet: {e}"),
                    },
                    Err(e) => tracing::debug!("lockdown not ready yet: {e}"),
                }
            }
            Err(_) => tracing::debug!("device {udid:?} not seen yet"),
        }
        if std::time::Instant::now() >= deadline {
            bail!("device did not become ready within {timeout}s");
        }
        std::thread::sleep(std::time::Duration::from_secs(interval_ms));
        interval_ms = (interval_ms as f64 * 1.5).min(10000.0) as u64;
    }
}

async fn cmd_install_or_upgrade(
    _dev: &UsbmuxdDevice,
    provider: &dyn IdeviceProvider,
    ipa: &Path,
    install: bool,
) -> Result<()> {
    if !ipa.exists() {
        bail!("ipa file not found: {}", ipa.display());
    }
    let staging_path = push_ipa(provider, ipa).await?;
    let mut client = InstallationProxyClient::connect(provider)
        .await
        .context("failed to connect to installation proxy")?;
    tracing::info!("{} {staging_path} ...", if install { "installing" } else { "upgrading" });
    let result = if install {
        client
            .install_with_callback(staging_path.clone(), None, |(pct, _)| async move {
                if pct % 10 == 0 {
                    tracing::info!("install progress={pct}");
                }
            }, ())
            .await
    } else {
        client
            .upgrade_with_callback(staging_path.clone(), None, |(pct, _)| async move {
                if pct % 10 == 0 {
                    tracing::info!("upgrade progress={pct}");
                }
            }, ())
            .await
    };
    result.with_context(|| format!("installation proxy {}", if install { "install" } else { "upgrade" }))?;
    drop(client);
    cleanup_staging(provider, &staging_path).await;
    println!("operation={} result=ok", if install { "install" } else { "upgrade" });
    Ok(())
}

async fn push_ipa(provider: &dyn IdeviceProvider, ipa: &Path) -> Result<String> {
    let file_name = ipa
        .file_name()
        .context("ipa path has no file name")?
        .to_string_lossy()
        .into_owned();
    let data = tokio::fs::read(ipa)
        .await
        .with_context(|| format!("failed to read {}", ipa.display()))?;
    let staging = format!("{STAGING_DIR}/{file_name}");

    let mut afc = AfcClient::connect(provider)
        .await
        .context("failed to connect to AFC")?;
    if let Err(e) = afc.mk_dir(STAGING_DIR).await {
        tracing::debug!("staging dir exists or failed: {e}");
    }
    let mut file = afc
        .open_owned(staging.clone(), AfcFopenMode::WrOnly)
        .await
        .with_context(|| format!("failed to open {staging} on device"))?;
    for chunk in data.chunks(512 * 1024) {
        file.write_entire(chunk)
            .await
            .context("failed to write IPA to device")?;
    }
    let _afc = file.close().await.context("failed to close device file")?;
    tracing::info!("staged {} bytes to {staging}", data.len());
    Ok(staging)
}

async fn cleanup_staging(provider: &dyn IdeviceProvider, staging_path: &str) {
    let mut afc = match AfcClient::connect(provider).await {
        Ok(a) => a,
        Err(_) => return,
    };
    let _ = afc.remove(staging_path).await;
}

async fn cmd_uninstall(_dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider, bundle_id: &str) -> Result<()> {
    let mut client = InstallationProxyClient::connect(provider)
        .await
        .context("failed to connect to installation proxy")?;
    client
        .uninstall(bundle_id.to_string(), None)
        .await
        .with_context(|| format!("uninstall {bundle_id} failed"))?;
    println!("uninstall bundle_id={bundle_id} result=ok");
    Ok(())
}

async fn cmd_verify(_dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider, bundle_id: &str) -> Result<()> {
    let mut client = InstallationProxyClient::connect(provider)
        .await
        .context("failed to connect to installation proxy")?;
    let apps = client.get_apps(None, Some(vec![bundle_id.to_string()])).await?;
    match apps.get(bundle_id) {
        Some(info) => {
            let d = info.as_dictionary();
            let ver = d.and_then(|d| d.get("CFBundleShortVersionString")).and_then(|v| v.as_string()).unwrap_or("?");
            let build = d.and_then(|d| d.get("CFBundleVersion")).and_then(|v| v.as_string()).unwrap_or("?");
            let path = d.and_then(|d| d.get("Path")).and_then(|v| v.as_string()).unwrap_or("?");
            let min_os = d.and_then(|d| d.get("MinimumOSVersion")).and_then(|v| v.as_string()).unwrap_or("?");
            println!("installed bundle_id={bundle_id} version={ver} build={build} path={path} min_os={min_os}");
        }
        None => {
            println!("installed bundle_id={bundle_id} present=false");
        }
    }
    drop(client);
    verify_profiles(provider, bundle_id).await;
    Ok(())
}

async fn verify_profiles(provider: &dyn IdeviceProvider, bundle_id: &str) {
    let mut client = match MisagentClient::connect(provider).await {
        Ok(c) => c,
        Err(e) => {
            tracing::warn!("misagent unavailable: {e}");
            return;
        }
    };
    let profiles = match client.copy_all().await {
        Ok(p) => p,
        Err(e) => {
            tracing::warn!("failed to copy provisioning profiles: {e}");
            return;
        }
    };
    println!("provisioning_profiles={}", profiles.len());
    for p in &profiles {
        if let Some((uuid, name, expiry, app_ids)) = parse_profile(p) {
            let matches = app_ids.iter().any(|id| id.contains(bundle_id));
            println!(
                "profile uuid={uuid} name={name} expiry={expiry} matches_bundle={matches} app_ids={}",
                app_ids.join(",")
            );
        } else {
            println!("profile uuid=? name=? expiry=? (unparseable, {} bytes)", p.len());
        }
    }
}

/// Best effort parse of a .mobileprovision: extracts the embedded XML plist and
/// returns (UUID, Name, ExpirationDate, application-identifier list).
fn parse_profile(data: &[u8]) -> Option<(String, String, String, Vec<String>)> {
    let start = data
        .windows(5)
        .position(|w| w == b"<?xml")?;
    let end = data
        .windows(8)
        .enumerate()
        .skip(start)
        .find_map(|(i, w)| (w == b"</plist>").then_some(i))?;
    let plist_bytes = &data[start..end + 8];
    let val: plist::Value = plist::from_bytes(plist_bytes).ok()?;
    let d = val.as_dictionary()?;
    let uuid = d.get("UUID").and_then(|v| v.as_string()).unwrap_or("?").to_string();
    let name = d.get("Name").and_then(|v| v.as_string()).unwrap_or("?").to_string();
    let expiry = d
        .get("ExpirationDate")
        .and_then(|v| v.as_date())
        .map(|dt| dt.to_xml_format())
        .unwrap_or_else(|| "?".into());
    let app_ids = d
        .get("Entitlements")
        .and_then(|v| v.as_dictionary())
        .and_then(|e| e.get("application-identifier"))
        .and_then(|v| v.as_string())
        .map(|s| vec![s.to_string()])
        .unwrap_or_default();
    Some((uuid, name, expiry, app_ids))
}

async fn cmd_documents(_dev: &UsbmuxdDevice, provider: &dyn IdeviceProvider, bundle_id: &str, path: Option<&str>) -> Result<()> {
    let house = HouseArrestClient::connect(provider)
        .await
        .context("failed to connect to house arrest")?;
    let mut afc = house
        .vend_documents(bundle_id.to_string())
        .await
        .with_context(|| format!("house arrest refused access to {bundle_id} (is it a developer-signed app?)"))?;
    let dir = path.unwrap_or(".");
    let entries = afc
        .list_dir(dir)
        .await
        .with_context(|| format!("failed to list {dir}"))?;
    println!("documents bundle_id={bundle_id} path={dir} entries={}", entries.len());
    for e in entries {
        println!("  {e}");
    }
    Ok(())
}
