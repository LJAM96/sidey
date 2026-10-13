import SwiftUI

@MainActor
@Observable
final class HomeModel {
    enum State {
        case loading
        case loaded
        case failed(String)
    }

    var state: State = .loading
    var serverOnline = false
    var devices: [DeviceItem] = []
    var activeJobs: [JobItem] = []
    var pendingUpdates: [UpdateEntry] = []
    var authFailures: [JobItem] = []
    /// Set after a successful load; rows read these, never recompute in body.
    var expiring: [RefreshEntry] = []
    var attention: [RefreshEntry] = []
    private var loadGen = 0

    /// Pure classification shared by load() and tests: no networking.
    static func classify(_ entries: [RefreshEntry]) -> (expiring: [RefreshEntry], attention: [RefreshEntry]) {
        let expiring = entries
            .filter { ($0.daysToExpiry ?? .max) <= 7 }
            .sorted { ($0.daysToExpiry ?? .max) < ($1.daysToExpiry ?? .max) }
        return (expiring, entries.filter(\.needsAttention))
    }

    /// A recent Apple-auth sign failure usually means a 2FA code is waiting.
    var needs2FA: Bool { !authFailures.isEmpty }

    func load(session: SessionStore) async {
        loadGen += 1
        let gen = loadGen
        // Stale-while-revalidate: keep the last good lists on screen and
        // let .refreshable show its own spinner.
        if case .loaded = state {
            // keep stale lists visible
        } else {
            state = .loading
        }
        do {
            let client = try session.client()
            async let online = client.health()
            async let devices = client.devices()
            async let entries = client.refreshCalendar()
            async let jobs = client.jobs()
            async let updates = client.updates()
            let (o, d, e, j, u) = try await (online, devices, entries, jobs, updates)
            guard gen == loadGen else { return }
            serverOnline = o
            self.devices = d
            self.activeJobs = j.filter(\.isActive)
            self.authFailures = j.filter(\.isRecentlyFailedAuth)
            self.pendingUpdates = u
            let classified = Self.classify(e)
            self.expiring = classified.expiring
            self.attention = classified.attention
            state = .loaded
            await ExpiryReminders.sync(items: self.expiring.map {
                ($0.id, $0.appName.isEmpty ? $0.bundleID : $0.appName, $0.daysToExpiry ?? 0)
            })
        } catch {
            guard gen == loadGen else { return }
            // Preserve stale lists on failure when we have them.
            if case .loaded = state {
                return
            }
            state = .failed((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)
        }
    }
}

struct HomeView: View {
    @Environment(SessionStore.self) private var session
    @State private var model = HomeModel()
    @State private var showing2FA = false
    @State private var twoFACode = ""
    @State private var twoFAMessage: String?
    @State private var successTick = 0

    var body: some View {
        NavigationStack {
            content
                .navigationTitle("Sidey")
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
                .task {
                    await model.load(session: session)
                }
                .sheet(isPresented: $showing2FA) {
                    NavigationStack {
                        Form {
                            Section("Apple verification code") {
                                TextField("6-digit code", text: $twoFACode)
                                    .keyboardType(.numberPad)
                                    .textContentType(.oneTimeCode)
                                    .autocorrectionDisabled()
                                Button("Submit") {
                                    Task {
                                        do {
                                            let client = try session.client()
                                            try await client.submit2FA(code: twoFACode.trimmingCharacters(in: .whitespaces))
                                            twoFAMessage = "Code submitted — the pending sign-in will pick it up."
                                            twoFACode = ""
                                            successTick += 1
                                            announce(twoFAMessage ?? "Code submitted.")
                                            await model.load(session: session)
                                        } catch {
                                            twoFAMessage = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
                                            announce(twoFAMessage ?? "Submission failed.")
                                        }
                                    }
                                }
                                .disabled(twoFACode.trimmingCharacters(in: .whitespaces).isEmpty)
                            }
                            if let twoFAMessage {
                                Section {
                                    Text(twoFAMessage).font(.caption).foregroundStyle(.secondary)
                                }
                            }
                        }
                        .navigationTitle("2FA Code")
                        .navigationBarTitleDisplayMode(.inline)
                        .sensoryFeedback(.success, trigger: successTick)
                        .toolbar {
                            ToolbarItem(placement: .cancellationAction) {
                                Button("Done") { showing2FA = false }
                            }
                        }
                    }
                }
        }
    }

