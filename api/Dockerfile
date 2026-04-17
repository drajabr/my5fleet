# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy module definition first for layer-cache efficiency
COPY api/go.mod .

# Copy source and compile a fully static binary (no libc deps → scratch image)
COPY api/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -trimpath -o server .

# ── Stage 2: minimal runtime ───────────────────────────────────────────────────
# 'scratch' is an empty image — the binary is the only process, no shell, no OS.
# Final image ≈ 5 MB (binary ~4 MB + frontend assets).
FROM scratch

# TLS root certs (needed if you ever add HTTPS upstream calls)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Binary
COPY --from=builder /build/server /server

# Frontend assets (docker-compose passes the repo root as build context)
COPY frontend/ /frontend/

EXPOSE 17380

ENV FRONTEND_DIR=/frontend \
    PORT=17380

ENTRYPOINT ["/server"]
