//  The transfer sheet — mIRC's DCC panel: one row per transfer, a bar that
//  fills, and the two buttons that matter on an offer someone made you.

import SwiftUI

struct TransfersView: View {
    @EnvironmentObject private var store: Store
    @Environment(\.theme) private var t
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("file transfers").font(Face.mono(13, .heavy)).foregroundStyle(t.text)
                Spacer()
                Button("[close]") { dismiss() }
                    .accessibilityLabel("close")
                    .font(Face.mono(13)).foregroundStyle(t.muted)
            }
            if store.snap.transfers.isEmpty {
                Text("nothing yet. /send <nick> offers a file to someone nearby.")
                    .font(Face.mono(13)).foregroundStyle(t.muted).padding(.vertical, 20)
            } else {
                ScrollView {
                    VStack(spacing: 10) {
                        ForEach(store.snap.transfers.reversed()) { Row($0) }
                    }
                }
            }
            Spacer()
            Text("peer to peer over the local network · nothing is uploaded anywhere")
                .font(Face.mono(10.5)).foregroundStyle(t.muted)
                .frame(maxWidth: .infinity)
        }
        .padding(14)
        .background(t.surface)
    }

    private func status(_ x: Transfer) -> String {
        switch x.state {
        case "offered": return x.dir == "recv" ? "offers you" : "waiting for accept"
        case "running": return "\(Int(x.progress * 100))%"
        case "done": return x.dir == "recv" ? "received" : "sent"
        default: return x.error ?? x.state
        }
    }

    @ViewBuilder private func Row(_ x: Transfer) -> some View {
        let bad = x.state == "failed" || x.state == "declined"
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Text(x.dir == "send" ? ">>" : "<<")
                    .font(Face.mono(13, .heavy)).foregroundStyle(t.muted)
                Text(x.name).font(Face.mono(13, .bold)).foregroundStyle(t.text).lineLimit(1)
                Spacer()
                Text(size(x.size)).font(Face.mono(11)).foregroundStyle(t.muted)
            }
            Text("\(x.dir == "send" ? "to" : "from") \(x.peer_nick)")
                .font(Face.mono(11)).foregroundStyle(t.muted)
            ProgressView(value: x.progress)
                .tint(bad ? t.accent : t.join)
            HStack {
                Text(status(x)).font(Face.mono(11)).foregroundStyle(bad ? t.accent : t.muted)
                Spacer()
                if x.dir == "recv" && x.state == "offered" {
                    Button("[accept]") { store.accept(x.id) }
                        .accessibilityLabel("accept")
                        .font(Face.mono(11.5, .bold)).foregroundStyle(t.join)
                    Button("[decline]") { store.decline(x.id) }
                        .accessibilityLabel("decline")
                        .font(Face.mono(11.5, .bold)).foregroundStyle(t.muted)
                }
                if x.state == "done" && x.dir == "recv" {
                    Text("in Files").font(Face.mono(11.5, .bold)).foregroundStyle(t.link)
                }
            }
        }
        .padding(10)
        .background(t.surface2)
        .overlay(Rectangle().stroke(t.border, lineWidth: 1))
    }

    private func size(_ n: Int) -> String {
        n >= 1048576 ? String(format: "%.1f MB", Double(n) / 1048576)
            : (n >= 1024 ? "\(n / 1024) KB" : "\(n) B")
    }
}
