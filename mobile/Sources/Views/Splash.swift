//  Top-of-history watermark: the app icon, drawn in block characters.
//
//  Same grid as web/src/ui.ts, so the top of a channel looks the same on a
//  phone as it does in a browser. The letters are palette slots, not text:
//  k outline, w coat, p/y/g/b the mane, e the eye — which is what lets it come
//  out right in both themes instead of being a picture of one of them.

import SwiftUI

private let unicorn = [
    "..........................k.",
    ".........................kyk",
    "........................kwyk",
    ".......................kwyk.",
    "......................kyyk..",
    "..........kkkkk.k.kk.kywyk..",
    "........kkpppppkwkppkywyk...",
    ".......kppppppkwwkpkywyk....",
    "......kpppyyykwwwkkywyyk....",
    ".....kpppyyggkwwwkkyyyk.k...",
    "....kpppyyggbkwwwwwkykpkpk..",
    "....kppyyggbkwwwwwwwkkpppk..",
    "...kpppyggbbkwwwwwwwwwkpk...",
    "...kppyygbbkwwwwwwewwwwk....",
    "..kppgyygbbkwwwwwwkkwwwk....",
    "..kppgyygbbkwwwwwwwwwwwwk...",
    ".kpppgyyggbkwkwwwwwwwwwwwk..",
    ".kpppgyyggbkwkwwwwwwwwwwwwk.",
    ".kpppggyyggbkwkwwwwwwwwwwwwk",
    ".kpppggyyyggkwwkwwwwwwwwwkwk",
    "..kpppggyyygbkwwkkkkkwwwwwwk",
    "..kpppyggyygg.kwwk...kkwwwk.",
    "...kpppyggyggbkwwk.....kkk..",
    "....kppygggygb.kwwk.........",
    ".k..kppyyggggb.kwwk.........",
    "kpkkpppyyggbgb.kwwwk........",
    "kppppppyygbbgbkwwwwk........",
    "kpppppyyggbgb.kwwwwwk.......",
    ".kpppyyggbbb.kwwwwwwk.......",
    "..kkygggbbb.kwwwwwwwk.......",
    "....k.bbbb.kkkkkkkkkk.......",
    ".....k....k.................",
    "......kkkk..................",
]

struct Splash: View {
    @Environment(\.theme) private var t

    /// Drawn rather than typed. The web version gets square cells from a
    /// monospace font at line-height 1; SwiftUI's line height is roughly twice
    /// the advance width, so a grid of `█` glyphs comes out stretched to two to
    /// one and the unicorn is visibly wrong. A Canvas has no font metrics to
    /// fight, and it is one draw call instead of 33 concatenated Texts.
    var body: some View {
        let cols = unicorn[0].count
        let rows = unicorn.count
        Canvas { ctx, size in
            let cell = min(size.width / CGFloat(cols), size.height / CGFloat(rows))
            let originX = (size.width - cell * CGFloat(cols)) / 2
            for (y, row) in unicorn.enumerated() {
                for (x, ch) in row.enumerated() where ch != "." {
                    let rect = CGRect(x: originX + CGFloat(x) * cell, y: CGFloat(y) * cell,
                                      // A hair of overlap: exact cells leave
                                      // hairline seams on a non-integral scale.
                                      width: cell + 0.5, height: cell + 0.5)
                    ctx.fill(Path(rect), with: .color(coat(ch)))
                }
            }
        }
        .frame(width: 196, height: 196 / CGFloat(cols) * CGFloat(rows))
        .frame(maxWidth: .infinity)
        .padding(.bottom, 14)
        .accessibilityHidden(true)
    }

    private func coat(_ c: Character) -> Color {
        switch c {
        case "k": return t.border
        case "w": return t.border2
        case "p": return t.accent
        case "y": return t.gold
        case "g": return t.join
        case "b": return t.link
        case "e": return t.text
        default: return .clear // '.' — the paper shows through
        }
    }
}
