//! Headless signing worker binary (Phase F).
//!
//! Signs an IPA for a specific device with the account's existing certificate
//! identity (reused by machine name) and writes the signed IPA to disk,
//! printing a JSON document to stdout on completion or failure.
//!
//! Usage:
//!   signonly <apple_id> <input.ipa> <output.ipa>
//!
//! Env:
//!   SIDEY_ISIDELOAD_STATE  isideload storage dir (cert identity, anisette state)
//!   ANISETTE_URL           anisette v3 provider URL (default http://127.0.0.1:6970)
//!   DEVICE_UDID            target device UDID (registered with the team)
//!   DEVICE_NAME            target device display name
//!   DEVICE_TYPE            target platform: ios|tvos|watchos (default ios)
//!   MACHINE_NAME           certificate identity machine name (default isideload-minimal)
//!   SIGNONLY_2FA_CODE_FILE path to a file containing a 2FA code (default /tmp/opencode/2fa-code.txt)
//!
//! Credentials: the Apple ID is a positional argument; the password is read
//! from the SIDEY_APPLE_MAIN_PASSWORD environment variable. The password is
//! deliberately NOT accepted as a command-line argument, so it never appears
//! in /proc/PID/cmdline or process listings. The legacy
//! `signonly <apple_id> <password> <in.ipa> <out.ipa>` form is still accepted
//! with a warning for one-off use.
//!
//! Success output:
//!   {"status":"ok","signed_ipa_sha256":"...","bundle_identifier":"...",
//!    "signed_bundle_identifier":"...","version":"...","profile_expiry_at":"...",
//!    "cert_serial":"...","team_id":"...","device_count":N,"app_id_count":N}
//!
//! Failure output (exit code 1):
//!   {"status":"error","category":"auth|certificate|provisioning|entitlement|codesign|network|other","error":"..."}
use isideload::{
    anisette::remote_v3::RemoteV3AnisetteProvider,
    auth::apple_account::{AppleAccount, TwoFactorCallbackParams, TwoFactorCallbackResponse},
    dev::{
        app_ids::AppIdsApi,
        developer_session::DeveloperSession,
        devices::DevicesApi,
        teams::TeamsApi,
    },
    sideload::{SideloaderBuilder, TeamSelection, builder::MaxCertsBehavior},
    util::fs_storage::FsStorage,
};
use std::{
    env,
    io::Write,
    path::{Path, PathBuf},
};

use plist::Value;
use sha2::{Digest, Sha256};
use tracing::Level;
use tracing_subscriber::FmtSubscriber;

fn storage_dir() -> PathBuf {
    env::var("SIDEY_ISIDELOAD_STATE")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/var/lib/sidey/isideload"))
}

fn output_json(value: &serde_json::Value) {
    let mut stdout = std::io::stdout().lock();
    let _ = stdout.write_all(serde_json::to_string(value).unwrap().as_bytes());
    let _ = stdout.write_all(b"\n");
    let _ = stdout.flush();
}

fn fail(category: &str, message: &str) -> ! {
    output_json(&serde_json::json!({"status": "error", "category": category, "error": message}));
    std::process::exit(1);
}

