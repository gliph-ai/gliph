//  Settings: who you are, what you are connected to, and the way out. The
//  chrome key that used to sign you out opens this instead — one tap from
//  every screen to a destructive action is how a phone loses a session by
//  accident.

import SwiftUI
#if canImport(WiFiAware)
import DeviceDiscoveryUI
import WiFiAware
#endif

struct SettingsView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Binding var open: Bool
    @State private var confirmSignOut = false

    private var version: String {
        let b = Bundle.main.infoDictionary
        let v = b?["CFBundleShortVersionString"] as? String ?? "?"
        let n = b?["CFBundleVersion"] as? String ?? "?"
        return "\(v) (\(n))"
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                Title("Signed in")
                Row("nick", store.snap.user?.nick ?? (store.snap.solo ? "nearby only" : "—"))
                if let role = store.snap.user?.role, !role.isEmpty { Row("role", role) }
                Row("server", store.snap.server_name.isEmpty ? store.snap.server : store.snap.server_name)
                Row("connection", store.snap.solo ? "nearby only"
                    : (store.snap.online ? "connected" : "reconnecting"))

                Title("App")
                Row("version", version)
                Row("commands", "\(store.snap.command_count) · \(store.snap.plugin_command_count) from plugins")

                #if canImport(WiFiAware)
                if #available(iOS 26, *), WACapabilities.supportedFeatures.contains(.wifiAware) {
                    Title("Phones nearby")
                    AwarePairing()
                }
                #endif

                Title(store.snap.solo ? "Nearby" : "Session")
                Button {
                    if store.snap.solo {
                        store.submit("/nearby-off")
                        open = false
                    } else {
                        confirmSignOut = true
                    }
                } label: {
                    Text(store.snap.solo ? "leave nearby" : "sign out")
                        .font(Face.mono(13, .bold)).foregroundStyle(t.accent)
                        .padding(.horizontal, 14)
                        .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
                        .background(t.surface2)
                        .overlay(Rectangle().stroke(t.border, lineWidth: 1))
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .padding(.horizontal, 14).padding(.top, 4)

                Text("signing out makes the encrypted history on this device unreadable: "
                     + "it forgets its key as well as its session.")
                    .font(Face.mono(11)).foregroundStyle(t.muted)
                    .padding(.horizontal, 14).padding(.top, 8)
            }
            .padding(.vertical, 20)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(t.rail)
        .confirmationDialog("sign out?", isPresented: $confirmSignOut, titleVisibility: .visible) {
            Button("sign out", role: .destructive) { open = false; store.signOut() }
            Button("stay", role: .cancel) {}
        } message: {
            Text("this device forgets its key as well as its session, so the encrypted history "
                 + "on it becomes unreadable. signing back in re-collects the keys from the server.")
        }
    }

    @ViewBuilder private func Title(_ s: String) -> some View {
        Text(s).font(Face.mono(11, .heavy)).foregroundStyle(t.muted)
            .padding(.horizontal, 14).padding(.top, 16).padding(.bottom, 6)
    }

    @ViewBuilder private func Row(_ label: String, _ value: String) -> some View {
        HStack(spacing: 10) {
            Text(label).font(Face.mono(12)).foregroundStyle(t.muted)
            Spacer(minLength: 12)
            Text(value).font(Face.mono(12.5)).foregroundStyle(t.text2)
                .lineLimit(1).truncationMode(.middle)
        }
        .padding(.horizontal, 14).padding(.vertical, 5)
    }
}

#if canImport(WiFiAware)
/// Wi-Fi Aware pairing (Mesh.swift). Once per pair of phones: one finds, the
/// other is found, the OS sheet does the rest. After that the two reach each
/// other in nearby mode with no shared network.
@available(iOS 26, *)
private struct AwarePairing: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @State private var paired: [String] = []
    private let service = "_gliph._tcp"

    var body: some View {
        if let pub = WAPublishableService.allServices[service],
           let sub = WASubscribableService.allServices[service] {
            VStack(alignment: .leading, spacing: 8) {
                Text("pair two phones once to chat nearby with no Wi-Fi network.")
                    .font(Face.mono(11)).foregroundStyle(t.muted)
                HStack(spacing: 8) {
                    DevicePicker(.wifiAware(.connecting(to: .userSpecifiedDevices, from: sub)),
                                 onSelect: { _ in }) {
                        key("find a phone")
                    } fallback: { EmptyView() }
                    DevicePairingView(.wifiAware(.connecting(to: pub, from: .userSpecifiedDevices))) {
                        key("be found")
                    } fallback: { EmptyView() }
                }
                Text(paired.isEmpty ? "no phones paired yet" : "paired: " + paired.joined(separator: ", "))
                    .font(Face.mono(11)).foregroundStyle(t.text2)
            }
            .padding(.horizontal, 14).padding(.top, 4)
            // The pairing sheet publishes the same service the mesh does.
            .onAppear { store.pauseAware(true) }
            .onDisappear { store.pauseAware(false) }
            .task {
                do {
                    for try await all in WAPairedDevice.allDevices {
                        paired = all.values.map { $0.name ?? $0.pairingInfo?.pairingName ?? "a phone" }.sorted()
                    }
                } catch {}
            }
        }
    }

    private func key(_ s: String) -> some View {
        Text(s).font(Face.mono(13, .bold)).foregroundStyle(t.accent)
            .padding(.horizontal, 14).frame(minHeight: 44)
            .background(t.surface2)
            .overlay(Rectangle().stroke(t.border, lineWidth: 1))
    }
}
#endif
