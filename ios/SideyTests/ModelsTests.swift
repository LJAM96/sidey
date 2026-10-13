import Foundation
import Testing
@testable import Sidey

private func rowsData(_ rows: [[String: Any]]) throws -> Data {
    try JSONSerialization.data(withJSONObject: ["columns": ["id"], "rows": rows])
}

@Suite struct TableResponseTests {
    @Test func decodesValidEnvelope() throws {
        let table = try TableResponse.decode(try rowsData([["id": "a"], ["id": "b"]]))
        #expect(table.rows.count == 2)
        #expect(table.rows[0].string("id") == "a")
    }
    @Test func missingRowsThrows() {
        let bad = try! JSONSerialization.data(withJSONObject: ["columns": []])
        #expect(throws: APIError.self) {
            try TableResponse.decode(bad)
        }
    }

    @Test func emptyRowsDecodes() throws {
        let table = try TableResponse.decode(try rowsData([]))
        #expect(table.rows.isEmpty)
    }

    @Test func rowStringFromString() {
        #expect(TableResponse.Row(["v": "s"]).string("v") == "s")
    }

    @Test func rowCoercionFromNumber() {
        #expect(TableResponse.Row(["v": 42]).string("v") == "42")
        #expect(TableResponse.Row(["v": 42]).int("v") == 42)
    }

    @Test func rowNullAndMissing() {
        #expect(TableResponse.Row(["v": NSNull()]).string("v") == "")
        #expect(TableResponse.Row(["v": NSNull()]).optionalString("v") == nil)
        #expect(TableResponse.Row([:]).string("v") == "")
        #expect(TableResponse.Row([:]).int("v") == 0)
        #expect(TableResponse.Row([:]).optionalString("v") == nil)
    }
}

@Suite struct EntryModelTests {
    private func refreshRow(result: String) -> TableResponse.Row {
        TableResponse.Row([
            "deployment_id": "dep-1", "device_name": "Phone", "udid": "u",
            "profile_expiry_at": "2026-10-20T00:00:00Z",
            "next_refresh_due_at": "2026-10-18T00:00:00Z",
            "last_refresh_result": result, "last_refresh_error": "",
            "refresh_job_state": "", "app_name": "Apollo",
            "bundle_identifier": "com.example.apollo",
            "sign_state": "", "install_state": "", "apple_id": "",
        ])
    }

    @Test(arguments: ["failed", "attention"])
    func needsAttention(result: String) {
        #expect(RefreshEntry.make(from: refreshRow(result: result)).needsAttention)
    }

    @Test func okNeedsNoAttention() {
        #expect(!RefreshEntry.make(from: refreshRow(result: "ok")).needsAttention)
    }

    @Test func expiringEntryHasDays() {
        let entry = RefreshEntry.make(from: refreshRow(result: "ok"))
        #expect((entry.daysToExpiry ?? -99) >= 0)
    }

    @Test func devicePairing() {
        let paired = DeviceItem.make(from: TableResponse.Row(["pairing_status": "paired"]))
        let other = DeviceItem.make(from: TableResponse.Row(["pairing_status": "unpaired"]))
        #expect(paired.isPaired)
        #expect(!other.isPaired)
    }

    @Test func jobActiveStates() {
        for state in ["pending", "claimed", "in_progress"] {
            #expect(JobItem.make(from: TableResponse.Row(["state": state])).isActive)
        }
        #expect(!JobItem.make(from: TableResponse.Row(["state": "completed"])).isActive)
        #expect(!JobItem.make(from: TableResponse.Row(["state": "dead"])).isActive)
    }

    @Test func recentAuthFailureDetected() {
        let now = ISO8601DateFormatter().string(from: Date())
        let row = TableResponse.Row([
            "job_type": "sign", "state": "failed",
            "error_category": "auth", "created_at": now,
        ])
        #expect(JobItem.make(from: row).isRecentlyFailedAuth)
    }

    @Test func staleAuthFailureIgnored() {
        let row = TableResponse.Row([
            "job_type": "sign", "state": "failed",
            "error_category": "auth", "created_at": "2020-01-01T00:00:00Z",
        ])
        #expect(!JobItem.make(from: row).isRecentlyFailedAuth)
    }

    @Test func nonSignFailureIgnored() {
        let now = ISO8601DateFormatter().string(from: Date())
        let row = TableResponse.Row([
            "job_type": "install", "state": "failed",
            "error_category": "auth", "created_at": now,
        ])
        #expect(!JobItem.make(from: row).isRecentlyFailedAuth)
    }

    @Test func updateIDsAreStableAcrossVersions() {
        let a = UpdateEntry(kind: "deployment", bundleID: "com.x", appName: nil,
                            currentVersion: "1", latestVersion: "2", latestDownloadURL: nil,
                            deviceID: "d1", deploymentID: "dep1", deviceName: nil, sourceName: nil)
        let b = UpdateEntry(kind: "deployment", bundleID: "com.x", appName: nil,
                            currentVersion: "1", latestVersion: "3", latestDownloadURL: nil,
                            deviceID: "d1", deploymentID: "dep1", deviceName: nil, sourceName: nil)
        let c = UpdateEntry(kind: "deployment", bundleID: "com.x", appName: nil,
                            currentVersion: "1", latestVersion: "3", latestDownloadURL: nil,
                            deviceID: "d2", deploymentID: "dep2", deviceName: nil, sourceName: nil)
        #expect(a.id == b.id) // version bump must not churn identity
        #expect(a.id != c.id) // same app on two devices must not collide
    }
}

@Suite struct ISODateTests {
    @Test(arguments: [
        ("2026-10-12T09:00:00.000Z", true),
        ("2026-10-12T09:00:00Z", true),
        ("", false),
        ("not-a-date", false),
    ])
    func parsesVariants(input: String, expected: Bool) {
        #expect((ISODate.parse(input) != nil) == expected)
    }
}
