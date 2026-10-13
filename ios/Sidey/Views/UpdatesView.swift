import SwiftUI

@MainActor
@Observable
final class UpdatesModel {
    enum State {
        case loading
        case loaded
        case failed(String)
    }

    var state: State = .loading
    var updates: [UpdateEntry] = []
    var devices: [DeviceItem] = []
    var importing: Set<String> = []
    var notice: String?
    var successTick = 0
    var selectedDeviceID = ""
    private var loadGen = 0

    func load(session: SessionStore) async {
        loadGen += 1
        let gen = loadGen
        if case .loaded = state {
            // keep stale lists visible
        } else {
            state = .loading
        }
        do {
            let client = try session.client()
            async let updates = client.updates()
            async let devices = client.devices()
            let (u, d) = try await (updates, devices)
            guard gen == loadGen else { return }
            self.updates = u
            self.devices = d
            if selectedDeviceID.isEmpty || !d.contains(where: { $0.id == selectedDeviceID }) {
                selectedDeviceID = d.first?.id ?? ""
            }
            state = .loaded
        } catch {
            guard gen == loadGen else { return }
            if case .loaded = state {
                return
            }
            state = .failed((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    func importUpdate(_ update: UpdateEntry, deviceID: String, mode: String, session: SessionStore) async {
        guard let url = update.latestDownloadURL, !url.isEmpty else {
            notice = "No download available."
            return
        }
        let key = update.id + deviceID
        importing.insert(key)
        defer { importing.remove(key) }
        do {
            let client = try session.client()
            let name = "\(update.appName ?? update.bundleID)-\(update.latestVersion ?? "")"
            let result = try await client.importApp(name: name, downloadURL: url, deviceID: deviceID, mode: mode)
            notice = result.message
            successTick += 1
            announce(result.message)
            await load(session: session)
        } catch {
            notice = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            announce(notice ?? "Import failed.")
        }
    }
}

struct UpdatesView: View {
    @Environment(SessionStore.self) private var session
    @State private var model = UpdatesModel()

    var body: some View {
        NavigationStack {
            content
                .navigationTitle("Updates")
                .toolbar {
                    ToolbarItem(placement: .primaryAction) {
                        Button {
                            Task { await model.load(session: session) }
                        } label: {
                            Label("Refresh", systemImage: "arrow.clockwise")
                        }
                    }
                }
                .refreshable {
                    await model.load(session: session)
                }
                .sensoryFeedback(.success, trigger: model.successTick)
                .task {
                    await model.load(session: session)
                }
                .alert("Update", isPresented: Binding(
                    get: { model.notice != nil },
                    set: { if !$0 { model.notice = nil } }
                )) {
                    Button("OK", role: .cancel) {}
                } message: {
                    Text(model.notice ?? "")
                }
        }
    }

    @ViewBuilder
    private var content: some View {
        switch model.state {
        case .loading:
            ProgressView("Checking for updates…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        case .failed(let message):
            InlineError(message: message) {
                Task { await model.load(session: session) }
            }
        case .loaded:
            if model.updates.isEmpty {
                EmptyState(title: "Up to Date", systemImage: "checkmark.circle",
                           detail: "No newer versions in your sources. Watch apps in Browse to track them here.")
            } else {
                List {
                    if model.updates.contains(where: { ($0.deviceID ?? "").isEmpty }) {
                        Section("Import target") {
                            Picker("Import target", selection: $model.selectedDeviceID) {
                                ForEach(model.devices) { d in
                                    Text(d.name.isEmpty ? d.udid : d.name).tag(d.id)
                                }
                            }
                        }
                    }
                    ForEach(model.updates) { u in
                        let target = (u.deviceID?.isEmpty == false) ? (u.deviceID ?? "") : model.selectedDeviceID
                        let busy = model.importing.contains(u.id + target)
                        let appName = u.appName ?? u.bundleID
                        HStack {
                            VStack(alignment: .leading, spacing: 2) {
                                Text(appName).font(.headline)
                                Text("\(u.currentVersion ?? "—") → \(u.latestVersion ?? "?")")
                                    .font(.caption).foregroundStyle(.secondary)
                                    .accessibilityLabel("Version \(u.currentVersion ?? "unknown") to \(u.latestVersion ?? "unknown")")
                                Text(u.kind == "subscription" ? "Watched" : (u.deviceName ?? ""))
                                    .font(.caption2).foregroundStyle(.tertiary)
                            }
                            .accessibilityElement(children: .combine)
                            .accessibilityLabel("\(appName), version \(u.currentVersion ?? "unknown") to \(u.latestVersion ?? "unknown")")
                            Spacer()
                            Menu {
                                Button("Import via LiveContainer") {
                                    Task { await model.importUpdate(u, deviceID: target, mode: "livecontainer", session: session) }
                                }
                                Button("Import Natively") {
                                    Task { await model.importUpdate(u, deviceID: target, mode: "native", session: session) }
                                }
                            } label: {
                                Text(busy ? "Importing…" : "Import")
                            }
                            .buttonStyle(.bordered)
                            .disabled(target.isEmpty || busy)
                            .accessibilityLabel(busy ? "Importing \(appName) update" : "Import \(appName) update")
                        }
                    }
                }
            }
        }
    }
}
