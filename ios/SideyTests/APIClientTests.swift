import Foundation
import Testing
@testable import Sidey

/// Intercepts requests for APIClient tests. Serial test execution makes the
/// shared handler safe; `nonisolated(unsafe)` satisfies Swift 6.
final class StubURLProtocol: URLProtocol {
    nonisolated(unsafe) static var handler: ((URLRequest) -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let handler = Self.handler else { return }
        let (response, data) = handler(request)
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

private func stubClient(handler: @escaping (URLRequest) -> (HTTPURLResponse, Data)) -> APIClient {
    StubURLProtocol.handler = handler
    let config = URLSessionConfiguration.ephemeral
    config.protocolClasses = [StubURLProtocol.self]
    return APIClient(
        baseURL: URL(string: "https://sidey.test")!,
        token: "test-token",
        session: URLSession(configuration: config)
    )
}

private func jsonResponse(_ object: Any, status: Int = 200) -> (HTTPURLResponse, Data) {
    let data = try! JSONSerialization.data(withJSONObject: object)
    let resp = HTTPURLResponse(url: URL(string: "https://sidey.test/")!, statusCode: status,
                               httpVersion: nil, headerFields: nil)!
    return (resp, data)
}

@Suite(.serialized) struct APIClientTests {
    @Test func unauthorizedMapsOnGet() async {
        let client = stubClient { _ in jsonResponse(["error": "nope"], status: 401) }
        do {
            _ = try await client.devices()
            Issue.record("expected unauthorized")
        } catch APIError.unauthorized {
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    @Test func unauthorizedMapsOnPost() async {
        let client = stubClient { _ in jsonResponse(["error": "nope"], status: 401) }
        do {
            try await client.triggerRefresh(deploymentID: "d")
            Issue.record("expected unauthorized")
        } catch APIError.unauthorized {
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    @Test func serverErrorExtractsMessage() async {
        let client = stubClient { _ in jsonResponse(["error": "quota exhausted"], status: 429) }
        do {
            _ = try await client.devices()
            Issue.record("expected http error")
        } catch APIError.http(let code, let msg) {
            #expect(code == 429)
            #expect(msg == "quota exhausted")
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    @Test func healthTrueAndFalse() async throws {
        let ok = stubClient { _ in jsonResponse(["status": "ok"]) }
        #expect(try await ok.health())
        let bad = stubClient { _ in jsonResponse(["status": "down"]) }
        #expect(!(try await bad.health()))
    }

    @Test func devicesDecodeFromRows() async throws {
        let client = stubClient { _ in jsonResponse([
            "columns": ["id", "device_name", "pairing_status"],
            "rows": [["id": "d1", "device_name": "Phone", "pairing_status": "paired"]],
        ]) }
        let devices = try! await client.devices()
        #expect(devices.count == 1)
        #expect(devices[0].name == "Phone")
        #expect(devices[0].isPaired)
    }

    @Test func importReturnsMessageAndID() async throws {
        let client = stubClient { _ in jsonResponse([
            "artifact_id": "a1", "message": "Into quarantine.",
        ], status: 201) }
        let result = try! await client.importApp(name: "X", downloadURL: "https://x/y.ipa",
                                                 deviceID: "d", mode: "livecontainer")
        #expect(result.message == "Into quarantine.")
        #expect(result.artifactID == "a1")
    }

    @Test func importFallsBackWithoutMessage() async throws {
        let client = stubClient { _ in jsonResponse(["ok": true], status: 201) }
        let result = try! await client.importApp(name: "X", downloadURL: "https://x/y.ipa",
                                                 deviceID: "d", mode: "native")
        #expect(result.message == "Imported into quarantine.")
        #expect(result.artifactID == nil)
    }

    @Test func uploadEncodesFilenameAndReturnsID() async throws {
        var seenURL: URL?
        let client = stubClient { req in
            seenURL = req.url
            return jsonResponse(["id": "up1"])
        }
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("t.ipa")
        try! Data([0, 1, 2]).write(to: file)
        defer { try? FileManager.default.removeItem(at: file) }
        let id = try! await client.uploadIPA(filename: "My App.ipa", fileURL: file)
        #expect(id == "up1")
        #expect(seenURL?.query?.contains("filename=My%20App.ipa") == true)
    }
}