    @ViewBuilder
    private var content: some View {
        switch model.state {
        case .loading:
            ProgressView("Contacting server…")
                .frame(maxWidth: .infinity, maxHeight: .infinity)
        case .failed(let message):
            InlineError(message: message) {
                Task { await model.load(session: session) }
            }
        case .loaded:
            List {
                if model.needs2FA {
                    Section {
                        Button {
                            showing2FA = true
                        } label: {
                            Label("Apple needs verification — enter 2FA code", systemImage: "person.badge.key.fill")
                        }
                        .tint(.orange)
                    }
                }
                Section("Server") {
                    HStack {
                        LabeledContent("Status", value: model.serverOnline ? "Online" : "Unreachable")
                        StatusDot(kind: model.serverOnline ? .ok : .bad)
                    }
                    LabeledContent("Devices", value: "\(model.devices.count)")
                    LabeledContent("Updates available", value: "\(model.pendingUpdates.count)")
                }

                if !model.attention.isEmpty {
                    Section("Needs attention") {
                        ForEach(model.attention) { e in
                            let title = e.appName.isEmpty ? e.bundleID : e.appName
                            let detail = e.lastError.isEmpty ? e.lastResult : e.lastError
                            HStack(alignment: .top) {
                                VStack(alignment: .leading, spacing: 4) {
                                    Text(title)
                                        .font(.headline)
                                    Text(detail)
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                        .lineLimit(3)
                                }
                                Spacer()
                                StatusDot(kind: .bad)
                            }
                            .accessibilityElement(children: .combine)
                            .accessibilityLabel("\(title), needs attention, \(detail)")
                        }
                    }
                }

                Section("Expiring soon") {
                    if model.expiring.isEmpty {
                        Text("Nothing expiring within 7 days.")
                            .foregroundStyle(.secondary)
                    } else {
                        ForEach(model.expiring) { e in
                            let title = e.appName.isEmpty ? e.bundleID : e.appName
                            HStack {
                                VStack(alignment: .leading) {
                                    Text(title)
                                        .font(.headline)
                                    Text(e.deviceName)
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()
                                ExpiryBadge(days: e.daysToExpiry)
                            }
                            .accessibilityElement(children: .combine)
                            .accessibilityLabel("\(title) on \(e.deviceName)")
                        }
                    }
                }

                if !model.activeJobs.isEmpty {
                    Section("Active jobs") {
                        ForEach(model.activeJobs) { job in
                            VStack(alignment: .leading, spacing: 4) {
                                Text("\(job.jobType) · \(job.state)")
                                    .font(.headline)
                                if job.progress > 0 {
                                    ProgressView(value: Double(job.progress), total: 100)
                                        .accessibilityLabel("\(job.jobType) progress")
                                        .accessibilityValue("\(job.progress) percent")
                                }
                            }
                        }
                    }
                }

                Section("Devices") {
                    if model.devices.isEmpty {
                        Text("No devices registered.")
                            .foregroundStyle(.secondary)
                    } else {
                        ForEach(model.devices) { d in
                            let title = d.name.isEmpty ? d.udid : d.name
                            HStack {
                                VStack(alignment: .leading) {
                                    Text(title)
                                        .font(.headline)
                                    Text("\(d.platform) · \(d.osVersion)")
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                Spacer()
                                StatusDot(kind: d.isPaired ? .ok : .idle)
                            }
                            .accessibilityElement(children: .combine)
                            .accessibilityLabel("\(title), \(d.isPaired ? "paired" : "not paired")")
                        }
                    }
                }
            }
        }
    }
}
