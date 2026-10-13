import Foundation
import Observation

/// Bonjour service type advertised by Sidey servers (Phase 6 contract).
enum DiscoveryConstants {
    static let serviceType = "_sidey._tcp"
    static let defaultPort = 8080
    static let probeTimeout: TimeInterval = 5
    static let maxHostsPerServer = 8
}

/// A Sidey server seen on the network or remembered from a past pairing.
/// `serverID` is nil for legacy servers predating `server-identity`.
struct FoundServer: Sendable, Identifiable, Hashable {
    let serverID: String?
    let name: String
    let host: String
    let port: Int
    let source: Source
    let lastSeen: Date

    enum Source: String, Sendable { case bonjour, remembered, candidate }

    var id: String { "\(host):\(port):\(serverID ?? "-")" }

    var url: URL? {
        var comps = URLComponents()
        comps.scheme = "http"
        comps.host = host
        comps.port = port
        return comps.url
    }

    var displayHost: String { port == 80 ? host : "\(host):\(port)" }
}

/// Unauthenticated server identity (`GET /api/v1/server-identity`).
/// Unknown fields are ignored per the mobile API contract.
struct ServerIdentity: Decodable, Sendable {
    let id: String?
    let name: String?
    let version: Int?
}

/// One known server: stable key (`id`, or `"legacy:"+host`) plus hosts
/// seen for it. Pure value bookkeeping; Keychain I/O lives in
/// KeychainStore/SessionStore so this stays unit-testable.
struct KnownServerEntry: Codable, Sendable {
    var name: String
    var hosts: [String]
    var lastSeen: Date
}

struct KnownServers: Codable, Sendable {
    var servers: [String: KnownServerEntry] = [:]

    static func key(id: String?, host: String) -> String {
        guard let id, !id.isEmpty else { return "legacy:" + host.lowercased() }
        return id
    }

    mutating func note(id: String?, name: String, host: String, port: Int, date: Date = Date()) {
        let hostPort = port == 80 ? host : "\(host):\(port)"
        let k = Self.key(id: id, host: hostPort)
        var entry = servers[k] ?? KnownServerEntry(name: name, hosts: [], lastSeen: date)
        entry.name = name
        entry.hosts.removeAll { $0.caseInsensitiveCompare(hostPort) == .orderedSame }
        entry.hosts.insert(hostPort, at: 0)
        entry.hosts = Array(entry.hosts.prefix(DiscoveryConstants.maxHostsPerServer))
        entry.lastSeen = date
        servers[k] = entry
    }

    func hosts(forKey key: String) -> [String] { servers[key]?.hosts ?? [] }
    func isKnown(key: String) -> Bool { servers[key] != nil }

    static func decode(_ string: String?) -> KnownServers {
        guard let string, !string.isEmpty,
              let data = string.data(using: .utf8),
              let decoded = try? JSONDecoder().decode(KnownServers.self, from: data)
        else { return KnownServers() }
        return decoded
    }

    func encode() -> String {
        guard let data = try? JSONEncoder().encode(self) else { return "" }
        return String(data: data, encoding: .utf8) ?? ""
    }
}

/// Pure parsing/URL helpers for discovery (unit-tested).
enum DiscoveryParse {
    /// NetService TXT record → (id, name). Values are UTF-8 strings.
    static func txt(_ data: Data?) -> (id: String?, name: String?) {
        guard let data, !data.isEmpty else { return (nil, nil) }
        let dict = NetService.dictionary(fromTXTRecord: data)
        func s(_ k: String) -> String? {
            guard let d = dict[k], !d.isEmpty else { return nil }
            return String(data: d, encoding: .utf8)
        }
        return (s("id"), s("name"))
    }

    static func identity(_ data: Data) -> ServerIdentity? {
        try? JSONDecoder().decode(ServerIdentity.self, from: data)
    }

