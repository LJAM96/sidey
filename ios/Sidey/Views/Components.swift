import SwiftUI
#if canImport(UIKit)
import UIKit
#endif

struct StatusDot: View {
    enum Kind { case ok, warn, bad, idle }
    let kind: Kind

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 10, height: 10)
            .accessibilityHidden(true)
    }

    private var color: Color {
        switch kind {
        case .ok: return .green
        case .warn: return .orange
        case .bad: return .red
        case .idle: return .gray
        }
    }
}

struct ExpiryBadge: View {
    let days: Int?

    var body: some View {
        Group {
            if let days {
                Text(days < 0 ? "expired" : days == 0 ? "today" : "\(days)d")
                    .font(.caption2.bold())
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(days <= 2 ? Color.red.opacity(0.15) : Color.green.opacity(0.15))
                    .foregroundStyle(days <= 2 ? .red : .green)
                    .clipShape(.capsule)
                    .accessibilityLabel(days < 0 ? "Profile expired" : days == 0 ? "Profile expires today" : "Profile expires in \(days) days")
            } else {
                Text("—")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .accessibilityLabel("Expiry unknown")
            }
        }
    }
}

#if canImport(UIKit)
import UIKit

/// Spoken confirmation for async outcomes that would otherwise be
/// visual- or haptic-only. Safe to call from any actor.
func announce(_ message: String) {
    DispatchQueue.main.async {
        UIAccessibility.post(notification: .announcement, argument: message)
    }
}
#endif

struct EmptyState: View {
    let title: String
    let systemImage: String
    let detail: String?

    var body: some View {
        ContentUnavailableView(title, systemImage: systemImage, description: detail.map(Text.init))
    }
}

struct InlineError: View {
    let message: String
    var retry: (() -> Void)?

    var body: some View {
        ContentUnavailableView {
            Label("Something went wrong", systemImage: "exclamationmark.triangle")
        } description: {
            Text(message)
        } actions: {
            if let retry {
                Button("Try Again", action: retry)
                    .buttonStyle(.borderedProminent)
            }
        }
    }
}

extension Date {
    var shortString: String {
        formatted(date: .abbreviated, time: .omitted)
    }
}
