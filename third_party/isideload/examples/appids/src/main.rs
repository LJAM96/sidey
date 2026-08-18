//! Developer portal App ID maintenance for the free (10-slot) team quota.
//!
//! Every unique bundle identifier that is signed (the app plus each of its
//! extensions) registers an App ID on Apple's portal, capped at 10 per free
//! team. When the cap is reached, sign jobs fail with "Not enough available
//! app IDs". This tool lists what is registered and deletes stale App IDs
//! (e.g. from apps that were replaced or uninstalled) so signing continues.
//!
//! Usage:
//!   appids <apple_id> list
//!   appids <apple_id> delete <app_id_id> [<app_id_id> ...]
//!
//! Env: SIDEY_ISIDELOAD_STATE (shared storage: session cookies, cert identity),
//!      ANISETTE_URL (default http://127.0.0.1:6970),
//!      DEVICE_TYPE (ios|tvos|watchos, default ios),
//!      SIDEY_APPLE_MAIN_PASSWORD (password; positional arg not accepted),
//!      SIGNONLY_2FA_CODE_FILE (same convention as signonly)
//!
//! Output is a single JSON document on stdout: {"status":"ok", mode,
//! "app_ids":[...], "max_quantity":N, "available_quantity":N} or
//! {"status":"error", "category":..., "error":...} with exit code 1.
use isideload::{
    anisette::remote_v3::RemoteV3AnisetteProvider,
    auth::apple_account::{AppleAccount, TwoFactorCallbackParams, TwoFactorCallbackResponse},
    dev::{
        app_ids::AppIdsApi,
        developer_session::DeveloperSession,
        teams::TeamsApi,
    },
    util::fs_storage::FsStorage,
};
use std::{
    env,
    io::Write,
    path::PathBuf,
};

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

fn get_2fa_code(params: &TwoFactorCallbackParams) -> TwoFactorCallbackResponse {
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
    TwoFactorCallbackResponse::SubmitCode(code.trim().to_string())
}

#[tokio::main]
async fn main() {
    isideload::init().expect("Failed to initialize error reporting");
    let subscriber = tracing_subscriber::FmtSubscriber::builder()
        .with_max_level(tracing::Level::INFO)
        .with_writer(std::io::stderr)
        .finish();
    tracing::subscriber::set_global_default(subscriber).expect("setting default subscriber failed");

    let args: Vec<String> = env::args().collect();
    let apple_id = match args.get(1) {
        Some(v) => v.clone(),
        None => fail("other", "missing Apple ID argument"),
    };
    let command = match args.get(2).map(String::as_str) {
        Some("list") => "list",
        Some("delete") => "delete",
        _ => fail("other", "usage: appids <apple_id> list|delete <app_id_id> [...]"),
    };

    let apple_password = match env::var("SIDEY_APPLE_MAIN_PASSWORD") {
        Ok(v) if !v.is_empty() => v,
        _ => fail("other", "missing Apple password (set SIDEY_APPLE_MAIN_PASSWORD)"),
    };

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
        Err(e) => fail("auth", &format!("{e:?}")),
    };

    let mut dev_session = match DeveloperSession::from_account(&mut account).await {
        Ok(d) => d,
        Err(e) => fail("auth", &format!("{e:?}")),
    };

    let teams = match dev_session.list_teams().await {
        Ok(t) => t,
        Err(e) => fail("other", &format!("{e:?}")),
    };
    let team = match teams.first() {
        Some(t) => t.clone(),
        None => fail("auth", "no developer teams available for account"),
    };

    let list = match dev_session.list_app_ids(&team, device_type.clone()).await {
        Ok(l) => l,
        Err(e) => fail("other", &format!("{e:?}")),
    };

    let deleted: Vec<String> = if command == "delete" {
        let ids: Vec<&String> = args.iter().skip(3).collect();
        if ids.is_empty() {
            fail("other", "delete requires at least one app_id_id");
        }
        let mut gone = Vec::new();
        for id in ids {
            eprintln!("appids: deleting App ID {id}");
            if let Err(e) = dev_session.delete_app_id(&team, id, device_type.clone()).await {
                fail("other", &format!("failed to delete App ID {id}: {e:?}"));
            }
            gone.push(id.clone());
        }
        gone
    } else {
        Vec::new()
    };

    let app_ids: Vec<serde_json::Value> = list
        .app_ids
        .iter()
        .map(|a| {
            serde_json::json!({
                "app_id_id": a.app_id_id,
                "identifier": a.identifier,
                "name": a.name,
            })
        })
        .collect();

    output_json(&serde_json::json!({
        "status": "ok",
        "mode": command,
        "deleted": deleted,
        "app_ids": app_ids,
        "max_quantity": list.max_quantity,
        "available_quantity": list.available_quantity,
    }));
}