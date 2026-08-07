use idevice::usbmuxd::{UsbmuxdAddr, UsbmuxdConnection};
use isideload::{
    anisette::remote_v3::RemoteV3AnisetteProvider,
    auth::apple_account::{AppleAccount, TwoFactorCallbackParams, TwoFactorCallbackResponse},
    dev::{
        certificates::DevelopmentCertificate, developer_session::DeveloperSession,
        teams::DeveloperTeam,
    },
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
    let subscriber = FmtSubscriber::builder()
        .with_max_level(Level::DEBUG)
        .finish();
    tracing::subscriber::set_global_default(subscriber).expect("setting default subscriber failed");

    let args: Vec<String> = env::args().collect();

    let apple_id = args
        .get(1)
        .expect("Please provide the Apple ID to use for installation");
    let apple_password = args.get(2).expect("Please provide the Apple ID password");
    let app_path = PathBuf::from(
        args.get(3).unwrap_or(&"".to_string()), // .expect("Please provide the path to the app to install"),
    );

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

        // Poll /tmp/opencode/2fa-code.txt for up to 10 minutes (SMS codes are
        // valid for several minutes), then fall back to stdin.
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
                .set_url(&std::env::var("ANISETTE_URL").unwrap_or_else(|_| "http://127.0.0.1:6970".to_string()))
                .set_serial_number("2".to_string())
                .set_storage(Box::new(FsStorage::new(storage_dir()))),
        )
        .login(apple_password, Box::new(get_2fa_code))
        .await;

    let mut account = account.unwrap();
    println!("LOGIN OK - account: {}", apple_id);

    let dev_session = DeveloperSession::from_account(&mut account)
        .await
        .expect("Failed to create developer session");

    let usbmuxd = UsbmuxdConnection::default().await;
    if usbmuxd.is_err() {
        panic!("Failed to connect to usbmuxd: {:?}", usbmuxd.err());
    }
    let mut usbmuxd = usbmuxd.unwrap();

    let devs = usbmuxd.get_devices().await.unwrap();
    if devs.is_empty() {
        panic!("No devices found");
    }

    let provider = devs
        .first()
        .unwrap()
        .to_provider(UsbmuxdAddr::from_env_var().unwrap(), "isideload-demo");

    let team_selection_prompt = |teams: &Vec<DeveloperTeam>| {
        teams.first().map(|t| t.team_id.clone())
    };

    let cert_selection_prompt = |certs: &Vec<DevelopmentCertificate>| {
        println!("Maximum number of certificates reached. Please select certificates to revoke:");
        for (index, cert) in certs.iter().enumerate() {
            println!(
                "({}) {}: {}",
                index + 1,
                cert.name.as_deref().unwrap_or("<Unnamed>"),
                cert.machine_name.as_deref().unwrap_or("<No Machine Name>"),
            );
        }
        println!("Enter the numbers of the certificates to revoke, separated by commas:");
        let mut input = String::new();
        std::io::stdin().read_line(&mut input).unwrap();
        let selections: Vec<usize> = input
            .trim()
            .split(',')
            .filter_map(|s| s.trim().parse::<usize>().ok())
            .filter(|&n| n > 0 && n <= certs.len())
            .collect();
        if selections.is_empty() {
            return None;
        }
        Some(
            selections
                .into_iter()
                .map(|n| certs[n - 1].serial_number.clone().unwrap_or_default())
                .collect::<Vec<_>>(),
        )
    };

    let mut sideloader = SideloaderBuilder::new(dev_session, apple_id.to_string())
        .team_selection(TeamSelection::PromptOnce(team_selection_prompt))
        .max_certs_behavior(MaxCertsBehavior::Prompt(Box::new(cert_selection_prompt)))
        .storage(Box::new(FsStorage::new(storage_dir())))
        .machine_name("isideload-minimal".to_string())
        .build();

    let result = sideloader.install_app(&provider, app_path, true).await;
    match result {
        Ok(_) => println!("App installed successfully"),
        Err(e) => panic!("{}", e),
    }
}
