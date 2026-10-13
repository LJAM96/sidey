import Foundation

/// A decoded `{"columns": [...], "rows": [{...}]}` dashboard table.
struct TableResponse {
    struct Row {
        private let values: [String: Any]

        init(_ values: [String: Any]) {
            self.values = values
        }

        func string(_ key: String) -> String {
            switch values[key] {
            case let s as String: return s
            case let n as NSNumber: return n.stringValue
            case .none: return ""
            case is NSNull: return ""
            case let v?: return String(describing: v)
            }
        }

        func optionalString(_ key: String) -> String? {
            guard let v = values[key], !(v is NSNull) else { return nil }
            let s = string(key)
            return s.isEmpty ? nil : s
        }

        func int(_ key: String) -> Int {
            if let n = values[key] as? NSNumber { return n.intValue }
            return Int(string(key)) ?? 0
        }
    }

    let rows: [Row]

    static func decode(_ data: Data) throws -> TableResponse {
        let obj = try JSONSerialization.jsonObject(with: data)
        guard let dict = obj as? [String: Any],
              let rawRows = dict["rows"] as? [[String: Any]] else {
            throw APIError.decoding("table envelope")
        }
        return TableResponse(rows: rawRows.map(Row.init))
    }
}

struct HealthStatus: Decodable {
    let status: String
}

struct ServerConfig: Decodable {
    let configured: Bool
}

/// One row of the refresh calendar.
struct RefreshEntry: Identifiable, Sendable {
    let id: String // deployment_id
    let deviceName: String
    let udid: String
    let profileExpiry: Date?
    let nextDue: Date?
    let lastResult: String
    let lastError: String
    let jobState: String
    let appName: String
    let bundleID: String
    let signState: String
    let installState: String
    let appleID: String

    var needsAttention: Bool {
        lastResult == "failed" || lastResult == "attention"
    }

    var daysToExpiry: Int? {
        guard let profileExpiry else { return nil }
        return Calendar.current.dateComponents([.day], from: Date(), to: profileExpiry).day
    }

    static func make(from row: TableResponse.Row) -> RefreshEntry {
        RefreshEntry(
            id: row.string("deployment_id"),
            deviceName: row.string("device_name"),
            udid: row.string("udid"),
            profileExpiry: ISODate.parse(row.string("profile_expiry_at")),
            nextDue: ISODate.parse(row.string("next_refresh_due_at")),
            lastResult: row.string("last_refresh_result"),
            lastError: row.string("last_refresh_error"),
            jobState: row.string("refresh_job_state"),
            appName: row.string("app_name"),
            bundleID: row.string("bundle_identifier"),
            signState: row.string("sign_state"),
            installState: row.string("install_state"),
            appleID: row.string("apple_id")
        )
    }
}

struct DeviceItem: Identifiable, Sendable {
    let id: String
    let udid: String
    let platform: String
    let name: String
    let model: String
    let osVersion: String
    let agentName: String
    let pairingStatus: String
    let lastConnected: Date?

    var isPaired: Bool { pairingStatus == "paired" }

    static func make(from row: TableResponse.Row) -> DeviceItem {
        DeviceItem(
            id: row.string("id"),
            udid: row.string("udid"),
            platform: row.string("platform"),
            name: row.string("device_name"),
            model: row.string("model"),
            osVersion: row.string("os_version"),
            agentName: row.string("agent_name"),
            pairingStatus: row.string("pairing_status"),
            lastConnected: ISODate.parse(row.string("last_connected_at"))
        )
    }
}

struct JobItem: Identifiable, Sendable {
    let id: String
    let jobType: String
    let deviceID: String
    let state: String
    let progress: Int
    let errorCategory: String
    let errorDetails: String
    let createdAt: Date?

    var isActive: Bool {
        state == "pending" || state == "claimed" || state == "in_progress"
    }

    var isRecentlyFailedAuth: Bool {
        guard jobType == "sign", state == "failed" || state == "dead" else { return false }
        guard errorCategory == "auth" else { return false }
        guard let createdAt else { return false }
        return Date().timeIntervalSince(createdAt) < 24 * 3600
    }

