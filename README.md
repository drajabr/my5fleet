# mt5-fleet

Run multiple isolated MetaTrader 5 terminals inside a single Docker container, each exposed as an RPyC endpoint. A lightweight web dashboard and REST API let you add/remove workers and retrieve connection details without touching the container.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Host                                                       │
│  :8080  ──► api container (Go, ~5 MB)                       │
│               ├─ / → frontend/                              │
│               └─ /api/* → proxied to engine:18810           │
│                                                             │
│  :18812-18912 ──► engine container (Debian + Wine + Xvfb)   │
│                     ├─ supervisord                          │
│                     │   ├─ Xvfb :99                         │
│                     │   └─ control-api :18810 (internal)    │
│                     └─ per worker (managed by control-api)  │
│                         ├─ terminal64.exe /portable         │
│                         └─ worker_rpyc.py :<port>           │
└─────────────────────────────────────────────────────────────┘
```

| Container | Image base | Role |
|-----------|-----------|------|
| `engine`  | Debian bookworm-slim + wine64 | Runs MT5 terminals + RPyC servers + internal control API |
| `api`     | scratch (static Go binary) | Serves the SPA and proxies REST calls to the engine |

Persistent volumes:

| Volume | Mounted at | Contains |
|--------|-----------|----------|
| `mt5-workers` | `/mt5-fleet/workers` | Per-worker directories (symlinks + writable dirs) |
| `mt5-config`  | `/mt5-fleet/config`  | `workers.json` state file |

The Wine prefix, MT5 binaries, and Windows Python 3.11 are baked into the `engine` image at build time — no internet access is needed at runtime.

---

## Quick start

```bash
# 1. Set image tags (usually GHCR latest)
#    Example:
#    ENGINE_IMAGE=ghcr.io/<owner>/mt5-fleet-engine:latest
#    API_IMAGE=ghcr.io/<owner>/mt5-fleet-api:latest
#    or put them in .env

# 2. Start
docker compose up -d

# 3. Open the dashboard
open http://localhost:8080
```

Then click **+ Add Worker**, fill in your MT5 credentials, and the engine will:
1. Create an isolated worker directory under `/mt5-fleet/workers/terminal_N/`
2. Launch `terminal64.exe /portable` via Wine
3. Start a `worker_rpyc.py` server on the next free port (starting at 18812)

---

## Configuration

| Env variable | Default | Description |
|-------------|---------|-------------|
| `WEB_PORT` | `8080` | Host port for the dashboard |
| `WORKER_PORT_RANGE_START` | `18812` | First RPyC port published to host |
| `WORKER_PORT_RANGE_END` | `18912` | Last RPyC port published to host |
| `VNC_WS_PORT_RANGE_START` | `19000` | First host port mapped to worker noVNC websocket (container 6800+) |
| `VNC_WS_PORT_RANGE_END` | `19100` | Last host port mapped to worker noVNC websocket (container 6900) |
| `ENGINE_IMAGE` | `ghcr.io/your-org/mt5-fleet-engine:latest` | Engine image used by `docker-compose.yml` |
| `API_IMAGE` | `ghcr.io/your-org/mt5-fleet-api:latest` | API image used by `docker-compose.yml` |

Set them in a `.env` file at the repo root or export them before running `docker compose up`.

---

## Connecting to a worker via RPyC

After adding a worker through the dashboard, click **"<>"** on the worker card to get its port and token. Then from any Python environment on the same machine:

```bash
pip install rpyc
```

```python
import socket
import rpyc

def connect_worker(host: str, port: int, token: str):
    sock = socket.create_connection((host, port), timeout=10)
    sock.sendall(token.encode() + b"\n")
    conn = rpyc.connect_stream(rpyc.SocketStream(sock))
    return conn.root

# Replace port and token with the values shown in the dashboard
mt5 = connect_worker("localhost", 18812, "<your-token>")

# Use the MT5 Python API exactly as you would natively.
# initialize() connects the library to the running terminal.
# Pass credentials here to log in, or call mt5.login() separately.
mt5.initialize(login=12345678, password="secret", server="ICMarkets-Demo")

print(mt5.account_info())
print(mt5.symbol_info("EURUSD"))

rates = mt5.copy_rates_from_pos("EURUSD", mt5.TIMEFRAME_H1, 0, 10)
print(rates)

mt5.shutdown()
```

> The custom authenticator reads `<token>\n` on the raw socket **before** the RPyC handshake begins. Any connection that sends the wrong token is rejected immediately.

---

## REST API

All endpoints are available through the `api` container at `http://localhost:8080/api/`.

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/api/health` | Liveness check |
| `GET`  | `/api/status` | Engine install status (`installing` / `ready`) |
| `GET`  | `/api/workers` | List all workers with status, port, and token |
| `POST` | `/api/workers` | Create a new worker (see body below) |
| `POST` | `/api/workers/{id}/start` | Start a stopped worker |
| `POST` | `/api/workers/{id}/stop`  | Stop a running worker |
| `DELETE` | `/api/workers/{id}` | Delete a worker and its directory |

**Create worker body:**
```json
{
  "name": "My Broker Demo",
  "config": {
    "login": 12345678,
    "password": "secret",
    "server": "ICMarkets-Demo"
  }
}
```

---

## Worker isolation

Each worker gets its own directory under `/mt5-fleet/workers/terminal_N/`. MT5 is launched with the `/portable` flag, which means it derives its Windows named-pipe name from the EXE path. Different directories → different pipe names → fully isolated processes.

Read-only files (the MT5 binaries) are **symlinked** back to `/mt5-fleet/reference/install/`. Only the directories that MT5 writes into are real copies:

```
MQL5/   logs/   config/   tester/   bases/   profiles/
```

---

## Development

### Use local build (no GHCR pull)

`docker-compose.local.yml` is for local image build/testing with all env and ports embedded in the file.

```bash
docker compose -f docker-compose.local.yml up -d --build
```

### Rebuild only one service locally

```bash
# Rebuild only the engine after script changes
docker compose -f docker-compose.local.yml build engine
docker compose -f docker-compose.local.yml up -d --force-recreate engine

# Rebuild only the api after Go changes
docker compose -f docker-compose.local.yml build api
docker compose -f docker-compose.local.yml up -d --force-recreate api

# Tail engine logs
docker compose -f docker-compose.local.yml logs -f engine

# Open a shell inside the engine
docker exec -it engine-local bash

# Check workers.json state
docker exec engine-local cat /mt5-fleet/config/workers.json
```

---

## CI image build

GitHub Actions workflow `.github/workflows/docker-images.yml` builds and pushes:

- `ghcr.io/<owner>/mt5-fleet-engine`
- `ghcr.io/<owner>/mt5-fleet-api`

Published tags:

- `latest` on the default branch
- `sha-...` for every push
- release tag passthrough for `v*` tags

---

## Project layout

```
docker-compose.yml
docker-compose.local.yml
.github/
  workflows/
    docker-images.yml
api/
  Dockerfile          # scratch image, static Go binary
  main.go             # HTTP server: SPA + /api/* reverse proxy
  go.mod
engine/
  Dockerfile          # Debian + wine64 + Xvfb + supervisord; bakes MT5 + Python at build time
  supervisord.conf    # manages Xvfb and control-api
  control_api/
    main.go           # HTTP handlers
    manager.go        # worker lifecycle (create / start / stop / delete)
    go.mod
  scripts/
    entrypoint.sh     # starts supervisord
    install_reference.sh  # installs MT5 + Windows Python into Wine (build-time)
    create_worker.sh  # sets up a new worker directory
    start_worker.sh   # launches terminal64.exe + worker_rpyc.py
    stop_worker.sh    # gracefully stops a worker
    worker_rpyc.py    # RPyC server wrapping the MetaTrader5 Python API (runs under Wine)
frontend/
  index.html
  static/style.css
```
