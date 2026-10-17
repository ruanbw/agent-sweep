#!/bin/sh
# 重新生成 iconset 与 .icns。
# 依赖：rsvg-convert（brew install librsvg）与 macOS 自带的 iconutil。
set -eu
cd "$(dirname "$0")"

command -v rsvg-convert >/dev/null 2>&1 || {
  echo "缺少 rsvg-convert（brew install librsvg）" >&2
  exit 1
}

MASTER="AgentSweep.svg"
SMALL="AgentSweep-small.svg"
OUT="../icon.iconset"

rm -rf "$OUT"
mkdir -p "$OUT"

# 小尺寸单独画：把手更粗、扇面更宽、刷毛缝隙更少、蓝点更大，保证 16/32px 不糊。
rsvg-convert -w 16   -h 16   "$SMALL"  -o "$OUT/icon_16x16.png"
rsvg-convert -w 32   -h 32   "$SMALL"  -o "$OUT/icon_16x16@2x.png"
rsvg-convert -w 32   -h 32   "$SMALL"  -o "$OUT/icon_32x32.png"
rsvg-convert -w 64   -h 64   "$MASTER" -o "$OUT/icon_32x32@2x.png"
rsvg-convert -w 128  -h 128  "$MASTER" -o "$OUT/icon_128x128.png"
rsvg-convert -w 256  -h 256  "$MASTER" -o "$OUT/icon_128x128@2x.png"
rsvg-convert -w 256  -h 256  "$MASTER" -o "$OUT/icon_256x256.png"
rsvg-convert -w 512  -h 512  "$MASTER" -o "$OUT/icon_256x256@2x.png"
rsvg-convert -w 512  -h 512  "$MASTER" -o "$OUT/icon_512x512.png"
rsvg-convert -w 1024 -h 1024 "$MASTER" -o "$OUT/icon_512x512@2x.png"

cp "$OUT/icon_512x512@2x.png" ../icon-1024.png
iconutil -c icns "$OUT" -o ../AgentSweep.icns
echo "  build/icon.iconset  build/AgentSweep.icns  build/icon-1024.png"
