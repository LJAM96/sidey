import SwiftUI

@MainActor
@Observable
final class MyAppsModel {
    enum State {
        case loading
        case loaded
        case failed(String)
    }

    struct DeviceGroup: Identifiable {
        let device: DeviceItem
        let deployments: [DeploymentItem]
        var id: String { device.id }
    }

    var state: State = .loading
    var groups: [DeviceGroup] = []
    var refreshEntries: [String: RefreshEntry] = [:] // deployment_id -> entry
    /// Recent install/refresh history per deployment, newest first.
    var historyByDeployment: [String: [JobItem]] = [:]
    var refreshing: Set<String> = []
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
            async let devices = client.devices()
            async let deployments = client.deployments()
            async let entries = client.refreshCalendar()
            async let jobs = client.jobs()
            let (d, deps, e, j) = try await (devices, deployments, entries, jobs)
            guard gen == loadGen else { return }
            refreshEntries = Dictionary(uniqueKeysWithValues: e.map { ($0.id, $0) })
            let byDevice = Dictionary(grouping: deps, by: \.deviceID)
            groups = d.map { device in
                DeviceGroup(device: device, deployments: byDevice[device.id] ?? [])
            }
            var history: [String: [JobItem]] = [:]
            for job in j where job.jobType == "install" || job.jobType == "refresh" {
                // History rows carry deployment_id in parameters when the
                // orchestrator created them; fall back to device scope.
                history[job.deviceID, default: []].append(job)
            }
            for key in history.keys {
                history[key]?.sort { ($0.createdAt ?? .distantPast) > ($1.createdAt ?? .distantPast) }
            }
            historyByDeployment = history
            state = .loaded
        } catch {
            guard gen == loadGen else { return }
            if case .loaded = state {
                return
            }
            state = .failed((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }

    /// Newest install/refresh jobs relevant to a deployment (device scope,
    /// newest first).
    func history(for deployment: DeploymentItem) -> [JobItem] {
        Array((historyByDeployment[deployment.deviceID] ?? []).prefix(5))
    }

    func refresh(deploymentID: String, session: SessionStore) async -> String? {
        refreshing.insert(deploymentID)
        defer { refreshing.remove(deploymentID) }
        do {
            let client = try session.client()
            try await client.triggerRefresh(deploymentID: deploymentID)
            await load(session: session)
            return nil
        } catch {
            return (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
        }
    }
}

struct MyAppsView: View {
    @Environment(SessionStore.self) private var session
    @State private var model = MyAppsModel()
    @State private var confirmRefresh: DeploymentItem?
    @State private var alertMessage: String?
    @State private var successTick = 0

    var body: some View {
        NavigationStack {
            content
                .navigationTitle("My Apps")
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
                .sensoryFeedback(.success, trigger: successTick)
                .task {
                    await model.load(session: session)
                }
                .confirmationDialog(
                    "Start refresh?",
                    isPresented: Binding(
                        get: { confirmRefresh != nil },
                        set: { if !$0 { confirmRefresh = nil } }
                    ),
                    presenting: confirmRefresh
                ) { dep in
                    Button("Refresh \(dep.currentBundle.isEmpty ? dep.target : dep.currentBundle)") {
                        Task {
                            if let err = await model.refresh(deploymentID: dep.id, session: session) {
                                alertMessage = err
                                announce(err)
                            } else {
                                successTick += 1
                                announce("Refresh started.")
                            }
                        }
                    }
                    Button("Cancel", role: .cancel) {}
                } message: { _ in
                    Text("The server will re-sign and reinstall. This uses one Apple login.")
                }
                .alert("Refresh failed", isPresented: Binding(
                    get: { alertMessage != nil },
                    set: { if !$0 { alertMessage = nil } }
                )) {
                    Button("OK", role: .cancel) {}
                } message: {
                    Text(alertMessage ?? "")
                }
        }
    }

    @ViewBuilder
    private var content: some View {
        switch model.state {
        case .loading:
            ProgressView("Loading apps…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        case .failed(let message):
            InlineError(message: message) {
                Task { await model.load(session: session) }
            }
        case .loaded:
            if model.groups.allSatisfy(\.deployments.isEmpty) {
                EmptyState(title: "No Apps", systemImage: "apps.iphone",
                           detail: "Deploy an app from the dashboard to manage it here.")
            } else {
                List {
                    ForEach(model.groups) { group in
                        if !group.deployments.isEmpty {
                            Section(group.device.name.isEmpty ? group.device.udid : group.device.name) {
                                ForEach(group.deployments) { dep in
                                    NavigationLink(value: dep) {
                                        DeploymentRow(
                                            deployment: dep,
                                            entry: model.refreshEntries[dep.id]
                                        )
                                    }
                                }
                            }
                        }
                    }
                }
                .navigationDestination(for: DeploymentItem.self) { dep in
                    DeploymentDetailView(
                        deployment: dep,
                        entry: model.refreshEntries[dep.id],
                        history: model.history(for: dep),
                        onRefresh: { confirmRefresh = dep },
                        isRefreshing: model.refreshing.contains(dep.id)
                    )
                }
            }
        }
    }
}

struct DeploymentRow: View {
    let deployment: DeploymentItem
    let entry: RefreshEntry?

    var body: some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(displayName).font(.headline)
                Text(displayVersion).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            ExpiryBadge(days: entry?.daysToExpiry)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(displayName), \(displayVersion.isEmpty ? "no version" : displayVersion)")
        .accessibilityHint("Double tap for details")
    }

    private var displayName: String {
        if let entry, !entry.appName.isEmpty { return entry.appName }
        return deployment.currentBundle.isEmpty ? deployment.target : deployment.currentBundle
    }

    private var displayVersion: String {
        [deployment.currentVersion, deployment.currentState].filter { !$0.isEmpty }.joined(separator: " · ")
    }
}

struct DeploymentDetailView: View {
    let deployment: DeploymentItem
    let entry: RefreshEntry?
    let history: [JobItem]
    var onRefresh: () -> Void
    var isRefreshing: Bool

    var body: some View {
        List {
            Section("App") {
                LabeledContent("Bundle", value: bundleID)
                LabeledContent("Version", value: deployment.currentVersion.isEmpty ? "—" : deployment.currentVersion)
                LabeledContent("State", value: deployment.currentState.isEmpty ? "—" : deployment.currentState)
                LabeledContent("Policy", value: deployment.updatePolicy.isEmpty ? "—" : deployment.updatePolicy)
            }
            Section("Signing") {
                LabeledContent("Profile expires", value: entry?.profileExpiry?.shortString ?? "—")
                LabeledContent("Next refresh", value: entry?.nextDue?.shortString ?? "—")
                LabeledContent("Last result", value: entry?.lastResult ?? "—")
                if let err = entry?.lastError, !err.isEmpty {
                    Text(err).font(.caption).foregroundStyle(.secondary)
                }
            }
            if !history.isEmpty {
                Section("Recent activity") {
                    ForEach(history) { job in
                        HStack {
                            Text("\(job.jobType) · \(job.state)").font(.subheadline)
                            Spacer()
                            if job.isActive, job.progress > 0 {
                                ProgressView(value: Double(job.progress), total: 100)
                                    .frame(width: 80)
                                    .accessibilityLabel("\(job.jobType) progress")
                                    .accessibilityValue("\(job.progress) percent")
                            }
                        }
                    }
                }
            }
            Section {
                Button {
                    onRefresh()
                } label: {
                    if isRefreshing {
                        ProgressView()
                            .frame(maxWidth: .infinity)
                            .accessibilityLabel("Refreshing")
                    } else {
                        Label("Refresh Now", systemImage: "arrow.clockwise")
                            .frame(maxWidth: .infinity)
                    }
                }
                .disabled(isRefreshing)
            }
        }
        .navigationTitle(bundleID)
        .navigationBarTitleDisplayMode(.inline)
    }

    private var bundleID: String {
        deployment.currentBundle.isEmpty ? (entry?.bundleID ?? "—") : deployment.currentBundle
    }
}
