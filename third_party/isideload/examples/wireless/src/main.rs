//! Wireless install proof over an RSD tunnel (Phase B / Phase I refresh).
//!
//! Signs an already-extracted app for the target device with the account's
//! certificate identity, then installs the signed bundle over a RemotePairing
//! RSD tunnel. Unlike `install_app_rsd`, this example drives `sign_app`
//! directly and prints the *real* provisioning expiry from the signed bundle
//! so the refresh agent can schedule the next refresh from actual data.
//!
//! Usage:
//!   wireless <apple_id> <app_path>
//!
//! The Apple password is read from the SIDEY_APPLE_MAIN_PASSWORD environment
//! variable so it never appears in argv/process listings. The legacy
//! `wireless <apple_id> <password> <app_path>` form is still accepted with a
//! warning for one-off use.
//!
//! Env:
//!   RSD_ADDR   RSD tunnel address (default fd14:1218:6927::1)
//!   RSD_PORT   RSD tunnel port (default 51569)
//!   DEVICE_UDID    target device UDID
//!   DEVICE_NAME    target device display name
//!   DEVICE_TYPE    ios|tvos|watchos (default ios)
//!
//! Success output (single JSON line on stdout):
//!   {"status":"ok","installed":true,"profile_expiry_at":"...","cert_serial":"...",
//!    "team_id":"...","signed_bundle_identifier":"..."}
use isideload::{
    anisette::remote_v3::RemoteV3AnisetteProvider,
    auth::apple_account::{AppleAccount, TwoFactorCallbackParams, TwoFactorCallbackResponse},
    dev::device_type::DeveloperDeviceType,
    dev::developer_session::DeveloperSession,
    dev::devices::DevicesApi,
    dev::teams::TeamsApi,
    sideload::{SideloaderBuilder, TeamSelection, builder::MaxCertsBehavior},
    util::fs_storage::FsStorage,
};
use std::{env, path::PathBuf};

use tracing::Level;
use tracing_subscriber::FmtSubscriber;

fn storage_dir() -> PathBuf {
    env::var("SIDEY_ISIDELOAD_STATE")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from("/var/lib/sidey/isideload"))
}

