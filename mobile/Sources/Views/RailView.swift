//  The rail: channels, nearby channels, DMs, then who is in the room. On a
//  phone it is a drawer that slides in from the left under the channel name
//  (see ChatView), so switching channel keeps the conversation on screen
//  behind it instead of replacing it with a screen of lists.

import SwiftUI

struct RailView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Binding var open: Bool

    private var channels: [Channel] { store.snap.channels.filter { !$0.is_dm && !$0.nearby } }
    private var nearby: [Channel] { store.snap.channels.filter(\.nearby) }
    private var dms: [Channel] { store.snap.channels.filter(\.is_dm) }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                Title("Channels")
                ForEach(channels) { Row($0) }
                if channels.isEmpty { Hint("none yet") }

                if store.snap.nearby_on {
                    Title("Nearby", badge: "\(store.snap.nearby_peers) peers")
                    ForEach(nearby) { Row($0) }
                    if nearby.isEmpty { Hint("/nearby #name to open one") }
                }
                if !dms.isEmpty {
                    Title("Direct")
                    ForEach(dms) { Row($0) }
                }
                if let ch = store.channel() {
                    Title("Members", badge: "\(ch.members.count)")
                    ForEach(ch.members) { m in
                        HStack(spacing: 7) {
                            Text(m.sigil).font(Face.mono(13, .bold)).foregroundStyle(sigilColor(m))
                            // Offline regulars stay listed, greyed, IRC style.
                            Text(m.nick).font(Face.mono(13)).foregroundStyle(m.online || m.kind != "user" ? t.text2 : t.muted)
                            Spacer()
                            if m.unverified {
                                Text("unapproved device").font(Face.mono(10.5, .bold)).foregroundStyle(t.accent)
                            }
                            if m.away != nil {
                                Text("away").font(Face.mono(10.5)).foregroundStyle(t.muted)
                            }
                        }
                        .padding(.horizontal, 14).padding(.vertical, 3)
                    }
                }

                Text("\(store.snap.server_name.isEmpty ? "gliph" : store.snap.server_name)\n"
                     + "\(store.snap.command_count) commands · "
                     + "\(store.snap.plugin_command_count) from plugins")
                    .font(Face.mono(11)).foregroundStyle(t.muted)
                    .padding(.horizontal, 14).padding(.top, 16)
            }
            .padding(.vertical, 20)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(t.rail)
    }

    private func sigilColor(_ m: Member) -> Color {
        if m.kind != "user" { return t.agent }
        return m.role == "op" ? t.accent : (m.role == "voice" ? t.join : t.border)
    }

    @ViewBuilder private func Title(_ s: String, badge: String? = nil) -> some View {
        HStack(spacing: 8) {
            Text(s).font(Face.mono(11, .heavy)).foregroundStyle(t.muted)
            if let badge { Text(badge).font(Face.mono(10.5)).foregroundStyle(t.muted) }
        }
        .padding(.horizontal, 14).padding(.top, 14).padding(.bottom, 4)
    }

    @ViewBuilder private func Hint(_ s: String) -> some View {
        Text(s).font(Face.mono(11)).foregroundStyle(t.muted).padding(.horizontal, 14)
    }

    @ViewBuilder private func Row(_ ch: Channel) -> some View {
        let selected = ch.id == store.snap.active
        Button {
            store.setActive(ch.id)
            open = false
        } label: {
            HStack(spacing: 7) {
                Text(ch.is_dm ? "•" : "#")
                    .font(Face.mono(13))
                    // A DM's dot is the other person's presence.
                    .foregroundStyle(ch.is_dm
                        ? (ch.members.first { $0.nick == ch.name }?.online == true ? t.join : t.muted)
                        : (ch.nearby ? t.agent : t.muted))
                Text(ch.name)
                    .font(Face.mono(13, selected ? .bold : .medium))
                    .foregroundStyle(selected ? t.text : t.text2)
                    .lineLimit(1)
                Spacer(minLength: 0)
                if ch.encrypted {
                    Text("e2e").font(Face.mono(10)).foregroundStyle(t.muted)
                }
                if ch.unread > 0 {
                    Text("\(ch.unread)")
                        .font(Face.mono(10, .bold)).foregroundStyle(.white)
                        .padding(.horizontal, 5).padding(.vertical, 1)
                        .background(t.accent)
                }
            }
            // A channel row is a thumb target, so it gets the 44pt Apple asks
            // for rather than the height its 13pt text happens to need.
            .padding(.horizontal, 14).padding(.vertical, 11)
            .frame(maxWidth: .infinity, minHeight: 44, alignment: .leading)
            .background(selected ? t.surface2 : .clear)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }
}
