import Foundation
import Testing
@testable import Sidey

private func stubSession(handler: @escaping (URLRequest) -> (HTTPURLResponse, Data)) -> URLSession {
    DiscoveryStubProtocol.handler = handler
    let config = URLSessionConfiguration.ephemeral
    config.protocolClasses = [DiscoveryStubProtocol.self]
    return URLSession(configuration: config)
}

/// Own interceptor (NOT the shared StubURLProtocol): DiscoveryTests runs
/// parallel to APIClientTests, so a separate handler global is required.
final class DiscoveryStubProtocol: URLProtocol {
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

private func stubResponse(_ object: Any, status: Int = 200) -> (HTTPURLResponse, Data) {
    let data = try! JSONSerialization.data(withJSONObject: object)
    let resp = HTTPURLResponse(url: URL(string: "https://sidey.test/")!, statusCode: status,
                               httpVersion: nil, headerFields: nil)!
    return (resp, data)
}

private func txtData(_ dict: [String: Data]) -> Data {
    NetService.data(fromTXTRecord: dict)
}

/// Shares StubURLProtocol.handler with APIClientTests, hence serialized.
@Suite(.serialized) struct DiscoveryTests {
    // MARK: - TXT records

    @Test func txtParsesIDAndName() {
        let data = txtData(["id": Data("srv-1".utf8), "name": Data("Garage".utf8)])
        let (id, name) = DiscoveryParse.txt(data)
        #expect(id == "srv-1")
        #expect(name == "Garage")
    }

    @Test func txtMissingKeysAreNil() {
        let data = txtData(["name": Data("Garage".utf8)])
        let (id, name) = DiscoveryParse.txt(data)
        #expect(id == nil)
        #expect(name == "Garage")
        #expect(DiscoveryParse.txt(nil) == (nil, nil))
        #expect(DiscoveryParse.txt(Data()) == (nil, nil))
    }

    @Test func txtGarbageIsNil() {
        #expect(DiscoveryParse.txt(Data([0xFF, 0x00, 0x13])) == (nil, nil))
    }

    // MARK: - Identity

    @Test func identityToleratesUnknownFields() {
        let json = #"{"id":"srv-1","name":"Garage","version":1,"future":"x","nested":{"a":1}}"#
        let identity = DiscoveryParse.identity(Data(json.utf8))
        #expect(identity?.id == "srv-1")
        #expect(identity?.name == "Garage")
        #expect(identity?.version == 1)
    }