#[tokio::main]
async fn main() {
    isideload::init().expect("Failed to initialize error reporting");
    let subscriber = FmtSubscriber::builder().with_max_level(Level::INFO).finish();
    tracing::subscriber::set_global_default(subscriber).expect("setting default subscriber failed");

    let args: Vec<String> = env::args().collect();

    let apple_id = args
        .get(1)
        .expect("Please provide the Apple ID to use for installation");
    // Password via env (never argv). Legacy 4-arg form (password as argv[2])
    // still works so older callers keep functioning, but is logged.
    let legacy_password = args.len() >= 4;
    if legacy_password {
        eprintln!(
            "WARNING: wireless called with the Apple password on the command line; use SIDEY_APPLE_MAIN_PASSWORD instead"
        );
    }
    let apple_password = match (env::var("SIDEY_APPLE_MAIN_PASSWORD"), legacy_password) {
        (Ok(v), _) if !v.is_empty() => v,
        (_, true) => args[2].clone(),
        _ => panic!("Please provide the Apple password via SIDEY_APPLE_MAIN_PASSWORD"),
    };
    let app_path = PathBuf::from(
        args.get(if legacy_password { 3 } else { 2 })
            .expect("Please provide the path to the app to install"),
    );

    let rsd_host: std::net::IpAddr = env::var("RSD_ADDR")
        .unwrap_or_else(|_| "fd14:1218:8b43::1".to_string())
        .parse()
        .expect("RSD_ADDR must be an IP");
    let rsd_port: u16 = env::var("RSD_PORT")
        .unwrap_or_else(|_| "51569".to_string())
        .parse()
        .expect("RSD_PORT must be a port");
    let device_name = env::var("DEVICE_NAME").unwrap_or_else(|_| "ACU Covert Camera".to_string());
    let device_udid = env::var("DEVICE_UDID").unwrap_or_else(|_| {
        panic!("DEVICE_UDID is required: refusing to sign without a target device")
    });
    let device_type: Option<DeveloperDeviceType> = match env::var("DEVICE_TYPE").as_deref() {
        Ok("tvos") => Some(DeveloperDeviceType::Tvos),
        Ok("watchos") => Some(DeveloperDeviceType::Watchos),
        Ok("ios") | Ok("") | Err(_) => None,
        Ok(other) => panic!("unknown DEVICE_TYPE {other:?} (ios|tvos|watchos)"),
    };

    let get_2fa_code = |params: TwoFactorCallbackParams| {
        if params.unknown {
            println!("The most recently attempted 2FA method failed, please try a different method.");
        } else if params.sms {
            println!("SMS 2FA code requested; enter the code from your messages.");
        } else {
            println!(
                "2FA push sent to your devices. Codes expire quickly; prefer SMS: send 'p<id>' to request one."
            );
        }
        for n in &params.numbers {
            println!("SMS number ID {}: {}", n.id, n.number_with_dial_code);
        }

        let mut code = String::new();
        for _ in 0..600 {
            if let Ok(c) = std::fs::read_to_string("/tmp/opencode/2fa-code.txt") {
                let c = c.trim().to_string();
                if !c.is_empty() {
                    std::fs::remove_file("/tmp/opencode/2fa-code.txt").ok();
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
    };

    let account = AppleAccount::builder(apple_id)
        .anisette_provider(
            RemoteV3AnisetteProvider::default()
                .unwrap()
                .set_url(&env::var("ANISETTE_URL").unwrap_or_else(|_| "http://127.0.0.1:6970".to_string()))
                .set_serial_number("2".to_string())
                .set_storage(Box::new(FsStorage::new(storage_dir()))),
        )
        .login(&apple_password, Box::new(get_2fa_code))
        .await;

    let mut account = account.expect("Apple login failed");
    println!("LOGIN OK - account: {}", apple_id);

    let dev_session = DeveloperSession::from_account(&mut account)
        .await
        .expect("Failed to create developer session");

    let stream = tokio::net::TcpStream::connect((rsd_host, rsd_port))
        .await
        .expect("Failed to connect to RSD tunnel");
    let mut handshake = idevice::rsd::RsdHandshake::new(stream)
        .await
        .expect("RSD handshake failed");
    println!(
        "RSD handshake OK - protocol v{}, {} services",
        handshake.protocol_version,
        handshake.services.len()
    );
    let mut provider = rsd_host;

    println!("Registering device {} with the team...", device_udid);
    let mut dev_session = dev_session;
    let teams = dev_session
        .list_teams()
        .await
        .expect("failed to list developer teams");
    let team = teams
        .first()
        .expect("no developer teams available")
        .clone();
    dev_session
        .ensure_device_registered(&team, &device_name, &device_udid, device_type.clone())
        .await
        .expect("failed to register device on the team");

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

    let mut sideloader = SideloaderBuilder::new(dev_session, apple_id.to_string())
        .team_selection(TeamSelection::PromptOnce(team_selection_prompt))
        .max_certs_behavior(MaxCertsBehavior::Prompt(Box::new(cert_selection_prompt)))
        .storage(Box::new(FsStorage::new(storage_dir())))
        .machine_name("isideload-minimal".to_string())
        .device_type(device_type)
        .build();

    let signed_app = sideloader
        .sign_app(app_path.clone(), Some(team.clone()), true)
        .await
        .unwrap_or_else(|e| panic!("signing failed: {e:?}"));

    println!("App signed - installing over RSD tunnel...");
    isideload::sideload::install::install_app_rsd(
        &mut provider,
        &mut handshake,
        &signed_app.bundle_dir,
        |progress| println!("Installing: {}%", progress),
    )
    .await
    .expect("Failed to install app on device over RSD");

    println!("TERMINAL INSTALL Complete");
    println!(
        "{}",
        serde_json::json!({
            "status": "ok",
            "installed": true,
            "profile_expiry_at": signed_app.profile_expiry.to_xml_format(),
            "cert_serial": signed_app.cert_serial,
            "team_id": signed_app.team_id,
            "signed_bundle_identifier": signed_app.signed_bundle_identifier,
        })
    );
}