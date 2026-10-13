import Foundation
import Observation

/// Pairing session: server URL + device token, persisted in Keychain.
@Observable
@MainActor
final class SessionStore {
    enum Connection {
        case unknown
        case checking
        case online
        case offline(String)
    }

    var serverURLString: String = ""
    var connection: Connection = .unknown
    /// Stable ID of the paired server (nil for legacy servers predating
    /// server-identity). Silent re-attach only ever targets this ID.
    var serverID: String?

    /// Cached credentials: Keychain I/O must not run on every body-path read.
    private var cachedToken: String?
    private var probeGen = 0
    private var knownCache: KnownServers?

    var baseURL: URL? {
        var s = serverURLString.trimmingCharacters(in: .whitespacesAndNewlines)
        if !s.isEmpty, !s.contains("://") { s = "http://" + s }
        return URL(string: s)
    }

    var token: String? { cachedToken }
    var isPaired: Bool { baseURL != nil && !(cachedToken ?? "").isEmpty }

    init() {
        serverURLString = KeychainStore.loadServerURL() ?? ""
        cachedToken = KeychainStore.loadToken()
        serverID = KeychainStore.loadServerID()
    }

    func client() throws -> APIClient {
        guard let baseURL, let token, !token.isEmpty else {
            throw APIError.notPaired
        }
        return APIClient(baseURL: baseURL, token: token)
    }

    func save(urlString: String, token: String) {
        let cleanURL = urlString.trimmingCharacters(in: .whitespacesAndNewlines)
        let cleanToken = token.trimmingCharacters(in: .whitespacesAndNewlines)
        KeychainStore.saveServerURL(cleanURL)
        KeychainStore.saveToken(cleanToken)
        serverURLString = cleanURL
        cachedToken = cleanToken.isEmpty ? cachedToken : cleanToken
    }

    func forget() {
        KeychainStore.clear()
        serverURLString = ""
        cachedToken = nil
        serverID = nil
        connection = .unknown
    }

    /// Switch to a new URL for the same pairing (auto-heal). The token is
    /// kept; the server ID is re-verified by the next checkConnection.
    func rehome(to urlString: String) {
        let clean = urlString.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !clean.isEmpty else { return }
        KeychainStore.saveServerURL(clean)
        serverURLString = clean
    }

    // MARK: - Known servers (Phase 6)

    private var known: KnownServers {
        get {
            if knownCache == nil {
                knownCache = KnownServers.decode(KeychainStore.loadKnownServers())
            }
            return knownCache ?? KnownServers()
        }
        set {
            knownCache = newValue
            KeychainStore.saveKnownServers(newValue.encode())
        }
    }

    /// Hosts remembered for the current pairing (ID-pinned, or legacy key).
    var currentKnownHosts: [String] {
        known.hosts(forKey: currentKnownKey)
    }

    /// Every remembered host (drives Nearby re-probe on tailnets).
    var allKnownHosts: [String] {
        var out: [String] = []
        for entry in known.servers.values {
            out.append(contentsOf: entry.hosts)
        }
        return out
    }

    func isKnownServer(id: String?) -> Bool {
        guard let id, !id.isEmpty else { return false }
        return known.isKnown(key: id)
    }

    /// Key under which the current pairing's hosts are remembered.
    var currentKnownKey: String {
        if let host = baseURL?.host {
            let port = baseURL?.port ?? 80
            let hostPort = port == 80 ? host : "\(host):\(port)"
            return KnownServers.key(id: serverID, host: hostPort)
        }
        return KnownServers.key(id: serverID, host: "")
    }

    /// Record a successful contact (every online check refreshes it).
    /// A non-nil fetched ID always wins; a nil fetch (legacy server)
    /// leaves the existing ID untouched.
    func recordServer(id: String?, name: String?, host: String, port: Int) {
        if let id, !id.isEmpty {
            serverID = id
            KeychainStore.saveServerID(id)
        }
        var k = known
        k.note(id: serverID, name: name ?? host, host: host, port: port)
        known = k
    }

    /// Paired but offline: probe remembered hosts and silently re-attach
    /// only on a server-ID match (or any healthy remembered host for
    /// legacy pairings — the host was good before). Returns online state.
    func heal() async -> Bool {
        let hosts = currentKnownHosts
        guard !hosts.isEmpty, baseURL != nil else { return false }
        guard let hit = await DiscoveryService.probeHosts(
            hosts: hosts, session: .shared, wantID: serverID
        ), let url = hit.url else { return false }
        rehome(to: url.absoluteString)
        await checkConnection()
        if case .online = connection { return true }
        return false
    }

    /// Probe reachability, then validate the token against an authenticated
    /// endpoint. Stale probes (rapid re-taps) are discarded by generation.
    func checkConnection() async {
        probeGen += 1
        let gen = probeGen
        guard let baseURL else {
            connection = .offline("No server configured.")
            return
        }
        connection = .checking
        guard await APIClient.probe(baseURL: baseURL) else {
            guard gen == probeGen else { return }
            connection = .offline("Server unreachable. Check the URL (same tailnet?) and try again.")
            return
        }
        do {
            let client = try client()
            _ = try await client.devices()
            guard gen == probeGen else { return }
            connection = .online
            // Cheap identity refresh: pins the server ID and remembers
            // this host (best-effort; legacy servers simply have no ID).
            if let host = baseURL.host {
                let identity = await DiscoveryService.fetchIdentity(url: baseURL, session: .shared)
                guard gen == probeGen else { return }
                recordServer(
                    id: identity?.id, name: identity?.name,
                    host: host, port: baseURL.port ?? 80
                )
            }
        } catch APIError.unauthorized {
            guard gen == probeGen else { return }
            connection = .offline("Token rejected — re-pair in Settings.")
        } catch {
            guard gen == probeGen else { return }
            connection = .offline("Server reachable, but the request failed: \((error as? LocalizedError)?.errorDescription ?? error.localizedDescription)")
        }
    }
}
