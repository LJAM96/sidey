import Foundation
import Testing
@testable import Sidey

private func storeApp(name: String, bundle: String, source: String) -> StoreApp {
    StoreApp(id: bundle, name: name, developer: "dev", description: "",
             iconURL: "", source: source, sourceType: "altstore", versions: [],
             bundleID: bundle, version: "1.0", channel: "stable",
             downloadURL: "", size: 0)
}

@Suite @MainActor struct BrowseFilterTests {
    @Test func emptyQueryMatchesSourceFilter() {
        let model = BrowseModel()
        model.apps = [storeApp(name: "Delta", bundle: "com.delta", source: "Accretion")]
        model.sourceFilter = "Accretion"
        #expect(model.filtered.count == 1)
    }

    @Test func queryMatchesNameCaseInsensitively() {
        let model = BrowseModel()
        model.apps = [storeApp(name: "Delta", bundle: "com.delta", source: "Accretion")]
        model.query = "delta"
        #expect(model.filtered.count == 1)
        model.query = "xyz"
        #expect(model.filtered.isEmpty)
    }

    @Test func queryMatchesBundleID() {
        let model = BrowseModel()
        model.apps = [storeApp(name: "Delta", bundle: "com.delta.emu", source: "Accretion")]
        model.query = "delta.emu"
        #expect(model.filtered.count == 1)
    }

    @Test func sourceFilterRejectsOthers() {
        let model = BrowseModel()
        model.apps = [storeApp(name: "Delta", bundle: "com.delta", source: "Other")]
        model.sourceFilter = "Accretion"
        #expect(model.filtered.isEmpty)
    }
}

@Suite @MainActor struct HomeLogicTests {
    @Test func attentionAndExpiringClassification() {
        let soon = RefreshEntry(id: "a", deviceName: "P", udid: "u",
                                profileExpiry: Date.now.addingTimeInterval(3 * 86400),
                                nextDue: nil, lastResult: "failed", lastError: "",
                                jobState: "", appName: "A", bundleID: "com.a",
                                signState: "", installState: "", appleID: "")
        let later = RefreshEntry(id: "b", deviceName: "P", udid: "u",
                                 profileExpiry: Date.now.addingTimeInterval(30 * 86400),
                                 nextDue: nil, lastResult: "ok", lastError: "",
                                 jobState: "", appName: "B", bundleID: "com.b",
                                 signState: "", installState: "", appleID: "")
        let (expiring, attention) = HomeModel.classify([later, soon])
        #expect(expiring.map { $0.id } == ["a"])
        #expect(attention.map { $0.id } == ["a"])
    }
}
