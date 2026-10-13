import SwiftUI
import UIKit
import VisionKit

/// Pairing payload shown by the dashboard: {"v":1,"url":"…","token":"…"}.
/// A bare token string is also accepted (server URL taken from Settings).
struct PairingPayload {
    let url: String?
    let token: String

    static func parse(_ string: String) -> PairingPayload? {
        let trimmed = string.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return nil }
        if let data = trimmed.data(using: .utf8),
           let dict = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           let token = dict["token"] as? String, !token.isEmpty {
            return PairingPayload(url: dict["url"] as? String, token: token)
        }
        return PairingPayload(url: nil, token: trimmed)
    }
}

struct PairingView: View {
    @Environment(SessionStore.self) private var session
    @Environment(DiscoveryService.self) private var discovery
    @Environment(\.dismiss) private var dismiss
    @State private var urlField = ""
    @State private var tokenField = ""
    @State private var showToken = false
    @State private var error: String?
    @State private var scanning = false
    @State private var successTick = 0
    @State private var connectingID: String?

    /// Camera scanning is unavailable on some devices (and when denied);
    /// the manual form below is always the fallback.
    static var canScan: Bool {
        DataScannerViewController.isSupported && DataScannerViewController.isAvailable
    }

    var body: some View {
        Form {
            Section("Scan") {
                if Self.canScan {
                    Button("Scan Pairing QR") {
                        scanning = true
                    }
                } else {
                    Text("Camera scanning isn't available — enter the details manually below.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
            Section("Nearby servers") {
                if discovery.found.isEmpty {
                    Text(discovery.browsing
                         ? "Looking for Sidey servers on this network…"
                         : "No Sidey servers found on this network.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .accessibilityLabel(discovery.browsing
                            ? "Looking for Sidey servers"
                            : "No Sidey servers found")
                }
                ForEach(discovery.found) { server in
                    Button {
                        connect(server)
                    } label: {
                        LabeledContent {
                            badge(for: server)
                        } label: {
                            Text(server.name)
                            Text(server.displayHost)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                    .disabled(connectingID != nil)
                    .accessibilityLabel("Connect to \(server.name) at \(server.displayHost)")
                    .accessibilityHint(badgeHint(for: server))
                }
                if connectingID != nil {
                    ProgressView("Pairing…")
                        .accessibilityLabel("Pairing in progress")
                }
            }
            Section("Manual entry") {
                TextField("Server URL", text: $urlField)
                    .keyboardType(.URL)
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                    .accessibilityLabel("Server URL")
                Group {
                    if showToken {
                        TextField("Device token", text: $tokenField)
                    } else {
                        SecureField("Device token", text: $tokenField)
                    }
                }
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .accessibilityLabel("Device token")
                Button(showToken ? "Hide Token" : "Show Token") {
                    showToken.toggle()
                }
                .font(.callout)
                Button("Save Pairing") {
                    save(url: urlField, token: tokenField)
                }
                .disabled(urlField.isEmpty || tokenField.isEmpty)
            }
            if let error {
                Section {
                    Text(error)
                        .foregroundStyle(.red)
                        .font(.caption)
                }
            }
        }
        .navigationTitle("Pair Server")
        .navigationBarTitleDisplayMode(.inline)
        .sensoryFeedback(.success, trigger: successTick)
        .sheet(isPresented: $scanning) {
            NavigationStack {
                QRScannerView { code in
                    scanning = false
                    guard let payload = PairingPayload.parse(code) else {
                        error = "That QR code is not a Sidey pairing code."
                        announce(error ?? "Invalid code.")
                        return
                    }
                    save(url: payload.url ?? urlField, token: payload.token)
                }
                .navigationTitle("Scan QR Code")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) {
                        Button("Cancel") { scanning = false }
                    }
                }
                .accessibilityLabel("QR code scanner")
                .accessibilityHint("Point the camera at the pairing code, or cancel and enter details manually.")
            }
        }
        .onAppear {
            if urlField.isEmpty {
                urlField = session.serverURLString
            }
        }
        .task {
            // Browse while visible (LAN Bonjour) plus a re-probe of
            // remembered hosts (tailnet path — no mDNS there).
            discovery.startBrowsing()
            await discovery.reprobe(
                hosts: session.allKnownHosts,
                savedURL: session.baseURL,
                session: .shared
            )
        }
        .onDisappear {
            discovery.stopBrowsing()
        }
    }

    /// Known = paired with this server ID before (silent re-attach is
    /// safe); Unverified = legacy server with no identity to pin; New =
    /// first contact, one tap enrolls after the operator sees it here.
    private func badgeKind(for server: FoundServer) -> String {
        if server.serverID == nil { return "Unverified" }
        return session.isKnownServer(id: server.serverID) ? "Known" : "New"
    }

    @ViewBuilder
    private func badge(for server: FoundServer) -> some View {
        let kind = badgeKind(for: server)
        Text(kind)
            .font(.caption)
            .foregroundStyle(kind == "New" ? .orange : .secondary)
            .accessibilityHidden(true)
    }

    private func badgeHint(for server: FoundServer) -> String {
        switch badgeKind(for: server) {
        case "Known": return "Previously paired server. Connects immediately."
        case "Unverified": return "Older server without identity. Manual pairing may be needed."
        default: return "New server. One tap pairs this phone."
        }
    }

    /// One-tap pairing: the server mints a scoped device token for this
    /// phone (source-gated server-side). Legacy servers answer 404 and
    /// fall back to manual entry.
    private func connect(_ server: FoundServer) {
        error = nil
        connectingID = server.id
        announce("Pairing with \(server.name).")
        Task {
            defer { connectingID = nil }
            guard let url = server.url else {
                fail("That server address is not valid.")
                return
            }
            do {
                let appVersion = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "1.0"
                let (token, _) = try await APIClient.enroll(
                    baseURL: url,
                    deviceName: UIDevice.current.name,
                    appVersion: appVersion
                )
                session.save(urlString: url.absoluteString, token: token)
                await session.checkConnection()
                if case .online = session.connection {
                    successTick += 1
                    dismiss()
                } else {
                    fail("Paired, but the server did not answer. Check the URL and try again.")
                }
            } catch APIError.http(let code, _) where code == 404 {
                fail("That server is too old for one-tap pairing — enter the details manually below.")
            } catch APIError.http(_, let message) {
                fail("The server refused automatic pairing (\(message)). Pair with a dashboard token instead.")
            } catch {
                fail("Could not reach that server. Check the network and try again.")
            }
        }
    }

    /// Shows an error in the form and announces it (single choke point so
    /// `@State error` is never ambiguous inside catch scopes).
    private func fail(_ message: String) {
        error = message
        announce(message)
    }

    private func save(url: String, token: String) {
        error = nil
        guard !url.trimmingCharacters(in: .whitespaces).isEmpty, !token.isEmpty else {
            fail("Both a server URL and a token are required.")
            return
        }
        session.save(urlString: url, token: token)
        Task {
            await session.checkConnection()
            if case .online = session.connection {
                successTick += 1
                dismiss()
            } else {
                fail("Saved, but the server did not answer. Check the URL and try again.")
            }
        }
    }
}

struct QRScannerView: UIViewControllerRepresentable {
    var onCode: (String) -> Void

    func makeCoordinator() -> Coordinator {
        Coordinator(onCode: onCode)
    }

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let vc = DataScannerViewController(
            recognizedDataTypes: [.barcode(symbologies: [.qr])],
            qualityLevel: .balanced,
            recognizesMultipleItems: false,
            isHighFrameRateTrackingEnabled: false,
            isPinchToZoomEnabled: true,
            isGuidanceEnabled: true,
            isHighlightingEnabled: true
        )
        vc.delegate = context.coordinator
        try? vc.startScanning()
        return vc
    }

    func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {}

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let onCode: (String) -> Void
        private let lock = NSLock()
        private var done = false

        init(onCode: @escaping (String) -> Void) {
            self.onCode = onCode
        }

        func dataScanner(_ scanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            lock.lock()
            guard !done else { lock.unlock(); return }
            done = true
            lock.unlock()
            for item in addedItems {
                if case .barcode(let code) = item, let payload = code.payloadStringValue {
                    // Delegate fires off the main actor; hop back for UI state.
                    DispatchQueue.main.async { self.onCode(payload) }
                    return
                }
            }
        }
    }
}
