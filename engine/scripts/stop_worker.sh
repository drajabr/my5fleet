#!/usr/bin/env bash
# stop_worker.sh <worker_id>
#
# Gracefully stops all 5 processes for a worker: RPyC server, MT5 terminal,
# x11vnc, websockify, and Xvfb.  Also removes the Xvfb lock/socket files so
# a subsequent start_worker.sh can reclaim the same display number.
set -euo pipefail

WORKER_ID="${1:?Usage: stop_worker.sh <worker_id>}"

FLEET_DIR="${FLEET_DIR:-/mt5-fleet}"
WORKER_DIR="$FLEET_DIR/workers/$WORKER_ID"
PIDS_FILE="$WORKER_DIR/.pids"

if [ ! -f "$PIDS_FILE" ]; then
    echo "[stop_worker] No .pids file found for $WORKER_ID – already stopped?"
    exit 0
fi

# .pids format: TERMINAL_PID RPYC_PID XVFB_PID X11VNC_PID WSOCKIFY_PID
read -r TERMINAL_PID RPYC_PID XVFB_PID X11VNC_PID WSOCKIFY_PID \
    < "$PIDS_FILE" || true

_kill_pid() {
    local pid="$1"
    local label="$2"
    if [ -z "$pid" ] || [ "$pid" -le 0 ] 2>/dev/null; then
        return
    fi
    if kill -0 "$pid" 2>/dev/null; then
        echo "[stop_worker] Sending SIGTERM to $label (PID $pid) ..."
        kill -TERM "$pid" 2>/dev/null || true
        for _ in $(seq 1 8); do
            sleep 1
            kill -0 "$pid" 2>/dev/null || return 0
        done
        echo "[stop_worker] Force-killing $label (PID $pid) ..."
        kill -KILL "$pid" 2>/dev/null || true
    else
        echo "[stop_worker] $label (PID $pid) already gone."
    fi
}

# Stop in reverse dependency order: RPyC first (no new API calls), then
# terminal, then VNC stack, then display last.
_kill_pid "$RPYC_PID"     "rpyc-server"
_kill_pid "$TERMINAL_PID" "mt5-terminal"
_kill_pid "$WSOCKIFY_PID" "websockify"
_kill_pid "$X11VNC_PID"   "x11vnc"
_kill_pid "$XVFB_PID"     "xvfb"

# Read the display number from the .pids file's companion information.
# The display number is encoded in the Xvfb process's command-line args; we
# can also derive it from the worker directory name + base constants, but the
# simplest approach is to clean up whichever lock files x11vnc might have left.
# Derive display number the same way start_worker.sh does:
#   portOffset = RPYC_PORT - BASE_PORT; displayNum = VNC_DISPLAY_BASE + offset
# We don't have RPYC_PORT here, so clean all potential lock files for this dir.
# In practice only one display number is used per worker; we scan for it.
for lockfile in /tmp/.X*-lock; do
    [ -f "$lockfile" ] || continue
    display_num="${lockfile#/tmp/.X}"
    display_num="${display_num%-lock}"
    # Only remove if the locked PID matches our Xvfb PID
    if [ -n "$XVFB_PID" ] && [ "$XVFB_PID" -gt 0 ] 2>/dev/null; then
        locked_pid=$(cat "$lockfile" 2>/dev/null | tr -d '[:space:]') || continue
        if [ "$locked_pid" = "$XVFB_PID" ]; then
            echo "[stop_worker] Removing stale Xvfb lock for display :${display_num}"
            rm -f "$lockfile" "/tmp/.X11-unix/X${display_num}"
        fi
    fi
done

rm -f "$PIDS_FILE"
echo "[stop_worker] Worker $WORKER_ID stopped."