    /// Probe candidates from remembered `host[:port]` strings plus the
    /// saved URL's host. No subnet scanning: every candidate is a host
    /// the operator or a previous pairing already named.
    static func candidates(
        rememberedHosts: [String],
        savedURL: URL?,
        defaultPort: Int = DiscoveryConstants.defaultPort
    ) -> [URL] {
        var seen = Set<String>()
        var out: [URL] = []
        func add(host: String, port: Int?) {
            var comps = URLComponents()
            comps.scheme = "http"
            comps.host = host
            comps.port = port ?? defaultPort
            guard let url = comps.url else { return }
            if seen.insert(url.absoluteString.lowercased()).inserted {
                out.append(url)
            }
        }
        for h in rememberedHosts {
            if let u = URL(string: "http://" + h), let host = u.host {
                add(host: host, port: u.port)
            }
        }
        if let savedURL, let host = savedURL.host {
            add(host: host, port: savedURL.port)
        }
        return out
    }
}

/// Browsing transport seam (live NetService browse vs stub in tests).
/// The service drives start/stop from the main actor; browse callbacks
/// arrive on that same thread, and mutable state is lock-guarded so
/// nothing crosses an isolation boundary.
protocol DiscoveryTransport: AnyObject {
    func start(onFound: @escaping (FoundServer) -> Void)
    func stop()
}

/// Live Bonjour browse via NetService. Network.framework's NWBrowser is
/// deliberately not used: it does not expose TXT records, which carry
/// the server ID and display name.
final class BonjourTransport: NSObject, DiscoveryTransport {
    private let lock = NSLock()
    private let browser = NetServiceBrowser()
    private var resolving: [NetService] = []
    private var onFound: ((FoundServer) -> Void)?

    func start(onFound: @escaping (FoundServer) -> Void) {
        lock.withLock { self.onFound = onFound }
        browser.delegate = self
        browser.searchForServices(ofType: DiscoveryConstants.serviceType, inDomain: "")
    }

    func stop() {
        lock.withLock { self.onFound = nil }
        browser.stop()
        browser.delegate = nil
        let pending = lock.withLock { resolving }
        for s in pending {
            s.stop()
            s.delegate = nil
        }
        lock.withLock { resolving = [] }
    }

    private func takeFound() -> ((FoundServer) -> Void)? {
        lock.withLock { onFound }
    }

    private func drop(_ service: NetService) {
        service.stop()
        service.delegate = nil
        lock.withLock { resolving.removeAll { $0 === service } }
    }
}

extension BonjourTransport: NetServiceBrowserDelegate {
    func netServiceBrowser(
        _ browser: NetServiceBrowser, didFind service: NetService, moreComing: Bool
    ) {
        service.delegate = self
        lock.withLock { resolving.append(service) }
        service.resolve(withTimeout: DiscoveryConstants.probeTimeout)
    }

    func netServiceBrowser(
        _ browser: NetServiceBrowser, didRemove service: NetService, moreComing: Bool
    ) {
        drop(service)
    }
}

extension BonjourTransport: NetServiceDelegate {
    func netServiceDidResolveAddress(_ sender: NetService) {
        let host = sender.hostName ?? ""
        let (id, name) = DiscoveryParse.txt(sender.txtRecordData())
        let found = FoundServer(
            serverID: id, name: name ?? (host.isEmpty ? "Sidey server" : host),
            host: host, port: Int(sender.port),
            source: .bonjour, lastSeen: Date()
        )
        drop(sender)
        takeFound()?(found)
    }

    func netService(
        _ sender: NetService, didNotResolve errorDict: [String: NSNumber]
    ) {
        drop(sender)
    }
}

/// Canned transport for tests, previews and stub screenshots.
final class StubTransport: DiscoveryTransport {
    var servers: [FoundServer]
    init(servers: [FoundServer] = []) { self.servers = servers }
    func start(onFound: @escaping (FoundServer) -> Void) {
        for s in servers { onFound(s) }
    }
    func stop() {}
}

