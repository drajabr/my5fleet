#!/usr/bin/env bash
# start_worker.sh <worker_id> <rpyc_port> <token>
#
# Spawns — for one worker — a dedicated Xvfb virtual display, the MT5 terminal
# (via Wine), the RPyC server (via Wine Windows Python), an x11vnc VNC server,
# and a websockify WebSocket proxy so the browser noVNC client can connect.
#
# Port mapping (must match manager.go constants):
#   basePort      = 18812   (first RPyC port)
#   vncDisplayBase = 100    (first Xvfb display number)
#   vncRFBBase    = 5900    (first x11vnc RFB port)
#   vncWSLocalBase = 6800   (first websockify container port)
#
# Writes all 5 PIDs to $WORKER_DIR/.pids so stop_worker.sh can clean up.
set -euo pipefail

WORKER_ID="${1:?Usage: start_worker.sh <worker_id> <rpyc_port> <token>}"
RPYC_PORT="${2:?Missing rpyc_port}"
TOKEN="${3:?Missing token}"

FLEET_DIR="${FLEET_DIR:-/mt5-fleet}"
WINEPREFIX="${WINEPREFIX:-$FLEET_DIR/wineprefix}"

WORKER_DIR="$FLEET_DIR/workers/$WORKER_ID"
WIN_PYTHONW="$WINEPREFIX/drive_c/Python311/pythonw.exe"
RPYC_SCRIPT="Z:\\opt\\mt5\\scripts\\worker_rpyc.py"
WIN_MT5_PATH="Z:\\mt5-fleet\\workers\\${WORKER_ID}\\terminal64.exe"

# ── Per-worker display/port numbers ──────────────────────────────────────────
# Must match the constants in manager.go.
BASE_PORT=18812
VNC_DISPLAY_BASE=100
VNC_RFB_BASE=5900
VNC_WS_BASE=6800

PORT_OFFSET=$(( RPYC_PORT - BASE_PORT ))
DISPLAY_NUM=$(( VNC_DISPLAY_BASE + PORT_OFFSET ))
WORKER_DISPLAY=":${DISPLAY_NUM}"
VNC_RFB_PORT=$(( VNC_RFB_BASE + PORT_OFFSET ))
VNC_WS_PORT=$(( VNC_WS_BASE + PORT_OFFSET ))

if [ ! -d "$WORKER_DIR" ]; then
    echo "[start_worker] Worker directory not found: $WORKER_DIR" >&2
    exit 1
fi

export WINEPREFIX
export WINEDEBUG="-all"

# ── 1. Xvfb — per-worker virtual display ─────────────────────────────────────
# Clean up stale lock/socket files from a previous SIGKILL; Xvfb refuses to
# start if they exist and exits silently, breaking the worker's display.
rm -f "/tmp/.X${DISPLAY_NUM}-lock" "/tmp/.X11-unix/X${DISPLAY_NUM}"

echo "[start_worker] Starting Xvfb on display $WORKER_DISPLAY ..."
Xvfb "$WORKER_DISPLAY" -screen 0 1280x800x24 -nolisten tcp -nolisten inet6 &
XVFB_PID=$!
echo "[start_worker] Xvfb PID: $XVFB_PID"

# Wait until the display is ready before launching Wine processes against it.
for i in $(seq 1 10); do
    xdpyinfo -display "$WORKER_DISPLAY" >/dev/null 2>&1 && break
    sleep 0.5
done

# ── 1b. ratpoison — tiling window manager ─────────────────────────────────────
echo "[start_worker] Starting ratpoison on display $WORKER_DISPLAY ..."
DISPLAY="$WORKER_DISPLAY" ratpoison &
RATPoison_PID=$!
echo "[start_worker] ratpoison PID: $RATPoison_PID"

# Give ratpoison a moment to own the X session before sending commands.
sleep 0.5

# Build a deterministic two-pane layout:
# - frame 0 (left) is dedicated to the first/main window
# - frame 1 (right) accepts newly created windows (e.g. MetaEditor)
_rp() {
    DISPLAY="$WORKER_DISPLAY" ratpoison -c "$1" >/dev/null 2>&1 || true
}

_rp "only"
_rp "hsplit"
_rp "fselect 0"
_rp "dedicate 1"
_rp "fselect 1"
_rp "dedicate 0"

# ── 2. MT5 terminal ───────────────────────────────────────────────────────────
export DISPLAY="$WORKER_DISPLAY"

echo "[start_worker] Starting MT5 terminal for $WORKER_ID ..."
wine "$WORKER_DIR/terminal64.exe" /portable \
    1>>"$WORKER_DIR/logs/terminal.stdout.log" \
    2>>"$WORKER_DIR/logs/terminal.stderr.log" &
TERMINAL_PID=$!
echo "[start_worker] MT5 terminal PID: $TERMINAL_PID"

# Give the terminal time to initialise its IPC pipe before the RPyC server
# calls mt5.initialize(). If not ready yet, worker_rpyc.py will retry.
sleep 8

# ── 3. RPyC server ────────────────────────────────────────────────────────────
echo "[start_worker] Starting RPyC server for $WORKER_ID on port $RPYC_PORT ..."
wine "$WIN_PYTHONW" "$RPYC_SCRIPT" \
    --port "$RPYC_PORT" \
    --token="$TOKEN" \
    --mt5-path "$WIN_MT5_PATH" \
    --log-file "Z:\\mt5-fleet\\workers\\${WORKER_ID}\\logs\\rpyc.log" \
    1>/dev/null 2>/dev/null &
RPYC_PID=$!
echo "[start_worker] RPyC server PID: $RPYC_PID"

# ── 4. x11vnc — VNC server for this worker's display ─────────────────────────
echo "[start_worker] Starting x11vnc on rfbport $VNC_RFB_PORT ..."
x11vnc \
    -display "$WORKER_DISPLAY" \
    -rfbport "$VNC_RFB_PORT" \
    -nopw -forever -shared \
    -noxdamage -q \
    -o "$WORKER_DIR/logs/x11vnc.log" &
X11VNC_PID=$!
echo "[start_worker] x11vnc PID: $X11VNC_PID"

# ── 5. websockify — WebSocket → VNC proxy ─────────────────────────────────────
echo "[start_worker] Starting websockify on port $VNC_WS_PORT → $VNC_RFB_PORT ..."
websockify "0.0.0.0:${VNC_WS_PORT}" "localhost:${VNC_RFB_PORT}" &
WSOCKIFY_PID=$!
echo "[start_worker] websockify PID: $WSOCKIFY_PID"

# ── Persist all 5 PIDs ────────────────────────────────────────────────────────
printf "%d %d %d %d %d" \
    "$TERMINAL_PID" "$RPYC_PID" "$XVFB_PID" "$X11VNC_PID" "$WSOCKIFY_PID" \
    > "$WORKER_DIR/.pids"

echo "[start_worker] Worker $WORKER_ID started."
echo "  terminal=$TERMINAL_PID rpyc=$RPYC_PID xvfb=$XVFB_PID x11vnc=$X11VNC_PID wsockify=$WSOCKIFY_PID"
