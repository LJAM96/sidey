import SwiftUI

struct SettingsView: View {
    @Environment(SessionStore.self) private var session
    @AppStorage("notifyExpiring") private var notifyExpiring = true
    @State private var serverField = ""
    @State private var tokenField = ""
    @State private var showToken = false

    var body: some View {
        NavigationStack {
            Form {
                Section("Server URL") {
                    TextField("http://100.x.y.z:8080", text: $serverField)
                        .keyboardType(.URL)
                        .autocorrectionDisabled()
                        .textInputAutocapitalization(.never)
                        .accessibilityLabel("Server URL")
                    NavigationLink("Pair with QR Code") {
                        PairingView()
                    }
                    connectionRow
                }

                Section("Device Token") {
                    Group {
                        if showToken {
                            TextField("Paste token", text: $tokenField)
                        } else {
                            SecureField("Paste token", text: $tokenField)
                        }
                    }
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                    .accessibilityLabel("Device token")
                    Button(showToken ? "Hide Token" : "Show Token") {
                        showToken.toggle()
                    }
                    .font(.callout)
                    Text("Leave blank to keep the current token.")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Button("Save & Test Connection") {
                        session.save(urlString: serverField, token: tokenField)
                        tokenField = ""
                        Task { await session.checkConnection() }
                    }
                    .disabled(serverField.trimmingCharacters(in: .whitespaces).isEmpty)
                }

                Section("Notifications") {
                    Toggle("Expiring profile reminders", isOn: $notifyExpiring)
                        .onChange(of: notifyExpiring) { _, newValue in
                            Task { await ExpiryReminders.setEnabled(newValue) }
                        }
                }

                Section("Session") {
                    Button("Forget Server", role: .destructive) {
                        session.forget()
                        serverField = ""
                        tokenField = ""
                    }
                }

                Section("About") {
                    LabeledContent("Version", value: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "1.0")
                    LabeledContent("Paired", value: session.isPaired ? "Yes" : "No")
                }
            }
            .navigationTitle("Settings")
            .onAppear {
                if serverField.isEmpty {
                    serverField = session.serverURLString
                }
            }
        }
    }

    @ViewBuilder
    private var connectionRow: some View {
        switch session.connection {
        case .unknown:
            Text("Not tested yet.")
                .foregroundStyle(.secondary)
        case .checking:
            ProgressView("Testing…")
        case .online:
            Label("Reachable", systemImage: "checkmark.circle.fill")
                .foregroundStyle(.green)
        case .offline(let message):
            VStack(alignment: .leading) {
                Label("Unreachable", systemImage: "xmark.circle.fill")
                    .foregroundStyle(.red)
                Text(message)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                if !session.currentKnownHosts.isEmpty {
                    Button("Search Again") {
                        Task {
                            _ = await session.heal()
                        }
                    }
                    .font(.callout)
                    .accessibilityLabel("Search again for the paired server")
                    .accessibilityHint("Probes remembered addresses for the paired server and reconnects automatically on a match.")
                }
            }
        }
    }
}
