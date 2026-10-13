import Foundation
import Security

/// Keychain storage for the pairing secret and server URL. Nothing
/// credential-like belongs in UserDefaults.
enum KeychainStore {
    private static let service = "com.sidey.companion"
    private static let tokenAccount = "api-token"
    private static let urlAccount = "server-url"
    private static let serverIDAccount = "server-id"
    private static let knownServersAccount = "known-servers"

    @discardableResult
    static func saveToken(_ token: String) -> Bool {
        save(token, account: tokenAccount)
    }

    static func loadToken() -> String? {
        load(account: tokenAccount)
    }

    @discardableResult
    static func saveServerURL(_ url: String) -> Bool {
        save(url, account: urlAccount)
    }

    static func loadServerURL() -> String? {
        load(account: urlAccount)
    }

    /// Stable server ID from server-identity (nil until first seen).
    /// Kept alongside known hosts so the app only ever silently
    /// re-attaches to a server it has paired with before.
    static func loadServerID() -> String? {
        load(account: serverIDAccount)
    }

    static func saveServerID(_ id: String?) {
        guard let id, !id.isEmpty else {
            delete(account: serverIDAccount)
            return
        }
        save(id, account: serverIDAccount)
    }

    /// Known-servers JSON (see KnownServers). Retained across Forget so a
    /// re-pair stays one tap; it holds hosts, never secrets.
    static func loadKnownServers() -> String? {
        load(account: knownServersAccount)
    }

    @discardableResult
    static func saveKnownServers(_ json: String) -> Bool {
        save(json, account: knownServersAccount)
    }

    static func clear() {
        delete(account: tokenAccount)
        delete(account: urlAccount)
        delete(account: serverIDAccount)
    }

    // MARK: - Private

    private static func save(_ value: String, account: String) -> Bool {
        guard let data = value.data(using: .utf8) else { return false }
        delete(account: account)
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        return SecItemAdd(query as CFDictionary, nil) == errSecSuccess
    }

    private static func load(account: String) -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess,
              let data = result as? Data else {
            return nil
        }
        return String(data: data, encoding: .utf8)
    }

    private static func delete(account: String) {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        SecItemDelete(query as CFDictionary)
    }
}
