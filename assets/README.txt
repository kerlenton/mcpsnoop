mcpsnoop — brand assets
=======================

Accent   : signal blue   #1FB6FF
Base     : near-black     #0B0E14
Wordmark : JetBrains Mono, weight 500, lowercase

FILES
-----
SVG (vector, scalable — prefer these)
  mcpsnoop-mark.svg           mark · blue · transparent
  mcpsnoop-mark-white.svg     mark · white (dark / photo backgrounds)
  mcpsnoop-mark-black.svg     mark · near-black (light backgrounds)
  mcpsnoop-icon.svg           app icon · blue mark on #0B0E14 squircle
  mcpsnoop-lockup.svg         mark + wordmark · all blue · transparent
  mcpsnoop-lockup-white.svg   mark + wordmark · all white
  mcpsnoop-lockup-ink.svg     blue mark + near-black wordmark (light bg)

PNG (raster)
  png/mcpsnoop-icon-1024..32.png    app icon · dark squircle
  png/mcpsnoop-mark-1024..256.png   mark · blue · transparent
  png/mcpsnoop-lockup.png           lockup · blue · transparent
  png/mcpsnoop-lockup-on-dark.png   lockup on #0B0E14

NOTES
-----
- Lockup SVGs pull JetBrains Mono via a web @import. For print or offline
  use, outline / convert the wordmark to paths in your editor first.
- Clear space: keep >= one node square on every side of the mark.
- Minimum size: legible to ~24 px; below that the brackets fold into a
  single tap block (still reads as a connection).
- One accent colour only (#1FB6FF). No gradients, shadows or recolouring.
