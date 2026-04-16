#!/usr/bin/env bash
# entrypoint.sh — container startup.
# MT5 + Python are baked into the image; just launch supervisord.
set -e

exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
