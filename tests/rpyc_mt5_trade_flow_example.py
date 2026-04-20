#!/usr/bin/env python3
"""Manual RPyC client example for exercising a remote MT5 worker.

Flow:
1. Fetch worker metadata from the REST API
2. Start the worker if needed and wait until it reports running
3. Connect over raw socket + token, then upgrade to RPyC
4. Initialize MT5, log in to the requested trading account
5. Print account, open positions, today's closed-position summary, and symbol info
6. Place a far-away minimum-size pending limit order with SL/TP, comment, and magic
7. List pending orders, delete the test order, list pending orders again, verify deletion

Usage example:
    # 1) Put values in tests/test.config (see template in this repo)
    # 2) Run with no sensitive CLI flags
    python tests/rpyc_mt5_trade_flow_example.py --config tests/test.config

    # CLI flags still override config values
    python tests/rpyc_mt5_trade_flow_example.py --config tests/test.config --symbol XAU
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import secrets
import socket
import sys
import time
from typing import Any
from urllib import error, request

import rpyc


DEFAULT_CONSTANTS = {
    "DEAL_ENTRY_IN": 0,
    "DEAL_ENTRY_OUT": 1,
    "DEAL_ENTRY_INOUT": 2,
    "DEAL_ENTRY_OUT_BY": 3,
    "ORDER_FILLING_FOK": 0,
    "ORDER_FILLING_IOC": 1,
    "ORDER_FILLING_RETURN": 2,
    "ORDER_TIME_GTC": 0,
    "ORDER_TIME_DAY": 1,
    "ORDER_TYPE_BUY": 0,
    "ORDER_TYPE_SELL": 1,
    "ORDER_TYPE_BUY_LIMIT": 2,
    "ORDER_TYPE_SELL_LIMIT": 3,
    "TRADE_ACTION_DEAL": 1,
    "TRADE_ACTION_PENDING": 5,
    "TRADE_ACTION_REMOVE": 8,
    "TRADE_RETCODE_DONE": 10009,
    "TRADE_RETCODE_DONE_PARTIAL": 10010,
    "TRADE_RETCODE_PLACED": 10008,
}


def _parse_int(value: str | None, default: int) -> int:
    if value is None or value == "":
        return default
    return int(value)


def _parse_bool(value: str | None, default: bool) -> bool:
    if value is None or value == "":
        return default
    normalized = value.strip().lower()
    if normalized in {"1", "true", "yes", "y", "on"}:
        return True
    if normalized in {"0", "false", "no", "n", "off"}:
        return False
    raise ValueError(f"invalid boolean value: {value!r}")


def load_key_value_config(path: str) -> dict[str, str]:
    """Load a simple key=value config file.

    Supports comments that start with # or ; and blank lines.
    Keys are normalized to lowercase with '-' replaced by '_'.
    """
    if not path:
        return {}
    if not os.path.exists(path):
        return {}

    config: dict[str, str] = {}
    with open(path, "r", encoding="utf-8") as f:
        for raw_line in f:
            line = raw_line.strip()
            if not line or line.startswith("#") or line.startswith(";"):
                continue
            if "=" not in line:
                continue
            key, value = line.split("=", 1)
            normalized_key = key.strip().lower().replace("-", "_")
            config[normalized_key] = value.strip()
    return config


def print_json(title: str, payload: Any) -> None:
    print(f"\n=== {title} ===")
    print(json.dumps(payload, indent=2, sort_keys=True, default=str))


def fail(message: str) -> None:
    raise RuntimeError(message)


def http_json(
    method: str,
    url: str,
    body: dict[str, Any] | None = None,
    timeout_s: int = 30,
) -> tuple[int, Any]:
    data = None
    headers: dict[str, str] = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = request.Request(url, method=method, data=data, headers=headers)
    try:
        with request.urlopen(req, timeout=timeout_s) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw else None
    except error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        fail(f"HTTP {exc.code} {method} {url} failed: {raw}")
    except error.URLError as exc:
        fail(f"HTTP {method} {url} failed: {exc}")


def get_worker(api_base: str, worker_id: str) -> dict[str, Any]:
    code, payload = http_json("GET", f"{api_base}/workers/{worker_id}")
    if code != 200 or not isinstance(payload, dict):
        fail(f"unexpected worker response: code={code} payload={payload}")
    return payload


def create_worker(
    api_base: str,
    worker_name: str,
    token: str,
    login: int,
    password: str,
    server: str,
) -> dict[str, Any]:
    code, payload = http_json(
        "POST",
        f"{api_base}/workers",
        {
            "name": worker_name,
            "token": token,
            "config": {
                "login": login,
                "password": password,
                "server": server,
            },
        },
        timeout_s=180,
    )
    if code != 201 or not isinstance(payload, dict):
        fail(f"unexpected create response: code={code} payload={payload}")
    return payload


def start_worker(api_base: str, worker_id: str) -> None:
    code, payload = http_json("POST", f"{api_base}/workers/{worker_id}/start")
    if code not in {200, 202}:
        fail(f"unexpected start response: code={code} payload={payload}")


def stop_worker(api_base: str, worker_id: str) -> None:
    code, payload = http_json("POST", f"{api_base}/workers/{worker_id}/stop")
    if code not in {200, 202}:
        fail(f"unexpected stop response: code={code} payload={payload}")


def delete_worker(api_base: str, worker_id: str) -> None:
    code, payload = http_json("DELETE", f"{api_base}/workers/{worker_id}")
    if code not in {202, 204}:
        fail(f"unexpected delete response: code={code} payload={payload}")


def wait_until_running(api_base: str, worker_id: str, timeout_s: int) -> dict[str, Any]:
    deadline = time.time() + timeout_s
    last_payload: dict[str, Any] | None = None
    last_error: str | None = None
    while time.time() < deadline:
        try:
            last_payload = get_worker(api_base, worker_id)
        except Exception as exc:
            # During startup, API can be reachable while engine backend is still
            # warming up. Keep retrying until timeout.
            last_error = str(exc)
            time.sleep(2)
            continue
        status = str(last_payload.get("status", "")).lower()
        if status == "running":
            return last_payload
        if status == "error":
            fail(f"worker entered error state: {last_payload}")
        time.sleep(2)
    fail(
        f"worker did not become running in {timeout_s}s; "
        f"last={last_payload}; last_error={last_error}"
    )


def connect_worker(host: str, port: int, token: str, timeout_s: int = 15):
    sock = socket.create_connection((host, port), timeout=timeout_s)
    sock.sendall(token.encode("utf-8") + b"\n")
    conn = rpyc.connect_stream(
        rpyc.SocketStream(sock),
        config={"sync_request_timeout": 90},
    )
    return conn


def load_constants(root: Any) -> dict[str, Any]:
    constants = dict(DEFAULT_CONSTANTS)
    try:
        remote = root.constants(list(DEFAULT_CONSTANTS))
    except Exception:
        remote = None
    if isinstance(remote, dict):
        constants.update(remote)
    return constants


def resolve_symbol(root: Any, requested_symbol: str) -> str:
    if root.symbol_select(requested_symbol, True):
        info = root.symbol_info(requested_symbol)
        if info:
            return requested_symbol

    requested_upper = requested_symbol.upper()
    symbols = root.symbols_get()
    for symbol in symbols:
        name = str(symbol.get("name", ""))
        if name.upper() == requested_upper:
            root.symbol_select(name, True)
            return name
    for symbol in symbols:
        name = str(symbol.get("name", ""))
        if requested_upper in name.upper():
            root.symbol_select(name, True)
            return name
    fail(f"symbol {requested_symbol!r} was not found")


def round_price(value: float, digits: int) -> float:
    return round(value, max(digits, 0))


def min_volume(symbol_info: dict[str, Any]) -> float:
    volume_min = float(symbol_info.get("volume_min") or 0)
    volume_step = float(symbol_info.get("volume_step") or 0)
    if volume_min <= 0:
        volume_min = volume_step if volume_step > 0 else 0.01
    if volume_step > 0:
        steps = round(volume_min / volume_step)
        return round(steps * volume_step, 8)
    return round(volume_min, 8)


def history_window_from_tick(tick: dict[str, Any]) -> tuple[float, float]:
    tick_time = tick.get("time")
    if not tick_time:
        now = dt.datetime.now(dt.timezone.utc)
    else:
        now = dt.datetime.fromtimestamp(float(tick_time), tz=dt.timezone.utc)
    start = now.replace(hour=0, minute=0, second=0, microsecond=0)
    return start.timestamp(), now.timestamp()


def summarize_closed_positions_today(deals: list[dict[str, Any]], constants: dict[str, Any]) -> list[dict[str, Any]]:
    exit_entries = {
        constants["DEAL_ENTRY_OUT"],
        constants["DEAL_ENTRY_INOUT"],
        constants["DEAL_ENTRY_OUT_BY"],
    }
    grouped: dict[int, dict[str, Any]] = {}
    for deal in deals:
        entry = deal.get("entry")
        if entry not in exit_entries:
            continue
        position_id = int(deal.get("position_id") or deal.get("position") or 0)
        if position_id <= 0:
            continue
        item = grouped.setdefault(
            position_id,
            {
                "position_id": position_id,
                "symbol": deal.get("symbol"),
                "volume": 0.0,
                "profit": 0.0,
                "swap": 0.0,
                "commission": 0.0,
                "deals": 0,
                "last_time": deal.get("time"),
            },
        )
        item["volume"] += float(deal.get("volume") or 0.0)
        item["profit"] += float(deal.get("profit") or 0.0)
        item["swap"] += float(deal.get("swap") or 0.0)
        item["commission"] += float(deal.get("commission") or 0.0)
        item["deals"] += 1
        item["last_time"] = max(float(item["last_time"] or 0), float(deal.get("time") or 0))
    return sorted(grouped.values(), key=lambda item: (item["last_time"], item["position_id"]))


def pending_order_request(
    symbol: str,
    symbol_info: dict[str, Any],
    tick: dict[str, Any],
    constants: dict[str, Any],
    magic: int,
    comment: str,
) -> dict[str, Any]:
    point = float(symbol_info.get("point") or 0)
    digits = int(symbol_info.get("digits") or 0)
    volume = min_volume(symbol_info)
    bid = float(tick.get("bid") or tick.get("last") or 0)
    if point <= 0 or bid <= 0 or volume <= 0:
        fail(f"symbol info/tick are not tradable enough: info={symbol_info} tick={tick}")

    stops_level = int(symbol_info.get("trade_stops_level") or 0)
    sltp_points = max(stops_level + 1, 10)
    entry_gap_points = max(stops_level * 20, 5000)

    price = round_price(max(point, bid - (entry_gap_points * point)), digits)
    sl = round_price(max(point, price - (sltp_points * point)), digits)
    tp = round_price(price + (sltp_points * point), digits)

    return {
        "action": constants["TRADE_ACTION_PENDING"],
        "symbol": symbol,
        "volume": volume,
        "type": constants["ORDER_TYPE_BUY_LIMIT"],
        "price": price,
        "sl": sl,
        "tp": tp,
        "deviation": 20,
        "magic": magic,
        "comment": comment,
        "type_time": constants["ORDER_TIME_GTC"],
        "type_filling": constants["ORDER_FILLING_RETURN"],
    }


def filling_mode_candidates(symbol_info: dict[str, Any], constants: dict[str, Any]) -> list[int]:
    candidates = [
        int(constants["ORDER_FILLING_RETURN"]),
        int(symbol_info.get("filling_mode") or -1),
        int(constants["ORDER_FILLING_FOK"]),
        int(constants["ORDER_FILLING_IOC"]),
    ]
    unique: list[int] = []
    for candidate in candidates:
        if candidate < 0 or candidate in unique:
            continue
        unique.append(candidate)
    return unique


def require_retcode(result: dict[str, Any], allowed: set[int], context: str) -> None:
    retcode = int(result.get("retcode") or 0)
    if retcode not in allowed:
        fail(f"{context} failed: retcode={retcode} result={json.dumps(result, default=str)}")


def order_ticket_from_result(result: dict[str, Any]) -> int:
    for key in ("order", "deal"):
        value = int(result.get(key) or 0)
        if value > 0:
            return value
    fail(f"order ticket missing from response: {result}")


def send_pending_order(
    root: Any,
    base_request: dict[str, Any],
    symbol_info: dict[str, Any],
    constants: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    allowed = {
        int(constants["TRADE_RETCODE_PLACED"]),
        int(constants["TRADE_RETCODE_DONE"]),
        int(constants["TRADE_RETCODE_DONE_PARTIAL"]),
    }
    last_result: dict[str, Any] | None = None
    last_request: dict[str, Any] | None = None
    for filling_mode in filling_mode_candidates(symbol_info, constants):
        request_with_filling = dict(base_request)
        request_with_filling["type_filling"] = filling_mode
        result = root.order_send(request_with_filling)
        retcode = int(result.get("retcode") or 0)
        if retcode in allowed:
            return request_with_filling, result
        last_request = request_with_filling
        last_result = result
    fail(
        "order_send failed for all filling modes: "
        f"request={json.dumps(last_request, default=str)} "
        f"result={json.dumps(last_result, default=str)}"
    )


def delete_pending_order(root: Any, constants: dict[str, Any], symbol: str, order_ticket: int, magic: int, comment: str) -> dict[str, Any]:
    return root.order_send(
        {
            "action": constants["TRADE_ACTION_REMOVE"],
            "order": order_ticket,
            "symbol": symbol,
            "magic": magic,
            "comment": f"{comment}-delete",
        }
    )


def verify_deleted(orders: list[dict[str, Any]], order_ticket: int) -> None:
    if any(int(order.get("ticket") or 0) == order_ticket for order in orders):
        fail(f"pending order {order_ticket} is still present after delete")


def parse_args() -> argparse.Namespace:
    script_dir = os.path.dirname(os.path.abspath(__file__))
    default_config_path = os.path.join(script_dir, "test.config")

    pre_parser = argparse.ArgumentParser(add_help=False)
    pre_parser.add_argument("--config", default=default_config_path, help="Path to key=value config file")
    pre_args, _ = pre_parser.parse_known_args()

    config = load_key_value_config(pre_args.config)

    parser = argparse.ArgumentParser(
        description="Manual RPyC MT5 trade-flow example",
        parents=[pre_parser],
    )
    parser.set_defaults(
        api_base=config.get("api_base", "http://localhost:17380/api"),
        rpyc_host=config.get("rpyc_host", "localhost"),
        worker_id=config.get("worker_id", ""),
        worker_name=config.get("worker_name", f"rpyc-auto-{int(time.time())}"),
        token=config.get("token"),
        login=_parse_int(config.get("login"), 0),
        password=config.get("password"),
        server=config.get("server"),
        symbol=config.get("symbol", "XAUUSD"),
        magic=_parse_int(config.get("magic"), 20260421),
        comment=config.get("comment", "my5fleet-rpyc-test"),
        wait_timeout=_parse_int(config.get("wait_timeout"), 180),
        auto_create_worker=_parse_bool(config.get("auto_create_worker"), True),
        cleanup_created_worker=_parse_bool(config.get("cleanup_created_worker"), False),
    )
    parser.add_argument("--api-base", dest="api_base", help="Base REST API URL")
    parser.add_argument("--rpyc-host", dest="rpyc_host", help="Host for direct RPyC socket")
    parser.add_argument("--worker-id", dest="worker_id", help="Existing worker id, for example terminal_1")
    parser.add_argument("--worker-name", dest="worker_name", help="Worker name used when auto-creating")
    parser.add_argument("--token", help="Worker token used by the raw-socket authenticator")
    parser.add_argument("--login", type=int, help="Trading account login")
    parser.add_argument("--password", help="Trading account password")
    parser.add_argument("--server", help="Broker server name")
    parser.add_argument("--symbol", help="Preferred symbol, exact name or substring")
    parser.add_argument("--magic", type=int, help="Magic number for the test order")
    parser.add_argument("--comment", help="Comment for the test order")
    parser.add_argument("--wait-timeout", dest="wait_timeout", type=int, help="Seconds to wait for worker startup")
    parser.add_argument(
        "--auto-create-worker",
        dest="auto_create_worker",
        action="store_true",
        help="Create a worker via API when worker_id is missing or not found",
    )
    parser.add_argument(
        "--no-auto-create-worker",
        dest="auto_create_worker",
        action="store_false",
        help="Do not create worker automatically",
    )
    parser.add_argument(
        "--cleanup-created-worker",
        dest="cleanup_created_worker",
        action="store_true",
        help="Stop and delete worker at the end only if this script created it",
    )

    args = parser.parse_args()
    missing: list[str] = []
    if not args.login:
        missing.append("login")
    if not args.password:
        missing.append("password")
    if not args.server:
        missing.append("server")
    if not args.worker_id and not args.auto_create_worker:
        missing.append("worker_id (or enable auto_create_worker)")
    if not args.token and not args.auto_create_worker:
        missing.append("token (or enable auto_create_worker)")
    if missing:
        parser.error(
            "missing required values: "
            + ", ".join(missing)
            + ". Provide them in --config or CLI flags."
        )
    return args


def main() -> int:
    args = parse_args()

    created_worker = False
    worker_id = args.worker_id
    token = args.token or ""

    print("[1/11] Resolving or creating worker...")
    worker: dict[str, Any] | None = None
    if worker_id:
        try:
            worker = get_worker(args.api_base, worker_id)
        except Exception:
            if not args.auto_create_worker:
                raise
            worker = None

    if worker is None:
        if not args.auto_create_worker:
            fail("worker not found and auto_create_worker is disabled")
        if not token:
            token = secrets.token_urlsafe(24)
        worker = create_worker(
            api_base=args.api_base,
            worker_name=args.worker_name,
            token=token,
            login=args.login,
            password=args.password,
            server=args.server,
        )
        created_worker = True
        worker_id = str(worker.get("id") or "")
        print_json("Created Worker", worker)
    else:
        worker_id = str(worker.get("id") or worker_id)
        if not token:
            token = str(worker.get("token") or "")
        print_json("Existing Worker", worker)

    if not worker_id:
        fail(f"worker id is missing in payload: {worker}")
    if not token:
        fail("worker token is missing; set token in config or use auto_create_worker")

    print("[2/11] Ensuring worker is running...")
    status = str((worker or {}).get("status", "")).lower()
    if status != "running":
        start_worker(args.api_base, worker_id)
    worker = wait_until_running(args.api_base, worker_id, args.wait_timeout)
    print_json("Worker Ready", worker)

    port = int(worker.get("port") or 0)
    if port <= 0:
        fail(f"worker does not expose a valid port: {worker}")

    print("[3/11] Connecting with token auth over raw socket + RPyC...")
    conn = connect_worker(args.rpyc_host, port, token)
    root = conn.root
    print_json("MT5 Version", root.version())

    try:
        constants = load_constants(root)

        print("[4/11] Initializing MT5 bridge on the remote worker...")
        initialized = bool(root.initialize())
        if not initialized:
            fail(f"mt5.initialize() failed: last_error={root.last_error()}")

        print("[5/11] Logging in to the trading account...")
        logged_in = bool(root.login(args.login, password=args.password, server=args.server))
        if not logged_in:
            fail(f"mt5.login() failed: last_error={root.last_error()}")

        terminal_info = root.terminal_info()
        account_info = root.account_info()
        print_json("Terminal Info", terminal_info)
        print_json("Account Info", account_info)

        print("[6/11] Resolving a gold symbol and loading market metadata...")
        symbol = resolve_symbol(root, args.symbol)
        symbol_info = root.symbol_info(symbol)
        tick = root.symbol_info_tick(symbol)
        if not symbol_info or not tick:
            fail(f"symbol data unavailable for {symbol!r}: info={symbol_info} tick={tick}")
        print_json("Symbol Info", symbol_info)
        print_json("Symbol Tick", tick)

        print("[7/11] Collecting open positions and today's closed-position summary...")
        open_positions = root.positions_get()
        day_start_ts, now_ts = history_window_from_tick(tick)
        today_orders = root.history_orders_get(day_start_ts, now_ts)
        today_deals = root.history_deals_get(day_start_ts, now_ts)
        closed_positions_today = summarize_closed_positions_today(today_deals, constants)
        print_json("Open Positions", open_positions)
        print_json("Today's History Orders", today_orders)
        print_json("Today's History Deals", today_deals)
        print_json("Today's Closed Positions Summary", closed_positions_today)

        print("[8/11] Building a far-away minimum-size buy limit order...")
        order_request = pending_order_request(
            symbol=symbol,
            symbol_info=symbol_info,
            tick=tick,
            constants=constants,
            magic=args.magic,
            comment=args.comment,
        )
        print_json("Pending Order Request", order_request)

        check_result = root.order_check(order_request)
        print_json("Order Check", check_result)

        print("[9/11] Sending the pending order...")
        used_order_request, send_result = send_pending_order(root, order_request, symbol_info, constants)
        if used_order_request["type_filling"] != order_request["type_filling"]:
            print_json("Pending Order Request Used", used_order_request)
        print_json("Order Send", send_result)
        order_ticket = order_ticket_from_result(send_result)

        pending_orders_before_delete = root.orders_get()
        print_json("Pending Orders After Place", pending_orders_before_delete)

        print("[10/11] Deleting the pending order...")
        delete_result = delete_pending_order(root, constants, symbol, order_ticket, args.magic, args.comment)
        print_json("Delete Result", delete_result)
        require_retcode(
            delete_result,
            {
                int(constants["TRADE_RETCODE_DONE"]),
                int(constants["TRADE_RETCODE_PLACED"]),
            },
            "order_delete",
        )

        print("[11/11] Verifying the pending order is gone...")
        pending_orders_after_delete = root.orders_get()
        print_json("Pending Orders After Delete", pending_orders_after_delete)
        verify_deleted(pending_orders_after_delete, order_ticket)
        print(f"Verified: pending order {order_ticket} was deleted.")
        return 0
    finally:
        try:
            root.shutdown()
        except Exception:
            pass
        conn.close()
        if created_worker and args.cleanup_created_worker and worker_id:
            print("Cleanup: stopping and deleting auto-created worker...")
            try:
                stop_worker(args.api_base, worker_id)
            except Exception as exc:
                print(f"Warning: stop failed for {worker_id}: {exc}")
            try:
                delete_worker(args.api_base, worker_id)
            except Exception as exc:
                print(f"Warning: delete failed for {worker_id}: {exc}")


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:  # noqa: BLE001
        print(f"FAILED: {exc}", file=sys.stderr)
        raise