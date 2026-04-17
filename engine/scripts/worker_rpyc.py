#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
worker_rpyc.py — RPyC server that exposes the MetaTrader5 Python API.

IMPORTANT: This script must be run via Wine's Windows Python because the
MetaTrader5 package is a Win32 extension that communicates with the terminal
over Windows named pipes:

    wine C:\\Python311\\python.exe Z:\\opt\\mt5\\scripts\\worker_rpyc.py \
        --port 18812 --token <token> --mt5-path Z:\\mt5-fleet\\workers\\terminal_1\\terminal64.exe

The /portable flag passed when launching the terminal means MT5 derives its
IPC pipe name from the terminal's absolute path, so each worker directory
produces a unique pipe name even though the binaries are shared symlinks.

Client-side connection (native Python):
    import socket, rpyc

    def connect_worker(host, port, token):
        sock = socket.create_connection((host, port), timeout=10)
        sock.sendall(token.encode() + b"\\n")
        conn = rpyc.connect_stream(rpyc.SocketStream(sock))
        return conn.root

    mt5 = connect_worker("localhost", 18812, "<token>")
    mt5.initialize()
    print(mt5.account_info())
"""

import argparse
import datetime
import hmac
import logging
import os
import sys
import time

# ── pythonw.exe guard ─────────────────────────────────────────────────────────
# When launched via `wine pythonw.exe` (GUI subsystem — no console), Python
# sets sys.stdout/stderr to None.  Replace with /dev/null so all code paths
# that write to them (including logging) don't crash before we open the log.
if sys.stdout is None:
    sys.stdout = open(os.devnull, "w")
if sys.stderr is None:
    sys.stderr = open(os.devnull, "w")

# ── Bootstrap logging early so any import errors are visible ─────────────────
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    stream=sys.stderr,
)
log = logging.getLogger("worker_rpyc")


# ── Argument parsing ──────────────────────────────────────────────────────────
def _parse_args():
    p = argparse.ArgumentParser(
        description="RPyC server wrapping the MetaTrader5 Python API"
    )
    p.add_argument("--port", type=int, required=True, help="TCP port to listen on")
    p.add_argument("--token", required=True, help="Authentication token")
    p.add_argument(
        "--mt5-path",
        required=True,
        help="Windows path to this worker's terminal64.exe, "
        r"e.g. Z:\mt5-fleet\workers\terminal_1\terminal64.exe",
    )
    p.add_argument(
        "--init-retries",
        type=int,
        default=12,
        help="How many times to retry mt5.initialize() before giving up (default 12)",
    )
    p.add_argument(
        "--init-delay",
        type=float,
        default=5.0,
        help="Seconds between mt5.initialize() retries (default 5.0)",
    )
    p.add_argument(
        "--log-file",
        default="",
        help="Path to the log file (used when running under pythonw.exe with no console)",
    )
    return p.parse_args()


# ── Serialisation helper ──────────────────────────────────────────────────────
def _to_py(obj):
    """Recursively convert MT5 result objects to plain Python types so they
    can cross the RPyC wire as value copies rather than netrefs."""
    if obj is None or isinstance(obj, (bool, int, float, str, bytes)):
        return obj
    if isinstance(obj, (list, tuple)):
        return [_to_py(x) for x in obj]
    if hasattr(obj, "_asdict"):  # namedtuple (most MT5 structs)
        return {k: _to_py(v) for k, v in obj._asdict().items()}
    # numpy ndarray (copy_rates_*, copy_ticks_* return structured arrays)
    try:
        import numpy as np  # noqa: PLC0415

        if isinstance(obj, np.ndarray):
            return obj.tolist()
    except ImportError:
        pass
    return obj


# ── Token authenticator ───────────────────────────────────────────────────────
def _make_authenticator(expected_token: str):
    """Returns an RPyC authenticator that reads a token line before the
    RPyC handshake starts.

    Protocol: client sends   <token>\\n   (UTF-8, exactly).
    Reading one byte at a time ensures no RPyC handshake bytes are consumed.
    """
    expected = expected_token.encode("utf-8")

    def authenticator(sock):
        buf = bytearray()
        while True:
            byte = sock.recv(1)
            if not byte:
                raise Exception("Connection closed during authentication")
            if byte == b"\n":
                break
            if len(buf) >= 256:
                sock.close()
                raise Exception("Token exceeds maximum length")
            buf.extend(byte)
        received = bytes(buf).rstrip(b"\r")
        if not hmac.compare_digest(received, expected):
            sock.close()
            raise Exception("Authentication failed: invalid token")
        return sock, None

    return authenticator


# ── RPyC service ──────────────────────────────────────────────────────────────
import MetaTrader5 as mt5  # noqa: E402  (must be imported after arg parse so errors are clear)
import rpyc  # noqa: E402
from rpyc.utils.server import ThreadedServer  # noqa: E402


class MT5Service(rpyc.Service):
    """Exposes the MetaTrader5 Python API over RPyC.

    All return values are converted to plain Python types (dicts, lists,
    primitives) so the caller receives value copies, not netrefs.
    """

    # Set at startup by main()
    _mt5_path: str = ""
    _init_retries: int = 12
    _init_delay: float = 5.0

    # ── Lifecycle ─────────────────────────────────────────────────────────────
    def on_connect(self, conn):
        peer = conn._channel.stream.sock.getpeername()
        log.info("Client connected from %s:%s", *peer)

    def on_disconnect(self, conn):
        log.info("Client disconnected")

    # ── Initialisation ────────────────────────────────────────────────────────
    def exposed_initialize(
        self,
        path: str = "",
        login: int = 0,
        password: str = "",
        server: str = "",
        timeout: int = 60000,
        portable: bool = False,
    ) -> bool:
        """Connect mt5 Python library to the running terminal.

        Call with no arguments to auto-connect using this worker's terminal path.
        """
        kwargs: dict = {"timeout": timeout, "portable": portable}
        if path:
            kwargs["path"] = path
        elif self._mt5_path:
            kwargs["path"] = self._mt5_path
        if login:
            kwargs["login"] = login
        if password:
            kwargs["password"] = password
        if server:
            kwargs["server"] = server
        return mt5.initialize(**kwargs)

    def exposed_shutdown(self) -> None:
        mt5.shutdown()

    def exposed_last_error(self) -> list:
        err = mt5.last_error()
        return list(err) if err else [0, ""]

    def exposed_version(self):
        v = mt5.version()
        return list(v) if v else None

    # ── Account / terminal ────────────────────────────────────────────────────
    def exposed_terminal_info(self) -> dict:
        return _to_py(mt5.terminal_info())

    def exposed_account_info(self) -> dict:
        return _to_py(mt5.account_info())

    def exposed_login(
        self, login: int, password: str = "", server: str = "", timeout: int = 60000
    ) -> bool:
        return mt5.login(login, password=password, server=server, timeout=timeout)

    # ── Symbols ───────────────────────────────────────────────────────────────
    def exposed_symbols_total(self) -> int:
        return mt5.symbols_total()

    def exposed_symbols_get(self, group: str = "") -> list:
        result = mt5.symbols_get(group) if group else mt5.symbols_get()
        return [_to_py(s) for s in result] if result else []

    def exposed_symbol_info(self, symbol: str) -> dict:
        return _to_py(mt5.symbol_info(symbol))

    def exposed_symbol_info_tick(self, symbol: str) -> dict:
        return _to_py(mt5.symbol_info_tick(symbol))

    def exposed_symbol_select(self, symbol: str, enable: bool = True) -> bool:
        return mt5.symbol_select(symbol, enable)

    # ── Market book ───────────────────────────────────────────────────────────
    def exposed_market_book_add(self, symbol: str) -> bool:
        return mt5.market_book_add(symbol)

    def exposed_market_book_release(self, symbol: str) -> bool:
        return mt5.market_book_release(symbol)

    def exposed_market_book_get(self, symbol: str) -> list:
        result = mt5.market_book_get(symbol)
        return [_to_py(x) for x in result] if result else []

    # ── Historical rates ──────────────────────────────────────────────────────
    def exposed_copy_rates_from(
        self, symbol: str, timeframe: int, date_from, count: int
    ) -> list:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        return _to_py(mt5.copy_rates_from(symbol, timeframe, date_from, count))

    def exposed_copy_rates_from_pos(
        self, symbol: str, timeframe: int, start_pos: int, count: int
    ) -> list:
        return _to_py(mt5.copy_rates_from_pos(symbol, timeframe, start_pos, count))

    def exposed_copy_rates_range(
        self, symbol: str, timeframe: int, date_from, date_to
    ) -> list:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        if isinstance(date_to, (int, float)):
            date_to = datetime.datetime.fromtimestamp(date_to)
        return _to_py(mt5.copy_rates_range(symbol, timeframe, date_from, date_to))

    def exposed_copy_ticks_from(
        self, symbol: str, date_from, count: int, flags: int
    ) -> list:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        return _to_py(mt5.copy_ticks_from(symbol, date_from, count, flags))

    def exposed_copy_ticks_range(
        self, symbol: str, date_from, date_to, flags: int
    ) -> list:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        if isinstance(date_to, (int, float)):
            date_to = datetime.datetime.fromtimestamp(date_to)
        return _to_py(mt5.copy_ticks_range(symbol, date_from, date_to, flags))

    # ── Open orders ───────────────────────────────────────────────────────────
    def exposed_orders_total(self) -> int:
        return mt5.orders_total()

    def exposed_orders_get(
        self, symbol: str = "", group: str = "", ticket: int = 0
    ) -> list:
        if ticket:
            result = mt5.orders_get(ticket=ticket)
        elif symbol:
            result = mt5.orders_get(symbol=symbol)
        elif group:
            result = mt5.orders_get(group=group)
        else:
            result = mt5.orders_get()
        return [_to_py(o) for o in result] if result else []

    # ── Open positions ────────────────────────────────────────────────────────
    def exposed_positions_total(self) -> int:
        return mt5.positions_total()

    def exposed_positions_get(
        self, symbol: str = "", group: str = "", ticket: int = 0
    ) -> list:
        if ticket:
            result = mt5.positions_get(ticket=ticket)
        elif symbol:
            result = mt5.positions_get(symbol=symbol)
        elif group:
            result = mt5.positions_get(group=group)
        else:
            result = mt5.positions_get()
        return [_to_py(p) for p in result] if result else []

    # ── Trade history ─────────────────────────────────────────────────────────
    def exposed_history_orders_total(self, date_from, date_to) -> int:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        if isinstance(date_to, (int, float)):
            date_to = datetime.datetime.fromtimestamp(date_to)
        return mt5.history_orders_total(date_from, date_to)

    def exposed_history_orders_get(
        self,
        date_from=None,
        date_to=None,
        group: str = "",
        ticket: int = 0,
        position: int = 0,
    ) -> list:
        if ticket:
            result = mt5.history_orders_get(ticket=ticket)
        elif position:
            result = mt5.history_orders_get(position=position)
        else:
            if isinstance(date_from, (int, float)):
                date_from = datetime.datetime.fromtimestamp(date_from)
            if isinstance(date_to, (int, float)):
                date_to = datetime.datetime.fromtimestamp(date_to)
            if group:
                result = mt5.history_orders_get(date_from, date_to, group=group)
            else:
                result = mt5.history_orders_get(date_from, date_to)
        return [_to_py(o) for o in result] if result else []

    def exposed_history_deals_total(self, date_from, date_to) -> int:
        if isinstance(date_from, (int, float)):
            date_from = datetime.datetime.fromtimestamp(date_from)
        if isinstance(date_to, (int, float)):
            date_to = datetime.datetime.fromtimestamp(date_to)
        return mt5.history_deals_total(date_from, date_to)

    def exposed_history_deals_get(
        self,
        date_from=None,
        date_to=None,
        group: str = "",
        ticket: int = 0,
        position: int = 0,
    ) -> list:
        if ticket:
            result = mt5.history_deals_get(ticket=ticket)
        elif position:
            result = mt5.history_deals_get(position=position)
        else:
            if isinstance(date_from, (int, float)):
                date_from = datetime.datetime.fromtimestamp(date_from)
            if isinstance(date_to, (int, float)):
                date_to = datetime.datetime.fromtimestamp(date_to)
            if group:
                result = mt5.history_deals_get(date_from, date_to, group=group)
            else:
                result = mt5.history_deals_get(date_from, date_to)
        return [_to_py(d) for d in result] if result else []

    # ── Trading operations ────────────────────────────────────────────────────
    def exposed_order_check(self, request: dict) -> dict:
        return _to_py(mt5.order_check(request))

    def exposed_order_send(self, request: dict) -> dict:
        return _to_py(mt5.order_send(request))

    def exposed_order_calc_margin(
        self, action: int, symbol: str, volume: float, price: float
    ) -> float:
        return mt5.order_calc_margin(action, symbol, volume, price)

    def exposed_order_calc_profit(
        self,
        action: int,
        symbol: str,
        volume: float,
        price_open: float,
        price_close: float,
    ) -> float:
        return mt5.order_calc_profit(action, symbol, volume, price_open, price_close)


# ── Main ──────────────────────────────────────────────────────────────────────
def main():
    args = _parse_args()

    # Reconfigure logging to write to a file when running under pythonw.exe
    # (which has no console, so stderr/stdout were replaced with /dev/null above).
    if args.log_file:
        root = logging.getLogger()
        for h in list(root.handlers):
            root.removeHandler(h)
        fh = logging.FileHandler(args.log_file, encoding="utf-8")
        fh.setFormatter(
            logging.Formatter("%(asctime)s [%(levelname)s] %(message)s")
        )
        root.addHandler(fh)

    # Store config on the class (shared across all service instances)
    MT5Service._mt5_path = args.mt5_path
    MT5Service._init_retries = args.init_retries
    MT5Service._init_delay = args.init_delay

    # Attempt to pre-initialise MT5 with retries so we fail fast if the
    # terminal never starts instead of silently serving a broken connection.
    log.info("Attempting to initialise MT5 at path: %s", args.mt5_path)
    initialized = False
    for attempt in range(1, args.init_retries + 1):
        ok = mt5.initialize(path=args.mt5_path)
        if ok:
            log.info("MT5 initialised successfully on attempt %d", attempt)
            initialized = True
            break
        err = mt5.last_error()
        log.warning(
            "MT5 init attempt %d/%d failed: %s", attempt, args.init_retries, err
        )
        if attempt < args.init_retries:
            time.sleep(args.init_delay)

    if not initialized:
        log.error(
            "MT5 could not be initialised after %d attempts. "
            "The RPyC server will still start so clients can retry "
            "via exposed_initialize().",
            args.init_retries,
        )

    authenticator = _make_authenticator(args.token)

    server = ThreadedServer(
        MT5Service,
        port=args.port,
        hostname="0.0.0.0",
        authenticator=authenticator,
        protocol_config={
            "allow_public_attrs": True,
            "sync_request_timeout": 60,
        },
    )
    log.info("RPyC server listening on 0.0.0.0:%d", args.port)
    server.start()


if __name__ == "__main__":
    main()
