//  Mobile push (spec F5).
//
//  The server's half of this is deliberately useless on its own: the wake it
//  sends carries no body, no nick and no channel — see server/push.go and
//  docs/protocol.md. So this file's job is not to display anything the push
//  said. It is to hand the OS token to the server, and to make sure that when
//  the OS wakes us the core re-syncs, decrypts with this device's own key, and
//  produces the notification locally.
//
//  Everything the user reads was written on this phone. That is the whole
//  design, and it is why the payload is empty.
//
//  ponytail: no push SDK. Registration is four lines of UIApplication; a
//  Firebase dependency would add an account, a config file and a second
//  analytics pipeline to do the same thing on iOS.

import Foundation
import UIKit
import UserNotifications

enum Push {
    private static var registered = ""

    /// Ask for alerts (iOS asks once; after that the answer lives in Settings)
    /// and register for the APNs token either way. A background wake needs no
    /// permission, so someone who said no to banners still gets a phone that
    /// re-syncs while asleep, and the banners come back the day they flip the
    /// switch in Settings › Notifications › Gliph.
    @MainActor
    static func register(_ store: Store) async {
        guard !store.token.isEmpty else { return }
        let centre = UNUserNotificationCenter.current()
        let granted = (try? await centre.requestAuthorization(options: [.alert, .badge, .sound]))
            ?? false
        NSLog("push: alerts \(granted ? "allowed" : "not allowed"), registering for wakes")
        UIApplication.shared.registerForRemoteNotifications()
    }

    /// Called from the app delegate with the OS token. Safe on every launch:
    /// the server upserts by token, so a rotation replaces the row.
    @MainActor
    static func send(_ store: Store, token: String) async {
        guard !token.isEmpty, token != registered, !store.token.isEmpty else { return }
        guard let url = store.api("/api/push/devices") else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(store.token)", forHTTPHeaderField: "Authorization")
        req.httpBody = try? JSONSerialization.data(
            withJSONObject: ["platform": "ios", "token": token])
        let status = (try? await URLSession.shared.data(for: req))
            .flatMap { $0.1 as? HTTPURLResponse }?.statusCode ?? 0
        NSLog("push: token handed to the server: HTTP \(status)")
        if status == 200 { registered = token }
    }

    /// Hand the token back on sign-out. Without this the account keeps being
    /// woken on a phone that is no longer signed into it.
    @MainActor
    static func unregister(_ store: Store) async {
        guard !registered.isEmpty, !store.token.isEmpty,
              let url = store.api("/api/push/devices") else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "DELETE"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(store.token)", forHTTPHeaderField: "Authorization")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["token": registered])
        registered = ""
        _ = try? await URLSession.shared.data(for: req)
    }
}
