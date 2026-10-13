import SwiftUI

@main
struct SideyApp: App {
    @State private var session = SessionStore()
    @State private var discovery = DiscoveryService()
    @State private var selection = 0

    init() {
        UserDefaults.standard.register(defaults: [ExpiryReminders.enabledKey: true])
    }

    var body: some Scene {
        WindowGroup {
            Group {
                if session.isPaired {
                    TabView(selection: $selection) {
                        Tab("Home", systemImage: "house", value: 0) {
                            HomeView()
                        }
                        Tab("My Apps", systemImage: "apps.iphone", value: 1) {
                            MyAppsView()
                        }
                        Tab("Browse", systemImage: "magnifyingglass", value: 2) {
                            BrowseView()
                        }
                        Tab("Updates", systemImage: "arrow.triangle.2.circlepath", value: 3) {
                            UpdatesView()
                        }
                        Tab("Settings", systemImage: "gear", value: 4) {
                            SettingsView()
                        }
                    }
                    .tabBarMinimizeBehavior(.onScrollDown)
                } else {
                    NavigationStack {
                        PairingView()
                    }
                }
            }
            .environment(session)
            .environment(discovery)
            .task {
                // Launch-time reachability: paired phones re-verify the
                // saved server and silently heal to a remembered address
                // when the server moved (DHCP / tailnet IP change).
                // Unpaired phones browse from PairingView instead.
                if session.isPaired {
                    await session.checkConnection()
                    if case .offline = session.connection {
                        _ = await session.heal()
                    }
                }
            }
        }
    }
}