/// Found-server list plus tailnet candidate probing. UI state lives here
/// (@MainActor); network verification is static so tests stay off the
/// main actor.
@MainActor
@Observable
final class DiscoveryService {
    var found: [FoundServer] = []
    var browsing = false

    /// Injectable seam (stub screenshots / previews).
    var makeTransport: () -> any DiscoveryTransport = { BonjourTransport() }
    private var transport: (any DiscoveryTransport)?

    func startBrowsing() {
        guard transport == nil else { return }
        let t = makeTransport()
        transport = t
        browsing = true
        t.start { [weak self] server in
            Task { @MainActor in self?.ingest(server) }
        }
    }

    func stopBrowsing() {
        transport?.stop()
        transport = nil
        browsing = false
    }

    func ingest(_ server: FoundServer) {
        if let i = found.firstIndex(where: { $0.host == server.host && $0.port == server.port }) {
            found[i] = server
        } else {
            found.append(server)
        }
    }

    /// Re-probe remembered hosts (tailnet path: no mDNS there). Matches
    /// are ingested as `.remembered` so the UI can badge them Known.
    func reprobe(hosts: [String], savedURL: URL?, session: URLSession) async {
        let urls = DiscoveryParse.candidates(rememberedHosts: hosts, savedURL: savedURL)
        await withTaskGroup(of: FoundServer?.self) { group in
            for url in urls {
                group.addTask {
                    await Self.verify(url: url, session: session)
                }
            }
            for await hit in group {
                if let hit { self.ingest(hit) }
            }
        }
    }

    /// Probe hosts for one known server ID (auto-heal). When `wantID` is
    /// nil (legacy pairing) any healthy remembered host is accepted —
    /// the host was good before, so the operator already trusts it.
    nonisolated static func probeHosts(
        hosts: [String], session: URLSession, wantID: String?
    ) async -> FoundServer? {
        let urls = DiscoveryParse.candidates(rememberedHosts: hosts, savedURL: nil)
        return await withTaskGroup(of: FoundServer?.self, returning: FoundServer?.self) { group in
            for url in urls {
                group.addTask {
                    guard let hit = await verify(url: url, session: session) else { return nil }
                    if let wantID, hit.serverID != wantID { return nil }
                    return hit
                }
            }
            for await hit in group {
                if let hit {
                    group.cancelAll()
                    return hit
                }
            }
            return nil
        }
    }

    /// Identity-verified probe of one candidate URL. Legacy servers
    /// (no identity endpoint, healthy `/healthz`) come back with nil ID.
    nonisolated static func verify(url: URL, session: URLSession) async -> FoundServer? {
        if let identity = await fetchIdentity(url: url, session: session) {
            return FoundServer(
                serverID: identity.id, name: identity.name ?? url.host ?? "?",
                host: url.host ?? "", port: url.port ?? DiscoveryConstants.defaultPort,
                source: .candidate, lastSeen: Date()
            )
        }
        guard await fetchLegacyHealth(url: url, session: session) else { return nil }
        return FoundServer(
            serverID: nil, name: url.host ?? "?", host: url.host ?? "",
            port: url.port ?? DiscoveryConstants.defaultPort,
            source: .candidate, lastSeen: Date()
        )
    }

    nonisolated static func fetchIdentity(url: URL, session: URLSession) async -> ServerIdentity? {
        var req = URLRequest(
            url: url.appendingPathComponent("/api/v1/server-identity"),
            timeoutInterval: DiscoveryConstants.probeTimeout
        )
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        do {
            let (data, resp) = try await session.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
            return DiscoveryParse.identity(data)
        } catch {
            return nil
        }
    }

    nonisolated static func fetchLegacyHealth(url: URL, session: URLSession) async -> Bool {
        var req = URLRequest(
            url: url.appendingPathComponent("/healthz"),
            timeoutInterval: DiscoveryConstants.probeTimeout
        )
        do {
            let (data, resp) = try await session.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else { return false }
            return (try? JSONDecoder().decode(HealthStatus.self, from: data))?.status == "ok"
        } catch {
            return false
        }
    }
}