    @Test func identityMissingIDStaysNil() {
        let identity = DiscoveryParse.identity(Data(#"{"name":"Garage"}"#.utf8))
        #expect(identity?.id == nil)
        #expect(identity?.name == "Garage")
    }

    @Test func identityInvalidJSONIsNil() {
        #expect(DiscoveryParse.identity(Data("not json".utf8)) == nil)
    }

    // MARK: - Candidates

    @Test func candidatesDedupeAndDefaultPort() {
        let urls = DiscoveryParse.candidates(
            rememberedHosts: ["192.168.1.5:8080", "garage.local"],
            savedURL: URL(string: "http://192.168.1.5:8080")
        )
        #expect(urls.count == 2)
        #expect(urls[0].absoluteString == "http://192.168.1.5:8080")
        #expect(urls[1].absoluteString == "http://garage.local:8080")
    }

    @Test func candidatesEmptyWithoutHosts() {
        #expect(DiscoveryParse.candidates(rememberedHosts: [], savedURL: nil).isEmpty)
    }

    // MARK: - Known servers

    @Test func knownNoteDedupesAndCaps() {
        var k = KnownServers()
        k.note(id: "srv-1", name: "Garage", host: "192.168.1.5", port: 8080)
        k.note(id: "srv-1", name: "Garage", host: "100.1.2.3", port: 8080)
        k.note(id: "srv-1", name: "Garage", host: "192.168.1.5", port: 8080)
        let hosts = k.hosts(forKey: "srv-1")
        #expect(hosts == ["192.168.1.5:8080", "100.1.2.3:8080"])
        #expect(k.isKnown(key: "srv-1"))
        #expect(!k.isKnown(key: "srv-9"))
    }

    @Test func knownLegacyKeyFallsBackToHost() {
        #expect(KnownServers.key(id: nil, host: "192.168.1.5:8080") == "legacy:192.168.1.5:8080")
        #expect(KnownServers.key(id: "", host: "X") == "legacy:x")
        #expect(KnownServers.key(id: "srv-1", host: "X") == "srv-1")
    }

    @Test func knownEncodeRoundTrip() {
        var k = KnownServers()
        k.note(id: "srv-1", name: "Garage", host: "192.168.1.5", port: 8080)
        let decoded = KnownServers.decode(k.encode())
        #expect(decoded.hosts(forKey: "srv-1") == ["192.168.1.5:8080"])
        #expect(KnownServers.decode(nil).servers.isEmpty)
        #expect(KnownServers.decode("garbage").servers.isEmpty)
    }

    // MARK: - Enrollment

    @Test func enrollReturnsToken() async throws {
        let session = stubSession { _ in
            stubResponse(["token": "tok-123", "device_id": "d-1"])
        }
        let (token, deviceID) = try await APIClient.enroll(
            baseURL: URL(string: "http://192.168.1.5:8080")!,
            deviceName: "Test iPhone", appVersion: "1.0", session: session
        )
        #expect(token == "tok-123")
        #expect(deviceID == "d-1")
    }

    @Test func enrollRefusedMaps() async {
        let session = stubSession { _ in stubResponse(["error": "denied"], status: 403) }
        do {
            _ = try await APIClient.enroll(
                baseURL: URL(string: "http://192.168.1.5:8080")!,
                deviceName: "x", appVersion: "1.0", session: session
            )
            Issue.record("expected refusal")
        } catch APIError.http(let code, _) {
            #expect(code == 403)
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    @Test func enrollLegacy404Maps() async {
        let session = stubSession { _ in stubResponse(["error": "nope"], status: 404) }
        do {
            _ = try await APIClient.enroll(
                baseURL: URL(string: "http://192.168.1.5:8080")!,
                deviceName: "x", appVersion: "1.0", session: session
            )
            Issue.record("expected 404")
        } catch APIError.http(let code, _) {
            #expect(code == 404)
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    @Test func enrollBadJSONDecodingFails() async {
        let session = stubSession { _ in stubResponse(["unexpected": true]) }
        do {
            _ = try await APIClient.enroll(
                baseURL: URL(string: "http://192.168.1.5:8080")!,
                deviceName: "x", appVersion: "1.0", session: session
            )
            Issue.record("expected decoding failure")
        } catch APIError.decoding {
        } catch {
            Issue.record("wrong error: \(error)")
        }
    }

    // MARK: - Host probing

    private func probeSession() -> URLSession {
        stubSession { req in
            switch req.url?.path {
            case "/api/v1/server-identity":
                return stubResponse(["id": "srv-1", "name": "Garage", "version": 1])
            case "/healthz":
                return stubResponse(["status": "ok"])
            default:
                return stubResponse(["error": "nope"], status: 404)
            }
        }
    }

    @Test func probeMatchesKnownID() async {
        let hit = await DiscoveryService.probeHosts(
            hosts: ["192.168.9.9:8080"], session: probeSession(), wantID: "srv-1"
        )
        #expect(hit?.serverID == "srv-1")
        #expect(hit?.host == "192.168.9.9")
        #expect(hit?.source == .candidate)
    }

    @Test func probeRejectsWrongID() async {
        let hit = await DiscoveryService.probeHosts(
            hosts: ["192.168.9.9:8080"], session: probeSession(), wantID: "srv-other"
        )
        #expect(hit == nil)
    }

    @Test func probeUnreachableIsNil() async {
        let session = stubSession { _ in stubResponse(["error": "x"], status: 404) }
        let hit = await DiscoveryService.probeHosts(
            hosts: ["192.168.9.9:8080"], session: session, wantID: nil
        )
        #expect(hit == nil)
    }
}
