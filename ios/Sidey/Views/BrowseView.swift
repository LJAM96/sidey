import SwiftUI
import UniformTypeIdentifiers

@MainActor
@Observable
final class BrowseModel {
    enum State {
        case loading
        case loaded
        case failed(String)
    }

    var state: State = .loading
    var apps: [StoreApp] = []
    var sources: [StoreSource] = []
    var query = ""
    var sourceFilter = "all"
    var devices: [DeviceItem] = []
    private var loadGen = 0

    var filtered: [StoreApp] {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines)
        return apps.filter { app in
            (sourceFilter == "all" || app.source == sourceFilter)
                && (q.isEmpty
                    || app.name.localizedStandardContains(q)
                    || app.bundleID.localizedStandardContains(q))
        }
    }

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
            async let apps = client.storeApps()
            async let sources = client.storeSources()
            async let devices = client.devices()
            let (a, s, d) = try await (apps, sources, devices)
            guard gen == loadGen else { return }
            self.apps = a
            self.sources = s
            self.devices = d
            state = .loaded
        } catch {
            guard gen == loadGen else { return }
            if case .loaded = state {
                return
            }
            state = .failed((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }
}

struct BrowseView: View {
    @Environment(SessionStore.self) private var session
    @State private var model = BrowseModel()
    @State private var importing = false
    @State private var uploading = false
    @State private var uploadMessage: String?
    @State private var uploadTick = 0

    /// IPA files (plus plain zips as fallback) — not arbitrary documents.
    static var ipaContentTypes: [UTType] {
        [UTType(filenameExtension: "ipa"), UTType.zip].compactMap { $0 }
    }

    var body: some View {
        NavigationStack {
            content
                .navigationTitle("Browse")
                .searchable(text: $model.query, prompt: "Apps, bundle IDs…")
                .searchToolbarBehavior(.minimize)
                .toolbar {
                    ToolbarItem(placement: .primaryAction) {
                        Button {
                            importing = true
                        } label: {
                            Label("Upload IPA", systemImage: "square.and.arrow.up")
                        }
                        .disabled(uploading)
                    }
                }
                .refreshable {
                    await model.load(session: session)
                }
                .task {
                    await model.load(session: session)
                }
                .fileImporter(isPresented: $importing, allowedContentTypes: Self.ipaContentTypes) { result in
                    Task { await upload(result) }
                }
                .sensoryFeedback(.success, trigger: uploadTick)
                .alert("Upload", isPresented: Binding(
                    get: { uploadMessage != nil },
                    set: { if !$0 { uploadMessage = nil } }
                )) {
                    Button("OK", role: .cancel) {}
                } message: {
                    Text(uploadMessage ?? "")
                }
        }
    }

    @ViewBuilder
    private var content: some View {
        switch model.state {
        case .loading:
            ProgressView("Loading catalog…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        case .failed(let message):
            InlineError(message: message) {
                Task { await model.load(session: session) }
            }
        case .loaded:
            List {
                if !model.sources.isEmpty {
                    Picker("Source", selection: $model.sourceFilter) {
                        Text("All Sources").tag("all")
                        ForEach(model.sources) { src in
                            Text(src.name).tag(src.name)
                        }
                    }
                    .pickerStyle(.menu)
                }
                ForEach(model.filtered) { app in
                    NavigationLink(value: app) {
                        AppRow(app: app)
                    }
                }
            }
            .navigationDestination(for: StoreApp.self) { app in
                AppDetailView(app: app, devices: model.devices)
            }
        }
    }

    private func upload(_ result: Result<URL, Error>) async {
        uploading = true
        defer { uploading = false }
        do {
            let url = try result.get()
            // Hold security-scoped access for the whole streaming upload.
            guard url.startAccessingSecurityScopedResource() else {
                uploadMessage = "Could not access that file."
                return
            }
            defer { url.stopAccessingSecurityScopedResource() }
            let client = try session.client()
            let id = try await client.uploadIPA(filename: url.lastPathComponent, fileURL: url)
            uploadMessage = "Uploaded into quarantine (\(id)). Approve it to use it."
            uploadTick += 1
            announce(uploadMessage ?? "Upload complete.")
        } catch {
            uploadMessage = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            announce(uploadMessage ?? "Upload failed.")
        }
    }
}

struct AppRow: View {
    let app: StoreApp

    var body: some View {
        HStack(spacing: 12) {
            AsyncImage(url: URL(string: app.iconURL)) { image in
                image.resizable()
            } placeholder: {
                Image(systemName: "app.dashed")
                    .foregroundStyle(.secondary)
            }
            .frame(width: 44, height: 44)
            .clipShape(RoundedRectangle(cornerRadius: 10))
            .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                Text(app.name).font(.headline).lineLimit(1)
                Text("\(app.version) · \(app.source)")
                    .font(.caption).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer()
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(app.name), version \(app.version), from \(app.source)")
        .accessibilityHint("Double tap for details")
    }
}

struct AppDetailView: View {
    @Environment(SessionStore.self) private var session
    let app: StoreApp
    let devices: [DeviceItem]
    @State private var versionIndex = 0
    @State private var deviceID: String
    @State private var message: String?
    @State private var working = false
    @State private var watching = false
    @State private var successTick = 0
    @State private var showingInstallConfirm = false
    @State private var importedArtifactID: String?

    init(app: StoreApp, devices: [DeviceItem]) {
        self.app = app
        self.devices = devices
        _deviceID = State(initialValue: devices.first?.id ?? "")
    }

    private var activeVersion: StoreVersion? {
        guard !app.versions.isEmpty else { return nil }
        return app.versions[min(versionIndex, app.versions.count - 1)]
    }

    var body: some View {
        List {
            Section {
                HStack(spacing: 12) {
                    AsyncImage(url: URL(string: app.iconURL)) { image in
                        image.resizable()
                    } placeholder: {
                        Image(systemName: "app.dashed").foregroundStyle(.secondary)
                    }
                    .frame(width: 60, height: 60)
                    .clipShape(RoundedRectangle(cornerRadius: 14))
                    .accessibilityHidden(true)
                    VStack(alignment: .leading) {
                        Text(app.name).font(.headline)
                        Text(app.developer).font(.subheadline).foregroundStyle(.secondary)
                    }
                }
                if !app.description.isEmpty {
                    Text(app.description).font(.callout).foregroundStyle(.secondary)
                }
            }

            if app.versions.count > 1 {
                Section("Release") {
                    Picker("Release", selection: $versionIndex) {
                        ForEach(Array(app.versions.enumerated()), id: \.element.stableID) { i, ver in
                            Text("\(ver.version) · \(ver.channel)").tag(i)
                        }
                    }
                    .pickerStyle(.menu)
                }
            }

            Section("Install") {
                if devices.isEmpty {
                    Text("No devices registered — add one before installing.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                } else {
                    Picker("Device", selection: $deviceID) {
                        ForEach(devices) { d in
                            Text(d.name.isEmpty ? d.udid : d.name).tag(d.id)
                        }
                    }
                }
                Button {
                    showingInstallConfirm = true
                } label: {
                    if working {
                        ProgressView()
                            .frame(maxWidth: .infinity)
                    } else {
                        Label("Install…", systemImage: "square.and.arrow.down")
                            .frame(maxWidth: .infinity)
                    }
                }
                .disabled(deviceID.isEmpty || working)
                .confirmationDialog("Install \(app.name)?", isPresented: $showingInstallConfirm, titleVisibility: .visible) {
                    Button("Install via LiveContainer") { install(mode: "livecontainer") }
                    Button("Install Natively") { install(mode: "native") }
                    Button("Cancel", role: .cancel) {}
                } message: {
                    Text("The IPA downloads into quarantine first. Each install uses one Apple login.")
                }
                if let importedArtifactID {
                    Button {
                        Task {
                            do {
                                let client = try session.client()
                                try await client.approveArtifact(id: importedArtifactID)
                                message = "Approved — it can now be signed and installed."
                                successTick += 1
                                self.importedArtifactID = nil
                            } catch {
                                message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
                            }
                        }
                    } label: {
                        Label("Approve Quarantined IPA", systemImage: "checkmark.seal")
                            .frame(maxWidth: .infinity)
                    }
                }
                Button(watching ? "Unwatch Updates" : "Watch for Updates") { toggleWatch() }
                    .sensoryFeedback(.selection, trigger: watching)
            }

            if let message {
                Section {
                    Text(message).font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle(app.name)
        .navigationBarTitleDisplayMode(.inline)
        .sensoryFeedback(.success, trigger: successTick)
    }

    private func install(mode: String) {
        guard let ver = activeVersion else {
            message = "No downloadable release."
            return
        }
        working = true
        Task {
            defer { working = false }
            do {
                let client = try session.client()
                let result = try await client.importApp(
                    name: "\(app.name)-\(ver.version)",
                    downloadURL: ver.downloadURL,
                    deviceID: deviceID,
                    mode: mode
                )
                message = result.message
                importedArtifactID = result.artifactID
                successTick += 1
                announce(result.message)
            } catch {
                message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
                announce(message ?? "Install failed.")
            }
        }
    }

    private func toggleWatch() {
        Task {
            do {
                let client = try session.client()
                if watching {
                    try await client.deleteSubscription(bundleID: app.bundleID)
                    watching = false
                    message = "Stopped watching \(app.name)."
                } else {
                    try await client.addSubscription(bundleID: app.bundleID)
                    watching = true
                    message = "Watching \(app.name) for updates."
                }
            } catch {
                message = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            }
        }
    }
}
