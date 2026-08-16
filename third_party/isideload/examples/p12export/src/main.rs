//! Certificate P12 export binary (Phase F).
//!
//! Exports the account's active development certificate and private key as a
//! PKCS#12 archive so it can be imported into LiveContainer / AltStore /
//! SideStore ("JIT-less" signing certificate).
//!
//! Usage:
//!   p12export <apple_id> <output.p12>
//!
//! Env:
//!   SIDEY_APPLE_MAIN_PASSWORD Apple account password (never on argv)
//!   SIDEY_ISIDELOAD_STATE     isideload storage dir (cert identity, anisette state)
//!   ANISETTE_URL              anisette v3 provider URL (default http://127.0.0.1:6970)
//!   MACHINE_NAME              certificate identity machine name (default isideload-minimal)
//!
//! The p12 is encrypted with the certificate's machine id as the password
//! (the same convention isideload uses when embedding ALTCertificate.p12).
//!
//! Success output:
//!   {"status":"ok","p12_sha256":"...","cert_serial":"...","team_id":"...","machine_name":"..."}
//!
//! Failure output (exit code 1):
//!   {"status":"error","category":"auth|certificate|other","error":"..."}
use isideload::{
    anisette::remote_v3::RemoteV3AnisetteProvider,
    auth::apple_account::{AppleAccount, TwoFactorCallbackParams, TwoFactorCallbackResponse},
    dev::{
        developer_session::DeveloperSession,
        teams::TeamsApi,
    },
    sideload::{
        builder::MaxCertsBehavior,
        cert_identity::CertificateIdentity,
    },
    util::fs_storage::FsStorage,
};
use std::{
    env,
    io::Write,
    path::PathBuf,
};

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

fn classify(error: &rootcause::Report) -> (String, String) {
    let message = format!("{error:?}").replace('\n', " ");
    if let Some(isideload::SideloadError::AuthWithMessage(_, _)) = error
        .iter_reports()
        .find_map(|node| node.downcast_current_context::<isideload::SideloadError>())
    {
        return ("auth".to_string(), message);
    }
    let lower = message.to_lowercase();
    let category = if lower.contains("sign in") || lower.contains("login")
        || lower.contains("two factor") || lower.contains("2fa")
        || lower.contains("verification code")
    {
        "auth"
    } else if lower.contains("failed to retrieve certificate identity")
        || lower.contains("certificate")
    {
        "certificate"
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

async fn sha256_file(path: &std::path::Path) -> Result<String, Box<dyn std::error::Error>> {
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
    let output_p12 = match args.get(2) {
        Some(v) => PathBuf::from(v),
        None => fail("other", "usage: p12export <apple_id> <output.p12>"),
    };
    let apple_password = match env::var("SIDEY_APPLE_MAIN_PASSWORD") {
        Ok(v) if !v.trim().is_empty() => v,
        _ => fail("other", "missing Apple password (set SIDEY_APPLE_MAIN_PASSWORD)"),
    };

    let machine_name = env::var("MACHINE_NAME").unwrap_or_else(|_| "isideload-minimal".to_string());

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

    let storage = Box::new(FsStorage::new(storage_dir()));
    let cert_identity = match CertificateIdentity::retrieve(
        &machine_name,
        &apple_id,
        &mut dev_session,
        &team,
        storage.as_ref(),
        &MaxCertsBehavior::Error,
    )
    .await
    {
        Ok(c) => c,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };

    let p12 = match cert_identity.as_p12(&cert_identity.machine_id).await {
        Ok(b) => b,
        Err(e) => {
            let (category, message) = classify(&e);
            fail(&category, &message);
        }
    };

    if let Some(parent) = output_p12.parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            fail("other", &format!("failed to create output directory: {e}"));
        }
    }
    if let Err(e) = std::fs::write(&output_p12, &p12) {
        fail("other", &format!("failed to write {}: {e}", output_p12.display()));
    }
    let sha256 = match sha256_file(&output_p12).await {
        Ok(h) => h,
        Err(e) => fail("other", &format!("failed to hash output p12: {e}")),
    };

    output_json(&serde_json::json!({
        "status": "ok",
        "p12_sha256": sha256,
        "cert_serial": cert_identity.get_serial_number(),
        "team_id": team.team_id,
        "machine_name": cert_identity.machine_name,
        "machine_id": cert_identity.machine_id,
    }));
}