/// Classify a report chain into a stable failure category so the control
/// plane can distinguish auth, certificate, provisioning, entitlement and
/// code signing failures.
fn classify(error: &rootcause::Report) -> (String, String) {
    let message = format!("{error:?}").replace('\n', " ");

    if let Some(dev_err) = error
        .iter_reports()
        .find_map(|node| node.downcast_current_context::<isideload::SideloadError>())
    {
        match dev_err {
            isideload::SideloadError::DeveloperError(code, _) if *code == 7460 => {
                return ("certificate".to_string(), message);
            }
            isideload::SideloadError::AuthWithMessage(_, _) => {
                return ("auth".to_string(), message);
            }
            isideload::SideloadError::AnisetteNotProvisioned => {
                return ("network".to_string(), message);
            }
            _ => {}
        }
    }

    let lower = message.to_lowercase();
    let category = if lower.contains("failed to retrieve certificate identity")
        || lower.contains("maximum number of certificates")
        || lower.contains("certificate") && lower.contains("quota")
        || lower.contains("csr")
    {
        "certificate"
    } else if lower.contains("sign in") || lower.contains("login") || lower.contains("two factor")
        || lower.contains("2fa") || lower.contains("verification code")
    {
        "auth"
    } else if lower.contains("provisioning profile") {
        "provisioning"
    } else if lower.contains("entitlement") {
        "entitlement"
    } else if lower.contains("failed to sign app") || lower.contains("codesign") {
        "codesign"
    } else if lower.contains("connect") || lower.contains("timeout") || lower.contains("dns")
        || lower.contains("connection refused") || lower.contains("tls")
    {
        "network"
    } else {
        "other"
    };
    (category.to_string(), message)
}

fn get_2fa_code(params: &TwoFactorCallbackParams) -> TwoFactorCallbackResponse {
    if params.unknown {
        eprintln!("The most recently attempted 2FA method failed, please try a different method.");
    } else if params.sms {
        eprintln!("SMS 2FA code requested; enter the code from your messages.");
    } else {
        eprintln!("2FA push sent to your devices. Codes expire quickly; prefer SMS: send 'p<id>' to request one.");
    }
    for n in &params.numbers {
        eprintln!("SMS number ID {}: {}", n.id, n.number_with_dial_code);
    }

    let code_file = env::var("SIGNONLY_2FA_CODE_FILE")
        .unwrap_or_else(|_| "/tmp/opencode/2fa-code.txt".to_string());
    let mut code = String::new();
    for _ in 0..600 {
        if let Ok(c) = std::fs::read_to_string(&code_file) {
            let c = c.trim().to_string();
            if !c.is_empty() {
                let _ = std::fs::remove_file(&code_file);
                code = c;
                break;
            }
        }
        std::thread::sleep(std::time::Duration::from_millis(1000));
    }
    if code.is_empty() {
        let _ = std::io::stdin().read_line(&mut code);
    }

    if code.trim().starts_with('p') {
        let selected_id = code.trim()[1..].parse::<u32>().unwrap();
        return TwoFactorCallbackResponse::SendSms(selected_id);
    }
    if code.trim() == "d" {
        return TwoFactorCallbackResponse::SendToDevices;
    }
    if code.trim() == "r" && !params.unknown {
        return TwoFactorCallbackResponse::ResendCode;
    }
    TwoFactorCallbackResponse::SubmitCode(code.trim().to_string())
}

/// Zip the signed .app bundle into an IPA with the canonical Payload layout.
fn zip_bundle(bundle_dir: &Path, output_ipa: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let app_name = bundle_dir
        .file_name()
        .ok_or("bundle dir has no file name")?
        .to_string_lossy()
        .to_string();
    let payload_prefix = format!("Payload/{app_name}");

    let out_file = std::fs::File::create(output_ipa)?;
    let mut writer = zip::ZipWriter::new(out_file);
    let options = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);

    fn walk(
        writer: &mut zip::ZipWriter<std::fs::File>,
        options: zip::write::SimpleFileOptions,
        base: &Path,
        dir: &Path,
        prefix: &str,
    ) -> Result<(), Box<dyn std::error::Error>> {
        let mut entries: Vec<_> = std::fs::read_dir(dir)?
            .filter_map(Result::ok)
            .collect();
        entries.sort_by_key(|e| e.file_name());
        for entry in entries {
            let path = entry.path();
            let rel = path.strip_prefix(base)?;
            let name = format!("{prefix}/{}", rel.to_string_lossy().replace('\\', "/"));
            if path.is_dir() {
                writer
                    .add_directory(name.clone() + "/", options)?;
                walk(writer, options, base, &path, prefix)?;
            } else {
                writer.start_file(name, options)?;
                let mut file = std::fs::File::open(&path)?;
                std::io::copy(&mut file, writer)?;
            }
        }
        Ok(())
    }

    walk(&mut writer, options, bundle_dir, bundle_dir, &payload_prefix)?;
    writer.finish()?;
    Ok(())
}

