# ── Stage 1a: build the Go control API ────────────────────────────────────────
FROM golang:1.22-alpine AS go-builder
WORKDIR /build
COPY control_api/go.mod .
COPY control_api/*.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -trimpath -o control-api .

# ── Stage 2: Wine + Xvfb runtime (Debian + WineHQ stable) ────────────────────
FROM debian:bookworm-slim

# ── System packages + WineHQ stable ──────────────────────────────────────────
# Using WineHQ stable on Debian (same approach as gmag11/MetaTrader5-Docker)
# avoids the anti-debugger detection issues that Alpine's community Wine has.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        wget gnupg2 ca-certificates \
        xvfb x11-utils tigervnc-standalone-server \
        supervisor procps unzip python3 python3-pip bash twm \
        xfonts-base xfonts-75dpi \
    && mkdir -pm755 /etc/apt/keyrings \
    && wget -O /etc/apt/keyrings/winehq-archive.key https://dl.winehq.org/wine-builds/winehq.key \
    && wget -NP /etc/apt/sources.list.d/ https://dl.winehq.org/wine-builds/debian/dists/bookworm/winehq-bookworm.sources \
    && dpkg --add-architecture i386 \
    && apt-get update \
    && apt-get install --install-recommends -y winehq-staging \
    && pip3 install --no-cache-dir --break-system-packages websockify \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# ── Directory layout ──────────────────────────────────────────────────────────
RUN mkdir -p \
        /my5fleet/workers \
        /my5fleet/reference \
        /my5fleet/config \
        /var/log/supervisor

# ── Bake MT5 + Python + packages into the image at build time ─────────────────
# Copy only the install script first so that changing other scripts later
# does NOT bust this expensive cache layer (~10-15 min, ~1.5 GB result).
COPY scripts/install_reference.sh /opt/mt5/scripts/install_reference.sh
RUN chmod +x /opt/mt5/scripts/install_reference.sh \
    && FLEET_DIR=/my5fleet \
       WINEPREFIX=/my5fleet/wineprefix \
       DISPLAY=:99 \
       WINEDEBUG=-all \
       /opt/mt5/scripts/install_reference.sh

# ── Control API binary (from builder) ─────────────────────────────────────────
RUN mkdir -p /opt/mt5/control_api
COPY --from=go-builder /build/control-api /opt/mt5/control_api/server

# ── Remaining scripts ─────────────────────────────────────────────────────────
COPY scripts/ /opt/mt5/scripts/
RUN chmod +x /opt/mt5/scripts/*.sh

# ── Supervisor configuration ──────────────────────────────────────────────────
RUN mkdir -p /etc/supervisor
COPY supervisord.conf /etc/supervisor/supervisord.conf

ENTRYPOINT ["/opt/mt5/scripts/entrypoint.sh"]
