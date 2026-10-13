import Foundation

enum APIError: Error, LocalizedError {
    case notPaired
    case network(URLError)
    case http(Int, String)
    case decoding(String)
    case unauthorized

    var errorDescription: String? {
        switch self {
        case .notPaired: return "Not paired — add your server in Settings."
        case .network(let e): return e.localizedDescription
        case .http(let code, let msg): return "Server error \(code): \(msg)"
        case .decoding(let what): return "Unexpected response (\(what))."
        case .unauthorized: return "Token rejected — re-pair in Settings."
        }
    }
}

/// Thin async client over the Sidey control-plane API. The server is the
/// authority; unknown JSON fields are ignored so the app tolerates
/// server-side additions (mobile API contract).
struct APIClient: Sendable {
    let baseURL: URL
    let token: String
    /// Injectable for tests; defaults to shared.
    let session: URLSession

    init(baseURL: URL, token: String, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.token = token
        self.session = session
    }

    private func get(_ path: String) async throws -> Data {
        guard let url = URL(string: path, relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 30)
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.data(for: req)
        } catch let e as URLError {
            throw APIError.network(e)
        }
        guard let http = resp as? HTTPURLResponse else {
            throw APIError.decoding("response")
        }
        switch http.statusCode {
        case 200..<300: return data
        case 401: throw APIError.unauthorized
        default:
            let msg = (try? JSONSerialization.jsonObject(with: data) as? [String: Any])
                .flatMap { $0["error"] as? String } ?? HTTPURLResponse.localizedString(forStatusCode: http.statusCode)
            throw APIError.http(http.statusCode, msg)
        }
    }

    func health() async throws -> Bool {
        // Health is unauthenticated; use a token-less request.
        guard let url = URL(string: "/healthz", relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        let (data, resp) = try await session.data(from: url)
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else { return false }
        return (try? JSONDecoder().decode(HealthStatus.self, from: data))?.status == "ok"
    }

    func config() async throws -> ServerConfig {
        try JSONDecoder().decode(ServerConfig.self, from: await get("/api/v1/config"))
    }

    func devices() async throws -> [DeviceItem] {
        try TableResponse.decode(await get("/api/v1/dashboard/devices")).rows.map(DeviceItem.make)
    }

    func refreshCalendar() async throws -> [RefreshEntry] {
        try TableResponse.decode(await get("/api/v1/dashboard/refresh")).rows.map(RefreshEntry.make)
    }

    func jobs() async throws -> [JobItem] {
        try TableResponse.decode(await get("/api/v1/dashboard/jobs")).rows.map(JobItem.make)
    }

    private func post(_ path: String, body: [String: Any]?) async throws {
        guard let url = URL(string: path, relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 60)
        req.httpMethod = "POST"
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if let body {
            req.httpBody = try JSONSerialization.data(withJSONObject: body)
        }
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.data(for: req)
        } catch let e as URLError {
            throw APIError.network(e)
        }
        guard let http = resp as? HTTPURLResponse else {
            throw APIError.decoding("response")
        }
        guard 200..<300 ~= http.statusCode else {
            if http.statusCode == 401 { throw APIError.unauthorized }
            let msg = (try? JSONSerialization.jsonObject(with: data) as? [String: Any])
                .flatMap { $0["error"] as? String } ?? HTTPURLResponse.localizedString(forStatusCode: http.statusCode)
            throw APIError.http(http.statusCode, msg)
        }
    }

    func updates() async throws -> [UpdateEntry] {
        try JSONDecoder().decode(UpdatesResponse.self, from: await get("/api/v1/dashboard/updates")).updates
    }

    func deployments() async throws -> [DeploymentItem] {
        try TableResponse.decode(await get("/api/v1/dashboard/deployments")).rows.map(DeploymentItem.make)
    }

    /// Starts a manual refresh. Succeeds when the server accepts the job;
    /// signing/installing progress is observed through `jobs()`.
    func triggerRefresh(deploymentID: String) async throws {
        try await post("/api/v1/admin/refresh", body: ["deployment_id": deploymentID])
    }

    /// Submits an Apple 2FA verification code for a pending sign-in.
    func submit2FA(code: String) async throws {
        try await post("/api/v1/admin/apple-accounts/2fa-code", body: ["two_factor_code": code])
    }

    func storeApps() async throws -> [StoreApp] {
        try JSONDecoder().decode(StoreAppsResponse.self, from: await get("/api/v1/admin/store/apps")).apps
    }

    func storeSources() async throws -> [StoreSource] {
        struct Resp: Decodable { let sources: [StoreSource] }
        return try JSONDecoder().decode(Resp.self, from: await get("/api/v1/admin/store/sources")).sources
    }

    struct ImportResult: Decodable {
        let artifactID: String?
        let message: String?

        enum CodingKeys: String, CodingKey {
            case artifactID = "artifact_id"
            case message
        }
    }

    /// Downloads a catalog IPA into quarantine. Returns the server message
    /// plus the artifact id when known (install only proceeds after
    /// explicit approval elsewhere).
    func importApp(name: String, downloadURL: String, deviceID: String, mode: String) async throws -> (message: String, artifactID: String?) {
        struct Req: Encodable {
            let name: String
            let downloadURL: String
            let deviceID: String
            let mode: String

            enum CodingKeys: String, CodingKey {
                case name
                case downloadURL = "download_url"
                case deviceID = "device_id"
                case mode
            }
        }
        guard let url = URL(string: "/api/v1/admin/store/install", relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 120)
        req.httpMethod = "POST"
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(Req(name: name, downloadURL: downloadURL, deviceID: deviceID, mode: mode))
        let (data, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse, 200..<300 ~= http.statusCode else {
            let code = (resp as? HTTPURLResponse)?.statusCode ?? -1
            if code == 401 { throw APIError.unauthorized }
            throw APIError.http(code, "import failed")
        }
        let decoded = try? JSONDecoder().decode(ImportResult.self, from: data)
        return (decoded?.message ?? "Imported into quarantine.", decoded?.artifactID)
    }

    func approveArtifact(id: String) async throws {
        guard let url = URL(string: "/api/v1/artifacts/\(id)", relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 30)
        req.httpMethod = "PATCH"
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: ["state": "approved"])
        let (_, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse, 200..<300 ~= http.statusCode else {
            throw APIError.http((resp as? HTTPURLResponse)?.statusCode ?? -1, "approve failed")
        }
    }

    func addSubscription(bundleID: String) async throws {
        try await post("/api/v1/admin/store/subscriptions", body: ["bundle_id": bundleID])
    }

    func deleteSubscription(bundleID: String) async throws {
        guard let url = URL(string: "/api/v1/admin/store/subscriptions/\(bundleID)", relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 30)
        req.httpMethod = "DELETE"
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let (_, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse, 200..<300 ~= http.statusCode else {
            throw APIError.http((resp as? HTTPURLResponse)?.statusCode ?? -1, "unwatch failed")
        }
    }

    /// Uploads a local IPA file into quarantine, streaming from disk so
    /// large files never sit wholly in memory. The caller must hold the
    /// security-scoped URL's access for the duration of the call.
    func uploadIPA(filename: String, fileURL: URL) async throws -> String {
        guard var comps = URLComponents(url: baseURL.appendingPathComponent("/api/v1/artifacts"), resolvingAgainstBaseURL: true) else {
            throw APIError.decoding("url")
        }
        comps.queryItems = [URLQueryItem(name: "filename", value: filename)]
        guard let url = comps.url else { throw APIError.decoding("url") }
        var req = URLRequest(url: url, timeoutInterval: 300)
        req.httpMethod = "POST"
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")
        let (respData, resp) = try await session.upload(for: req, fromFile: fileURL)
        guard let http = resp as? HTTPURLResponse, 200..<300 ~= http.statusCode else {
            throw APIError.http((resp as? HTTPURLResponse)?.statusCode ?? -1, "upload failed")
        }
        struct Resp: Decodable {
            let id: String?
            let sha256: String?
        }
        let decoded = try? JSONDecoder().decode(Resp.self, from: respData)
        return decoded?.id ?? decoded?.sha256 ?? "uploaded"
    }

    /// Unauthenticated probe used before saving pairing details.
    static func probe(baseURL: URL) async -> Bool {
        guard let url = URL(string: "/healthz", relativeTo: baseURL) else { return false }
        do {
            let (data, resp) = try await URLSession.shared.data(from: url)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else { return false }
            return (try? JSONDecoder().decode(HealthStatus.self, from: data))?.status == "ok"
        } catch {
            return false
        }
    }

    struct EnrollResponse: Decodable {
        let token: String
        let deviceID: String?

        enum CodingKeys: String, CodingKey {
            case token
            case deviceID = "device_id"
        }
    }

    /// One-tap enrollment (Phase 6). Unauthenticated; the server gates by
    /// source network. Throws `.http(404)` on legacy servers and
    /// `.http(403)` when the server refuses automatic enrollment.
    static func enroll(
        baseURL: URL, deviceName: String, appVersion: String,
        session: URLSession = .shared
    ) async throws -> (token: String, deviceID: String?) {
        guard let url = URL(string: "/api/v1/devices/enroll", relativeTo: baseURL) else {
            throw APIError.decoding("url")
        }
        var req = URLRequest(url: url, timeoutInterval: 30)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: [
            "device_name": deviceName,
            "platform": "ios",
            "app_version": appVersion,
        ])
        let (data, resp): (Data, URLResponse)
        do {
            (data, resp) = try await session.data(for: req)
        } catch let e as URLError {
            throw APIError.network(e)
        }
        guard let http = resp as? HTTPURLResponse else {
            throw APIError.decoding("response")
        }
        guard 200..<300 ~= http.statusCode else {
            if http.statusCode == 401 { throw APIError.unauthorized }
            let msg = (try? JSONSerialization.jsonObject(with: data) as? [String: Any])
                .flatMap { $0["error"] as? String } ?? HTTPURLResponse.localizedString(forStatusCode: http.statusCode)
            throw APIError.http(http.statusCode, msg)
        }
        guard let decoded = try? JSONDecoder().decode(EnrollResponse.self, from: data),
              !decoded.token.isEmpty
        else {
            throw APIError.decoding("enroll")
        }
        return (decoded.token, decoded.deviceID)
    }
}