    static func make(from row: TableResponse.Row) -> JobItem {
        JobItem(
            id: row.string("id"),
            jobType: row.string("job_type"),
            deviceID: row.string("device_id"),
            state: row.string("state"),
            progress: row.int("progress"),
            errorCategory: row.string("error_category"),
            errorDetails: row.string("error_details"),
            createdAt: ISODate.parse(row.string("created_at"))
        )
    }
}

struct DeploymentItem: Identifiable, Sendable, Hashable {
    let id: String
    let deviceID: String
    let target: String
    let desiredVersion: String
    let updatePolicy: String
    let currentState: String
    let currentBundle: String
    let currentVersion: String

    static func make(from row: TableResponse.Row) -> DeploymentItem {
        DeploymentItem(
            id: row.string("id"),
            deviceID: row.string("device_id"),
            target: row.string("target"),
            desiredVersion: row.string("desired_version"),
            updatePolicy: row.string("update_policy"),
            currentState: row.string("current_state"),
            currentBundle: row.string("current_bundle"),
            currentVersion: row.string("current_version")
        )
    }
}

struct StoreVersion: Decodable, Sendable, Hashable {
    let version: String
    let channel: String
    let bundleID: String
    let downloadURL: String
    let size: Int64
    let updatedDate: String

    /// Stable identity for ForEach: position-independent.
    var stableID: String { "\(version)|\(channel)|\(bundleID)" }

    enum CodingKeys: String, CodingKey {
        case version, channel
        case bundleID = "bundle_id"
        case downloadURL = "download_url"
        case size
        case updatedDate = "updated_date"
    }
}

struct StoreApp: Identifiable, Decodable, Sendable, Hashable {
    let id: String
    let name: String
    let developer: String
    let description: String
    let iconURL: String
    let source: String
    let sourceType: String
    let versions: [StoreVersion]
    let bundleID: String
    let version: String
    let channel: String
    let downloadURL: String
    let size: Int64

    enum CodingKeys: String, CodingKey {
        case id, name, developer, description
        case iconURL = "icon_url"
        case source
        case sourceType = "source_type"
        case versions
        case bundleID = "bundle_id"
        case version, channel
        case downloadURL = "download_url"
        case size
    }
}

struct StoreAppsResponse: Decodable {
    let apps: [StoreApp]
}

struct StoreSource: Identifiable, Decodable, Sendable {
    let id: String
    let type: String
    let url: String
    let name: String
}

struct StoreSourcesResponse: Decodable {
    let sources: [StoreSource]
}

struct UpdateEntry: Identifiable, Decodable, Sendable {
    /// Stable identity across refreshes: the same app on two devices, or a
    /// version bump, must never collide or swap row state.
    var id: String { "\(bundleID)|\(deviceID ?? "")|\(deploymentID ?? "")" }
    let kind: String?
    let bundleID: String
    let appName: String?
    let currentVersion: String?
    let latestVersion: String?
    let latestDownloadURL: String?
    let deviceID: String?
    let deploymentID: String?
    let deviceName: String?
    let sourceName: String?

    enum CodingKeys: String, CodingKey {
        case kind
        case bundleID = "bundle_id"
        case appName = "app_name"
        case currentVersion = "current_version"
        case latestVersion = "latest_version"
        case latestDownloadURL = "latest_download_url"
        case deviceID = "device_id"
        case deploymentID = "deployment_id"
        case deviceName = "device_name"
        case sourceName = "source_name"
    }
}

struct UpdatesResponse: Decodable {
    let updates: [UpdateEntry]
}

enum ISODate {
    static func parse(_ s: String) -> Date? {
        guard !s.isEmpty else { return nil }
        // Fresh formatter per call: no shared mutable state (Swift 6 safe),
        // negligible cost at dashboard volumes.
        for options: ISO8601DateFormatter.Options in [
            [.withInternetDateTime, .withFractionalSeconds],
            [.withInternetDateTime],
        ] {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = options
            if let date = formatter.date(from: s) {
                return date
            }
        }
        return nil
    }
}
