#!/usr/bin/env bash
# install_reference.sh — installs MT5 + Windows Python into Wine.
# Runs at Docker build time. Starts its own Xvfb for the build.
set -euo pipefail

export FLEET_DIR="${FLEET_DIR:-/my5fleet}"
export WINEPREFIX="${WINEPREFIX:-$FLEET_DIR/wineprefix}"
export WINEDEBUG="-all"
export DISPLAY="${DISPLAY:-:99}"

REFERENCE_DIR="$FLEET_DIR/reference/install"
PYTHON_WIN_VER="3.11.9"
PYTHON_EMBED_URL="https://www.python.org/ftp/python/${PYTHON_WIN_VER}/python-${PYTHON_WIN_VER}-embed-amd64.zip"
MT5_URL="https://download.mql5.com/cdn/web/metaquotes.software.corp/mt5/mt5setup.exe"

# ── 0. Start Xvfb for Wine ───────────────────────────────────────────────────
echo "[install] Starting Xvfb on $DISPLAY ..."
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp &
OWNED_XVFB=$!
for i in $(seq 1 30); do
    xdpyinfo -display "$DISPLAY" >/dev/null 2>&1 && break
    sleep 0.5
done
if ! xdpyinfo -display "$DISPLAY" >/dev/null 2>&1; then
    echo "[install] ERROR: Xvfb never came up on $DISPLAY" >&2
    exit 1
fi
echo "[install] Xvfb ready."

# ── 1. Initialise Wine prefix ─────────────────────────────────────────────────
echo "[install] Initialising Wine prefix at $WINEPREFIX ..."
mkdir -p "$WINEPREFIX"
# WINEDLLOVERRIDES suppresses the "Install Wine Mono" popup dialog
export WINEDLLOVERRIDES="mscoree=d"
wineboot --init
# Give wineserver a brief moment to settle; wineserver -w handles the real sync.
sleep 2
wineserver -w 2>/dev/null || true

# ── 1b. Set Windows 10 mode ──────────────────────────────────────────────────
echo "[install] Setting Wine to Windows 10 mode ..."
wine reg add "HKEY_CURRENT_USER\\Software\\Wine" /v Version /t REG_SZ /d "win10" /f 2>/dev/null || true
wineserver -w 2>/dev/null || true

# ── 1c. Let WM manage Wine windows ────────────────────────────────────────────
# Keep Wine windows WM-managed so the window manager controls placement,
# resizing and decorations policy.
echo "[install] Configuring Wine X11 driver (Managed=Y) ..."
wine reg add "HKEY_CURRENT_USER\\Software\\Wine\\X11 Driver" \
    /v Managed /t REG_SZ /d "Y" /f 2>/dev/null || true
wineserver -w 2>/dev/null || true

# ── 2. Install MetaTrader 5 ───────────────────────────────────────────────────
echo "[install] Downloading MT5 installer ..."
wget -q -O /tmp/mt5setup.exe "$MT5_URL"
echo "[install] MT5 installer downloaded."

echo "[install] Running MT5 installer ..."
wine /tmp/mt5setup.exe /auto &
MT5_INSTALL_PID=$!

# Poll for terminal64.exe anywhere under drive_c — Wine versions differ on install
# path ("Program Files" vs "Program Files (x86)" vs user AppData).
echo "[install] Waiting for terminal64.exe to appear (up to 15 min) ..."
MT5_FOUND=""
for i in $(seq 1 450); do
    MT5_FOUND=$(find "$WINEPREFIX/drive_c" -name "terminal64.exe" -type f 2>/dev/null | head -1)
    if [ -n "$MT5_FOUND" ]; then
        echo "[install] terminal64.exe found at: $MT5_FOUND after ~$((i * 2)) seconds."
        break
    fi

    # Every 30 s check if the installer already exited
    if [ $((i % 15)) -eq 0 ]; then
        if ! kill -0 "$MT5_INSTALL_PID" 2>/dev/null; then
            echo "[install] Installer exited; doing final scan ..." >&2
            MT5_FOUND=$(find "$WINEPREFIX/drive_c" -name "terminal64.exe" -type f 2>/dev/null | head -1)
            if [ -n "$MT5_FOUND" ]; then
                echo "[install] terminal64.exe found at: $MT5_FOUND"
                break
            fi
            echo "[install] ERROR: installer exited but terminal64.exe never appeared." >&2
            echo "[install] drive_c/Program Files:" >&2
            ls -la "$WINEPREFIX/drive_c/Program Files/" 2>/dev/null >&2 || true
            ls -la "$WINEPREFIX/drive_c/Program Files (x86)/" 2>/dev/null >&2 || true
            exit 1
        fi
    fi
    echo "[install] ... still waiting ($((i * 2))s) ..."
    sleep 2
