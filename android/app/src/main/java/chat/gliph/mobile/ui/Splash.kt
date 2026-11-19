//  Top-of-history watermark: the app icon in block cells, the same grid as
//  web/src/ui.ts and mobile/Sources/Views/Splash.swift. The letters are
//  palette slots, so it comes out right in both themes.

package chat.gliph.mobile.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.unit.dp

private val unicorn = listOf(
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
)

@Composable
fun Splash() {
    val th = t
    val cols = unicorn[0].length
    val rows = unicorn.size
    Box(Modifier.fillMaxWidth().padding(top = 8.dp, bottom = 14.dp), contentAlignment = Alignment.Center) {
        Canvas(Modifier.size(196.dp, (196f / cols * rows).dp)) {
            val cell = size.width / cols
            unicorn.forEachIndexed { y, row ->
                row.forEachIndexed { x, ch ->
                    val c = when (ch) {
                        'k' -> th.border; 'w' -> th.border2; 'p' -> th.accent; 'y' -> th.gold
                        'g' -> th.join; 'b' -> th.link; 'e' -> th.text; else -> null
                    } ?: return@forEachIndexed
                    // A hair of overlap: exact cells leave seams on a non-integral scale.
                    drawRect(c, Offset(x * cell, y * cell), Size(cell + 0.5f, cell + 0.5f))
                }
            }
        }
    }
}