async fn sha256_file(path: &Path) -> Result<String, Box<dyn std::error::Error>> {
    let mut file = tokio::fs::File::open(path).await?;
    let mut hasher = Sha256::new();
    let mut buf = [0u8; 65536];
    loop {
        let n = tokio::io::AsyncReadExt::read(&mut file, &mut buf).await?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(hex::encode(hasher.finalize()))
}

fn info_plist_value(bundle_dir: &Path, key: &str) -> Option<String> {
    let path = bundle_dir.join("Info.plist");
    let data = std::fs::read(path).ok()?;
    let value: Value = plist::from_bytes(&data).ok()?;
    match value.as_dictionary()?.get(key) {
        Some(Value::String(s)) => Some(s.clone()),
        _ => None,
    }
}

#[tokio::main]
async fn main() {
    isideload::init().expect("Failed to initialize error reporting");
    let subscriber = FmtSubscriber::builder()
        .with_max_level(Level::INFO)
        .with_writer(std::io::stderr)
        .finish();
    tracing::subscriber::set_global_default(subscriber).expect("setting default subscriber failed");

    let args: Vec<String> = env::args().collect();
    let apple_id = match args.get(1) {
        Some(v) => v.clone(),
        None => fail("other", "missing Apple ID argument"),
    };
    // Password from env by default; accept the legacy 4-arg form (password on
    // the command line) only with a warning, so old callers keep working while
    // new callers never leak the password into argv.
    let input_ipa = match args.len() {
        5 => {
            eprintln!("WARNING: legacy signonly invocation leaked the Apple password onto the command line; use SIDEY_APPLE_MAIN_PASSWORD instead");
            PathBuf::from(args[3].clone())
        }
        4 => PathBuf::from(args[2].clone()),
        _ => fail("other", "usage: signonly <apple_id> <input.ipa> <output.ipa>"),
    };
    let output_ipa = match args.len() {
        5 => PathBuf::from(args[4].clone()),
        4 => PathBuf::from(args[3].clone()),
        _ => fail("other", "usage: signonly <apple_id> <input.ipa> <output.ipa>"),
    };
    let apple_password = match env::var("SIDEY_APPLE_MAIN_PASSWORD") {
        Ok(v) if !v.is_empty() => v,
        _ => {
            if args.len() == 5 {
                args[2].clone()
            } else {
                fail("other", "missing Apple password (set SIDEY_APPLE_MAIN_PASSWORD)")
            }
        }
    };

    let device_name = env::var("DEVICE_NAME").unwrap_or_else(|_| "Sidey signing target".to_string());
    let device_udid = env::var("DEVICE_UDID").unwrap_or_default();
    if device_udid.is_empty() {
        fail(
            "other",
            "missing DEVICE_UDID: refusing to sign without a target device (the installed app would not activate)",
        );
    }
    let machine_name = env::var("MACHINE_NAME").unwrap_or_else(|_| "isideload-minimal".to_string());
    // Optional Sidey self-hosted App Store source URL pre-seeded into
    // LiveContainer bundles during signing.
    let source_url = env::var("SIDEY_SOURCE_URL")
        .ok()
        .filter(|s| !s.trim().is_empty());
    let device_type = match env::var("DEVICE_TYPE").as_deref() {
        Ok("tvos") => Some(isideload::dev::device_type::DeveloperDeviceType::Tvos),
        Ok("watchos") => Some(isideload::dev::device_type::DeveloperDeviceType::Watchos),
        Ok("ios") | Ok("") | Err(_) => None,
        Ok(other) => fail("other", &format!("unknown DEVICE_TYPE {other:?} (ios|tvos|watchos)")),
    };

    let get_2fa_code = |params: TwoFactorCallbackParams| get_2fa_code(&params);

    let account = AppleAccount::builder(&apple_id)
        .anisette_provider(
            RemoteV3AnisetteProvider::default()
                .unwrap()
                .set_url(&env::var("ANISETTE_URL").unwrap_or_else(|_| "http://127.0.0.1:6970".to_string()))
                .set_serial_number("2".to_string())
                .set_storage(Box::new(FsStorage::new(storage_dir()))),
        )
        .login(&apple_password, Box::new(get_2fa_code))
        .await;
    let mut account = match account {
        Ok(a) => a,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };

    let mut dev_session = match DeveloperSession::from_account(&mut account).await {
        Ok(d) => d,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };

    let teams = match dev_session.list_teams().await {
        Ok(t) => t,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };
    let team = match teams.first() {
        Some(t) => t.clone(),
        None => fail("auth", "no developer teams available for account"),
    };

    if let Err(e) = dev_session
        .ensure_device_registered(&team, &device_name, &device_udid, device_type.clone())
        .await
    {
        let (category, message) = classify(&e);
        fail(&category, &message);
    }

    let team_selection_prompt = |teams: &Vec<isideload::dev::teams::DeveloperTeam>| {
        teams.first().map(|t| t.team_id.clone())
    };
    let cert_selection_prompt = |certs: &Vec<isideload::dev::certificates::DevelopmentCertificate>| {
        eprintln!("Maximum number of certificates reached; refusing to revoke automatically.");
        for (index, cert) in certs.iter().enumerate() {
            eprintln!(
                "({}) {}: {}",
                index + 1,
                cert.name.as_deref().unwrap_or("<Unnamed>"),
                cert.machine_name.as_deref().unwrap_or("<No Machine Name>"),
            );
        }
        None
    };

    let mut sideloader = SideloaderBuilder::new(dev_session, apple_id)
        .team_selection(TeamSelection::PromptOnce(team_selection_prompt))
        .max_certs_behavior(MaxCertsBehavior::Prompt(Box::new(cert_selection_prompt)))
        .storage(Box::new(FsStorage::new(storage_dir())))
        .machine_name(machine_name)
        .device_type(device_type.clone())
        .source_url(source_url)
        .build();

    let signed_app = match sideloader.sign_app(input_ipa.clone(), Some(team.clone()), true).await {
        Ok(s) => s,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };

    if let Err(e) = zip_bundle(&signed_app.bundle_dir, &output_ipa) {
        fail("other", &format!("failed to zip signed bundle: {e}"));
    }

    let sha256 = match sha256_file(&output_ipa).await {
        Ok(h) => h,
        Err(e) => fail("other", &format!("failed to hash output IPA: {e}")),
    };

    // Report team slot usage (D12) from the developer portal.
    let dev_session = sideloader.get_dev_session();
    let device_count = dev_session
        .list_devices(&team, device_type.clone())
        .await
        .map(|d| d.len())
        .unwrap_or(0);
    let app_id_count = dev_session
        .list_app_ids(&team, device_type)
        .await
        .map(|d| d.app_ids.len())
        .unwrap_or(0);

    let version = info_plist_value(&signed_app.bundle_dir, "CFBundleShortVersionString");
    let bundle_identifier = info_plist_value(&signed_app.bundle_dir, "CFBundleIdentifier");
    let profile_expiry_at = signed_app.profile_expiry.to_xml_format();

    output_json(&serde_json::json!({
        "status": "ok",
        "signed_ipa_sha256": sha256,
        "bundle_identifier": bundle_identifier,
        "signed_bundle_identifier": signed_app.signed_bundle_identifier,
        "version": version,
        "profile_expiry_at": profile_expiry_at,
        "cert_serial": signed_app.cert_serial,
        "team_id": signed_app.team_id,
        "device_count": device_count,
        "app_id_count": app_id_count,
    }));
}