done

if [ -z "$MT5_FOUND" ]; then
    echo "[install] ERROR: terminal64.exe not found after 15 minutes." >&2
    echo "[install] drive_c/Program Files:" >&2
    ls -la "$WINEPREFIX/drive_c/Program Files/" 2>/dev/null >&2 || true
    ls -la "$WINEPREFIX/drive_c/Program Files (x86)/" 2>/dev/null >&2 || true
    kill "$MT5_INSTALL_PID" 2>/dev/null || true
    exit 1
fi

MT5_WIN_PATH=$(dirname "$MT5_FOUND")
# Kill the installer process now that we have the binary
kill "$MT5_INSTALL_PID" 2>/dev/null || true
sleep 2
echo "[install] MT5 installed at: $MT5_WIN_PATH"

# Copy binaries to our reference location so they are never under the wineprefix
# (which is shared state). Terminals symlink back into here.
echo "[install] Copying MT5 binaries to reference directory ..."
mkdir -p "$REFERENCE_DIR"
cp -r "$MT5_WIN_PATH/." "$REFERENCE_DIR/"
echo "[install] Reference directory: $REFERENCE_DIR"

# ── 3. Install Windows Python (embeddable zip — avoids 32-bit bootstrapper) ──
# The standard python-amd64.exe has a 32-bit wrapper that fails under wine64.
# The embeddable zip contains only 64-bit PE binaries; just unzip into place.
echo "[install] Downloading Python $PYTHON_WIN_VER embeddable package ..."
wget -q -O /tmp/python-embed.zip "$PYTHON_EMBED_URL"

WIN_PYTHON_DIR="$WINEPREFIX/drive_c/Python311"
rm -rf "$WIN_PYTHON_DIR"
mkdir -p "$WIN_PYTHON_DIR"
echo "[install] Extracting Python embeddable package ..."
unzip -qo /tmp/python-embed.zip -d "$WIN_PYTHON_DIR"

# Embeddable Python disables site-packages by default.
# Uncomment 'import site' in the ._pth file so pip can install packages.
sed -i 's/#import site/import site/' "$WIN_PYTHON_DIR/python311._pth"

WIN_PYTHON="$WIN_PYTHON_DIR/python.exe"
if [ ! -f "$WIN_PYTHON" ]; then
    echo "[install] ERROR: python.exe not found after extraction" >&2
    exit 1
fi
echo "[install] Windows Python extracted."

# ── 4. Install Python packages (host-side wheel download → unzip into Wine) ──
# Wine64 python.exe crashes at startup with OSError WinError 6 (Invalid handle)
# because Wine's console-handle emulation is incomplete.  Workaround: download
# Windows wheels using the host Linux Python3 (present via supervisor dep), then
# unzip them directly into the Wine Python site-packages — no Wine needed.
echo "[install] Bootstrapping pip on host Python ..."
python3 -m ensurepip --upgrade 2>/dev/null || true

SITE_PACKAGES="$WIN_PYTHON_DIR/Lib/site-packages"
mkdir -p "$SITE_PACKAGES" /tmp/wheels

echo "[install] Downloading Windows wheels for MetaTrader5 + rpyc + numpy ..."
python3 -m pip download \
    --quiet \
    --only-binary=:all: \
    --platform=win_amd64 \
    --python-version=311 \
    --implementation=cp \
    -d /tmp/wheels \
    MetaTrader5 rpyc numpy

echo "[install] Extracting wheels into Wine Python site-packages ..."
for whl in /tmp/wheels/*.whl; do
    echo "[install] Installing: $(basename "$whl")"
    unzip -q "$whl" -d "$SITE_PACKAGES"
done
echo "[install] Python packages installed."

# ── 5. Clean up ───────────────────────────────────────────────────────────────
rm -f /tmp/mt5setup.exe /tmp/python-embed.zip
rm -rf /tmp/wheels
# Kill Wine background services so the Docker layer stays clean
wineserver -k 2>/dev/null || true
# Stop Xvfb
kill "$OWNED_XVFB" 2>/dev/null || true
echo "[install] Done. Reference install complete."
