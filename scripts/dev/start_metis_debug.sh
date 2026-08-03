#!/bin/zsh
# Start metis TUI under dlv, listening on :2345 for GoLand attach.
# TUI renders in THIS terminal; GoLand connects via "metis TUI (GoLand attach)".
#
# Behavior on each run:
#   1. Kill any previous metis debug processes (dlv on :2345, orphaned __debug_bin, debugserver).
#   2. Build ./metis from ./cmd/metis so you're always testing the latest code.
#   3. Install metis to $GOBIN (or $GOPATH/bin) so `metis` on PATH is also fresh.
#   4. Launch dlv headless on :2345 (TUI runs in this terminal; GoLand attaches remotely).

set -e

SCRIPT_DIR="${0:A:h}"
PROJECT_ROOT="$SCRIPT_DIR/../.."
cd "$PROJECT_ROOT"

echo "==> Killing previous metis debug processes (if any)..."
"$SCRIPT_DIR/stop_metis_debug.sh" || true
echo

echo "==> Building metis..."
go build -o ./metis ./cmd/metis
echo "    Build OK: $(ls -lh ./metis | awk '{print $5, $9}')"
echo

echo "==> Installing metis to GOBIN..."
go install ./cmd/metis
GOBIN_DIR="$(go env GOBIN)"
if [ -z "$GOBIN_DIR" ]; then
    GOBIN_DIR="$(go env GOPATH)/bin"
fi
echo "    Installed: $GOBIN_DIR/metis"
echo

echo "==> Starting dlv on :2345 (TUI in this terminal; attach GoLand to localhost:2345)..."
export METIS_NO_TRUST_PROMPT=1
exec dlv debug ./cmd/metis \
    --headless \
    --listen=:2345 \
    --api-version=2 \
    --accept-multiclient \
    --check-go-version=false \
    --output=./__debug_bin \
    -- chat --debug
