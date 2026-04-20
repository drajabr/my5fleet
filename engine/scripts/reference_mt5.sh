#!/bin/sh
# reference_mt5.sh -- waits for the reference install to complete,
# then launches MT5 on the reference display (:99).
# Restarted by supervisord if MT5 exits (e.g. crash or user closes it).

REFERENCE_DIR="${FLEET_DIR:-/my5fleet}/reference/install"
LOCK_FILE="${FLEET_DIR:-/my5fleet}/reference/.installing"
MT5_EXE="$REFERENCE_DIR/terminal64.exe"
export WINEPREFIX="${WINEPREFIX:-${FLEET_DIR:-/my5fleet}/wineprefix}"
export WINEDEBUG="-all"

echo "[reference-mt5] Waiting for reference install to complete..."
while [ ! -f "$MT5_EXE" ]; do
    sleep 5
done
# Also wait for the lock file to disappear (install finalising)
while [ -f "$LOCK_FILE" ]; do
    sleep 2
done

# Ensure Wine X11 driver settings are correct before each startup
# This removes decorations (title bar, borders) and lets bspwm manage the window
echo "[reference-mt5] Applying Wine X11 driver configuration..."
wine reg add "HKEY_CURRENT_USER\\Software\\Wine\\X11 Driver" /v Managed /t REG_SZ /d "Y" /f 2>/dev/null || true
wine reg add "HKEY_CURRENT_USER\\Software\\Wine\\X11 Driver" /v Decorated /t REG_SZ /d "N" /f 2>/dev/null || true
wineserver -w 2>/dev/null || true

echo "[reference-mt5] Starting MT5 on reference display..."
exec wine "$MT5_EXE" /portable
