#!/usr/bin/env bash
# Renders cmd/btcvm/og/og.html to cmd/btcvm/web/og.png (1200x630), the
# social preview image, with headless Chrome.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}

"$CHROME" --headless=new --disable-gpu --hide-scrollbars \
  --window-size=1200,630 --virtual-time-budget=5000 \
  --screenshot="$ROOT/cmd/btcvm/web/og.png" \
  "file://$ROOT/cmd/btcvm/og/og.html" 2>/dev/null
echo "wrote cmd/btcvm/web/og.png"
