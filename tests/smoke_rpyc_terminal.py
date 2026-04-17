#!/usr/bin/env python3
"""End-to-end smoke test for mt5-fleet worker + RPyC connectivity.

Flow:
1) Create a worker through /api/workers
2) Start it through /api/workers/{id}/start
3) Wait until status becomes running
4) Connect via raw socket + token and RPyC stream
5) Call mt5.version() as a lightweight RPyC verification
6) Stop and delete worker (best effort cleanup)
"""

from __future__ import annotations

import argparse
import json
import secrets
import socket
import sys
import time
from dataclasses import dataclass
from typing import Any
from urllib import error, request

import rpyc


@dataclass
class Worker:
    id: str
    name: str
    port: int
    token: str
    status: str


def _http_json(
    method: str,
    url: str,
    body: dict[str, Any] | None = None,
    timeout_s: int = 20,
) -> tuple[int, Any]:
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = request.Request(url, method=method, data=data, headers=headers)
    try:
        with request.urlopen(req, timeout=timeout_s) as resp:
            raw = resp.read()
            if not raw:
                return resp.status, None
            return resp.status, json.loads(raw)
    except error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {exc.code} {method} {url}: {raw}") from exc


def create_worker(api_base: str, name: str, token: str) -> Worker:
    code, payload = _http_json(
        "POST",
        f"{api_base}/workers",
        {"name": name, "token": token},
        timeout_s=180,
    )
    if code != 201:
        raise RuntimeError(f"create worker failed: expected 201 got {code} payload={payload}")
    return Worker(
        id=payload["id"],
        name=payload.get("name", payload["id"]),
        port=int(payload["port"]),
        token=payload["token"],
        status=payload.get("status", "unknown"),
    )


def start_worker(api_base: str, worker_id: str) -> None:
    code, payload = _http_json("POST", f"{api_base}/workers/{worker_id}/start")
    if code != 200:
        raise RuntimeError(f"start worker failed: expected 200 got {code} payload={payload}")


def get_worker(api_base: str, worker_id: str) -> Worker:
    code, payload = _http_json("GET", f"{api_base}/workers/{worker_id}")
    if code != 200:
        raise RuntimeError(f"get worker failed: expected 200 got {code} payload={payload}")
    return Worker(
        id=payload["id"],
        name=payload.get("name", payload["id"]),
        port=int(payload["port"]),
        token=payload["token"],
        status=payload.get("status", "unknown"),
    )


def wait_running(api_base: str, worker_id: str, timeout_s: int = 180) -> Worker:
    deadline = time.time() + timeout_s
    last: Worker | None = None
    while time.time() < deadline:
        last = get_worker(api_base, worker_id)
        if last.status == "running":
            return last
        time.sleep(2)
    raise TimeoutError(f"worker did not reach running state in {timeout_s}s; last={last}")


def probe_rpyc(
    host: str,
    port: int,
    token: str,
    timeout_s: int = 120,
    strict_root_call: bool = False,
) -> tuple[bool, Any | None]:
    deadline = time.time() + timeout_s
    last_exc: Exception | None = None
    connected_once = False

    while time.time() < deadline:
        try:
            sock = socket.create_connection((host, port), timeout=10)
            sock.sendall(token.encode("utf-8") + b"\n")
            conn = rpyc.connect_stream(rpyc.SocketStream(sock))
            connected_once = True

            if not strict_root_call:
                conn.close()
                return True, None

            # Strict mode: require one remote call to complete.
            version = conn.root.version()
            conn.close()
            return True, version
        except Exception as exc:  # noqa: BLE001
            last_exc = exc
            time.sleep(2)

    if strict_root_call:
        raise TimeoutError(f"failed to establish strict RPyC root call in {timeout_s}s: {last_exc}")
    if connected_once:
        return True, None
    raise TimeoutError(f"failed to establish RPyC transport in {timeout_s}s: {last_exc}")


def stop_worker(api_base: str, worker_id: str) -> None:
    try:
        _http_json("POST", f"{api_base}/workers/{worker_id}/stop")
    except Exception:  # noqa: BLE001
        pass


def delete_worker(api_base: str, worker_id: str) -> None:
    code, _ = _http_json("DELETE", f"{api_base}/workers/{worker_id}")
    if code != 204:
        raise RuntimeError(f"delete worker failed: expected 204 got {code}")


def main() -> int:
    parser = argparse.ArgumentParser(description="Smoke-test mt5 worker start + RPyC connectivity")
    parser.add_argument("--api-base", default="http://localhost:17380/api", help="Base API URL")
    parser.add_argument("--rpyc-host", default="localhost", help="Host for direct RPyC socket")
    parser.add_argument(
        "--strict-root-call",
        action="store_true",
        help="Require an actual RPyC root call (mt5.version()) before passing",
    )
    parser.add_argument("--keep-worker", action="store_true", help="Do not stop/delete worker after test")
    args = parser.parse_args()

    worker_name = f"smoke-{int(time.time())}"
    token = secrets.token_urlsafe(24)
    worker: Worker | None = None

    print(f"[1/5] Creating worker {worker_name}...")
    worker = create_worker(args.api_base, worker_name, token)
    print(f"  created id={worker.id} port={worker.port}")

    try:
        print("[2/5] Starting worker...")
        start_worker(args.api_base, worker.id)

        print("[3/5] Waiting for running status...")
        worker = wait_running(args.api_base, worker.id)
        print(f"  worker is running on port {worker.port}")

        print("[4/5] Connecting through RPyC with token auth...")
        ok, version = probe_rpyc(
            args.rpyc_host,
            worker.port,
            worker.token,
            strict_root_call=args.strict_root_call,
        )
        if not ok:
            raise RuntimeError("RPyC probe failed")
        if args.strict_root_call:
            print(f"  RPyC strict check ok, mt5.version() -> {version}")
        else:
            print("  RPyC transport check ok (token auth + protocol connection)")

        print("[5/5] SUCCESS")
        return 0
    finally:
        if worker and not args.keep_worker:
            print("Cleanup: stopping and deleting test worker...")
            try:
                stop_worker(args.api_base, worker.id)
                print(f"  stopped worker {worker.id}")
            except Exception as exc:  # noqa: BLE001
                print(f"  warning: stop failed: {exc}")
            try:
                delete_worker(args.api_base, worker.id)
                print(f"  deleted worker {worker.id}")
            except Exception as exc:  # noqa: BLE001
                print(f"  ERROR: delete failed: {exc}")
                raise


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # noqa: BLE001
        print(f"FAILED: {exc}", file=sys.stderr)
        raise
