#!/bin/zsh
# Stop any lingering dlv / __debug_bin processes started by start_metis_debug.sh.
# Safe to run multiple times; silently exits when nothing is running.

set -u

killed=0

# Kill dlv listening on :2345
pids=$(lsof -ti :2345 2>/dev/null || true)
if [ -n "$pids" ]; then
    echo "killing dlv on :2345 → $pids"
    echo "$pids" | xargs kill -9 2>/dev/null || true
    killed=1
fi

# Kill any dlv started against this repo's cmd/metis (in case port was different)
dlv_pids=$(pgrep -f "dlv debug ./cmd/metis" 2>/dev/null || true)
if [ -n "$dlv_pids" ]; then
    echo "killing dlv debug ./cmd/metis → $dlv_pids"
    echo "$dlv_pids" | xargs kill -9 2>/dev/null || true
    killed=1
fi

# Kill orphaned __debug_bin processes from this repo
bin_pids=$(pgrep -f "metis/__debug_bin" 2>/dev/null || true)
if [ -n "$bin_pids" ]; then
    echo "killing orphaned __debug_bin → $bin_pids"
    echo "$bin_pids" | xargs kill -9 2>/dev/null || true
    killed=1
fi

# Kill any debugserver attached to those pids (lldb backend)
dbg_pids=$(pgrep -f "debugserver.*__debug_bin" 2>/dev/null || true)
if [ -n "$dbg_pids" ]; then
    echo "killing debugserver → $dbg_pids"
    echo "$dbg_pids" | xargs kill -9 2>/dev/null || true
    killed=1
fi

if [ "$killed" -eq 0 ]; then
    echo "no metis debug processes running"
else
    echo "done"
fi
