#!/bin/sh
# reference_mt5.sh -- waits for the reference install to complete,
# then launches MT5 on the reference display (:99).
# Restarted by supervisord if MT5 exits (e.g. crash or user closes it).

REFERENCE_DIR="${FLEET_DIR:-/my5fleet}/reference/install"
LOCK_FILE="${FLEET_DIR:-/my5fleet}/reference/.installing"
MT5_EXE="$REFERENCE_DIR/terminal64.exe"

echo "[reference-mt5] Waiting for reference install to complete..."
while [ ! -f "$MT5_EXE" ]; do
    sleep 5
done
# Also wait for the lock file to disappear (install finalising)
while [ -f "$LOCK_FILE" ]; do
    sleep 2
done

echo "[reference-mt5] Starting MT5 on reference display..."
exec wine "$MT5_EXE" /portable
