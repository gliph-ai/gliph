//  Gliph for iOS. A native SwiftUI client over the shared Rust core.
//
//  There is no webview here and no second implementation of anything: the
//  protocol, the crypto, the sync and the offline cache are core/, the same
//  crate the web client runs as wasm. What is in this target is the parts of a
//  client that are genuinely per-platform — the socket, the Keychain, Bonjour,
//  notifications, and the pixels.

import SwiftUI
import UserNotifications

@main
struct GliphApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var store = Store()
    @Environment(\.scenePhase) private var phase
    /// The code from a gliph://approve/<code> link, until it is confirmed.
    @State private var approveCode: String?

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(store)
                .environment(\.theme, store.theme)
                .preferredColorScheme(store.theme.isDark ? .dark : .light)
                // A new device's QR code, scanned with the Camera. Never
                // approves on its own: the person confirms which device.
                .onOpenURL { url in
                    if url.scheme == "gliph", url.host == "approve" {
                        approveCode = url.lastPathComponent
                    }
                }
                .alert(approveTitle, isPresented: Binding(
                    get: { approveCode != nil && store.signedIn && store.snap.device.known },
                    set: { if !$0 { approveCode = nil } })
                ) {
                    if approveWaiting, let code = approveCode {
                        Button("Approve") { store.submit("/approve \(code)") }
                    }
                    Button(approveWaiting ? "Cancel" : "OK", role: .cancel) {}
                } message: {
                    Text(approveMessage)
                }
                .task {
                    delegate.store = store
                    if store.signedIn {
                        store.connect()
                        await Push.register(store)
                    }
                }
                .onChange(of: phase) { new in
                    // Coming back after iOS suspended the socket: the only
                    // honest assumption is that we missed something.
                    store.foreground = new == .active
                    if new == .active, store.signedIn {
                        store.wake()
                        // Cheap when nothing changed (send dedupes by token),
                        // and the only way a token minted after launch, or a
                        // permission flipped in Settings, reaches the server.
                        Task { await Push.register(store) }
                    }
                }
        }
    }
}

extension GliphApp {
    /// The scanned code is a device of this account waiting on the server
    /// this app is on now. Approve is offered only then.
    private var approveWaiting: Bool {
        store.snap.device.signed && store.snap.device.pending.contains { $0.code == approveCode }
    }
    private var approveTitle: String {
        let name = store.snap.device.pending.first { $0.code == approveCode }?.name ?? ""
        return approveWaiting ? "Approve \(name.isEmpty ? "the new device" : name)?" : "Cannot approve"
    }
    private var approveMessage: String {
        let code = approveCode ?? ""
        if !store.snap.device.signed { return "This device is not approved itself, so it cannot approve another." }
        if !approveWaiting {
            return "No device of yours on \(store.snap.server_name) is waiting with \(code). Switch to the server the new device signed in to, then scan again."
        }
        return "It can then read your encrypted channels. Approve it only if it is yours and shows \(code)."
    }
}

struct RootView: View {
    @EnvironmentObject private var store: Store

    var body: some View {
        if store.signedIn {
            ChatView()
        } else {
            AuthView()
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    weak var store: Store?

    func application(_ app: UIApplication,
                     didFinishLaunchingWithOptions _: [UIApplication.LaunchOptionsKey: Any]? = nil)
        -> Bool
    {
        UNUserNotificationCenter.current().delegate = self
        return true
    }

    func application(_ app: UIApplication,
                     didRegisterForRemoteNotificationsWithDeviceToken token: Data) {
        let hex = token.map { String(format: "%02x", $0) }.joined()
        Task { @MainActor in
            if let store { await Push.send(store, token: hex) }
        }
    }

    func application(_ app: UIApplication,
                     didFailToRegisterForRemoteNotificationsWithError error: Error) {
        NSLog("push registration failed: \(error.localizedDescription)")
    }

    /// The wake. It carries nothing — that is the contract in docs/protocol.md,
    /// because gliph is end-to-end encrypted and the server cannot write a
    /// notification it is able to read. So there is nothing to display here:
    /// wake the core, which re-syncs, decrypts, and writes the real one.
    func application(_ app: UIApplication,
                     didReceiveRemoteNotification _: [AnyHashable: Any]) async
        -> UIBackgroundFetchResult
    {
        await MainActor.run { store?.wake() }
        return .newData
    }

    /// Tapping a notification opens the conversation it came from.
    func userNotificationCenter(_ centre: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse) async {
        let channel = response.notification.request.content.userInfo["channel"] as? String
        await MainActor.run {
            if let channel { store?.openFromNotification(channel) }
        }
    }
}
