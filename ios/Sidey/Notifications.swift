import Foundation
import UserNotifications

/// Local expiry reminders. No push infrastructure, no background modes:
/// requests scheduled while the app runs still fire later.
enum ExpiryReminders {
    static let enabledKey = "notifyExpiring"

    static var isEnabled: Bool { UserDefaults.standard.bool(forKey: enabledKey) }

    static func setEnabled(_ enabled: Bool) async {
        UserDefaults.standard.set(enabled, forKey: enabledKey)
        if enabled {
            _ = try? await UNUserNotificationCenter.current()
                .requestAuthorization(options: [.alert, .badge, .sound])
        } else {
            UNUserNotificationCenter.current().removeAllPendingNotificationRequests()
        }
    }

    /// Replace pending reminders with one per expiring deployment.
    static func sync(items: [(id: String, title: String, days: Int)]) async {
        let center = UNUserNotificationCenter.current()
        guard isEnabled else {
            center.removeAllPendingNotificationRequests()
            return
        }
        let settings = await center.notificationSettings()
        guard settings.authorizationStatus == .authorized
                || settings.authorizationStatus == .provisional else { return }
        center.removeAllPendingNotificationRequests()
        for item in items.prefix(10) {
            let timing = item.days < 0 ? "has expired"
                : item.days == 0 ? "expires today"
                : "expires in \(item.days) days"
            let content = UNMutableNotificationContent()
            content.title = "Sidey: profile expiring"
            content.body = "\(item.title) \(timing). Open Sidey to refresh."
            let interval = max(60.0, Double(max(item.days, 0)) * 86_400)
            let request = UNNotificationRequest(
                identifier: "sidey-expiry-\(item.id)",
                content: content,
                trigger: UNTimeIntervalNotificationTrigger(timeInterval: interval, repeats: false)
            )
            try? await center.add(request)
        }
    }
}
