#!/usr/bin/env bash
# create_worker.sh <worker_id>
# Builds a portable MT5 directory for a new worker by symlinking all
# read-only reference binaries and creating fresh writable directories.
# MT5 /portable mode writes all user data relative to the EXE path, so
# different directories = different IPC pipe names = isolated instances.
set -euo pipefail

WORKER_ID="${1:?Usage: create_worker.sh <worker_id>}"

FLEET_DIR="${FLEET_DIR:-/mt5-fleet}"
REFERENCE_DIR="$FLEET_DIR/reference/install"
WORKER_DIR="$FLEET_DIR/workers/$WORKER_ID"

if [ -d "$WORKER_DIR" ]; then
    echo "[create_worker] Directory already exists: $WORKER_DIR" >&2
    exit 1
fi

if [ ! -d "$REFERENCE_DIR" ]; then
    echo "[create_worker] Reference install not found at $REFERENCE_DIR" >&2
    exit 1
fi

# Directories that MT5 writes into (must NOT be symlinked to the reference):
# profiles, config, logs, MQL5, tester, bases
WRITABLE_DIRS=("MQL5" "logs" "config" "tester" "bases" "profiles")

echo "[create_worker] Creating worker directory: $WORKER_DIR"
mkdir -p "$WORKER_DIR"

# Symlink every file/dir from reference EXCEPT the writable ones
for item in "$REFERENCE_DIR"/*; do
    name="$(basename "$item")"
    skip=false
    for wd in "${WRITABLE_DIRS[@]}"; do
        if [ "$name" = "$wd" ]; then
            skip=true
            break
        fi
    done
    if [ "$skip" = false ]; then
        ln -sfn "$item" "$WORKER_DIR/$name"
    fi
done

# Create fresh writable directories
for wd in "${WRITABLE_DIRS[@]}"; do
    mkdir -p "$WORKER_DIR/$wd"
done

echo "[create_worker] Worker $WORKER_ID ready at $WORKER_DIR"
