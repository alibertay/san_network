#!/usr/bin/env python3
"""One-command bring-up for the Go SAN node.

This is the "start my node" script, not a test harness. It builds the Go
binaries when needed, creates (or reuses) the node identity key, starts the
node with sensible devnet defaults and reconciles the validator stake:

    python scripts/go_node.py --wallet 0x...            # start, no stake
    python scripts/go_node.py --wallet 0x... --stake 100
    python scripts/go_node.py --stop
    python scripts/go_node.py --status

The wallet must be the address of the node key file (default
<data-dir>/san_key.json): the node signs staking and transfer transactions
with that key and genesis funds are allocated to its address.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from decimal import Decimal, InvalidOperation
from pathlib import Path
from urllib import error as urlerror
from urllib import request as urlrequest

ROOT = Path(__file__).resolve().parents[1]
BIN_DIR = ROOT / "bin"
SAN_BASE = Decimal(10) ** 8
IS_WINDOWS = os.name == "nt"

DEFAULT_DATA_DIR = ROOT / "data" / "go-node"
DEFAULT_API_PORT = 8000
DEFAULT_P2P_PORT = 8765
DEFAULT_PEER_PORT = 8770
DEFAULT_CONTROLLER_PORT = 8769

GENESIS_PARAMETER_ENV = (
    "SAN_CHAIN_ID",
    "SAN_GENESIS_ALLOCATION",
    "SAN_BLOCK_REWARD",
    "SAN_MIN_VALIDATOR_STAKE",
    "SAN_UNBONDING_PERIOD",
    "SAN_SLASH_BPS",
    "SAN_BLOCK_GAS_LIMIT",
    "SAN_PROPOSER_TIMEOUT",
    "SAN_MIN_BLOCK_INTERVAL_MS",
)


class NodeToolError(RuntimeError):
    """A user-actionable bring-up failure."""


def log(message: str) -> None:
    print(f"[go_node] {message}", flush=True)


# ---------------------------------------------------------------------- #
# Binaries
# ---------------------------------------------------------------------- #


def binary(name: str) -> Path:
    return BIN_DIR / (name + (".exe" if IS_WINDOWS else ""))


def _newest_source_mtime() -> float:
    newest = 0.0
    for directory in ("cmd", "internal"):
        for path in (ROOT / directory).rglob("*.go"):
            newest = max(newest, path.stat().st_mtime)
    for name in ("go.mod", "go.sum"):
        path = ROOT / name
        if path.exists():
            newest = max(newest, path.stat().st_mtime)
    return newest


def ensure_binaries(force: bool = False) -> None:
    """Build bin/sannode and bin/sancli when missing or older than the sources."""
    target = binary("sannode")
    newest = _newest_source_mtime()
    needs_build = force or not target.exists() or not binary("sancli").exists()
    if not needs_build:
        oldest_binary = min(target.stat().st_mtime, binary("sancli").stat().st_mtime)
        needs_build = oldest_binary < newest
    if not needs_build:
        return
    log("building bin/sannode and bin/sancli (go build)")
    BIN_DIR.mkdir(parents=True, exist_ok=True)
    for package, name in (("./cmd/sannode", "sannode"), ("./cmd/sancli", "sancli")):
        completed = subprocess.run(
            ["go", "build", "-o", str(binary(name)), package],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
        )
        if completed.returncode != 0:
            raise NodeToolError(f"go build {package} failed:\n{completed.stderr.strip()}")


# ---------------------------------------------------------------------- #
# Key handling
# ---------------------------------------------------------------------- #


def sancli_json(args: list[str], allow_failure: bool = False) -> dict:
    completed = subprocess.run(
        [str(binary("sancli")), *args], cwd=str(ROOT), capture_output=True, text=True
    )
    if completed.returncode != 0 and not allow_failure:
        detail = (completed.stderr or completed.stdout).strip()
        raise NodeToolError(f"sancli {' '.join(args)} failed: {detail}")
    try:
        return json.loads(completed.stdout)
    except json.JSONDecodeError:
        if allow_failure:
            return {}
        raise NodeToolError(f"sancli {' '.join(args)} returned unexpected output: {completed.stdout!r}")


def key_address(key_file: Path) -> str:
    record = sancli_json(["wallet", "address", "--key", str(key_file)])
    return str(record["address"])


def ensure_key(key_file: Path) -> tuple[str, bool]:
    """Create the key file when missing; return (address, created)."""
    created = not key_file.exists()
    if created:
        key_file.parent.mkdir(parents=True, exist_ok=True)
        sancli_json(["wallet", "new", "--out", str(key_file)])
        log(f"created a new node key file: {key_file}")
    return key_address(key_file), created


def check_wallet(key_file: Path, address: str, wallet: str, created: bool) -> None:
    if wallet == address:
        return
    lines = [
        f"--wallet {wallet} does not match the node key file",
        f"  key file   : {key_file}",
        f"  its address: {address}",
    ]
    if created:
        lines.append("(the key file did not exist, so a fresh one was just created)")
    lines.append(
        "Use the wallet whose key lives in that file, or copy your wallet key there "
        f"(inspect any key with: {binary('sancli')} wallet address --key FILE)."
    )
    raise NodeToolError("\n".join(lines))


# ---------------------------------------------------------------------- #
# HTTP helpers
# ---------------------------------------------------------------------- #


def http_json(url: str, payload: dict | None = None, timeout: float = 10.0) -> dict:
    data = None
    headers = {}
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urlrequest.Request(url, data=data, headers=headers, method="POST" if data else "GET")
    with urlrequest.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def get_json(api: str, path: str, timeout: float = 10.0) -> dict:
    return http_json(f"{api}{path}", timeout=timeout)


def try_get_json(api: str, path: str, timeout: float = 3.0) -> dict | None:
    try:
        return get_json(api, path, timeout=timeout)
    except (urlerror.URLError, OSError, ValueError, json.JSONDecodeError):
        return None


def node_healthy(api: str) -> bool:
    health = try_get_json(api, "/health")
    return bool(health and health.get("status") == "ok")


# ---------------------------------------------------------------------- #
# Process control
# ---------------------------------------------------------------------- #


def process_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    if IS_WINDOWS:
        completed = subprocess.run(
            ["tasklist", "/FI", f"PID eq {pid}", "/NH"],
            capture_output=True,
            text=True,
        )
        return str(pid) in completed.stdout
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    return True


def read_pid(pid_file: Path) -> int:
    try:
        return int(pid_file.read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return 0


def stop_node(data_dir: Path, pid_file: Path, quiet: bool = False) -> int:
    pid = read_pid(pid_file)
    if pid and process_alive(pid):
        if IS_WINDOWS:
            subprocess.run(
                ["taskkill", "/F", "/T", "/PID", str(pid)],
                capture_output=True,
                text=True,
            )
        else:
            import signal

            try:
                os.kill(pid, signal.SIGTERM)
            except OSError:
                pass
        for _ in range(40):
            if not process_alive(pid):
                break
            time.sleep(0.25)
        if not quiet:
            log(f"stopped the node (pid {pid})")
    elif not quiet:
        log("no running node found for this data directory")
    pid_file.unlink(missing_ok=True)
    return 0


def api_url(port: int) -> str:
    return f"http://127.0.0.1:{port}"


def wait_for(predicate, timeout: float, description: str, interval: float = 0.5):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval)
    raise NodeToolError(f"timed out after {timeout:.0f}s waiting for {description}")


# ---------------------------------------------------------------------- #
# Stake reconciliation
# ---------------------------------------------------------------------- #


def san_units(amount: str | Decimal) -> int:
    try:
        scaled = Decimal(str(amount)) * SAN_BASE
    except InvalidOperation as exc:
        raise NodeToolError(f"invalid SAN amount: {amount!r}") from exc
    if scaled != scaled.to_integral_value():
        raise NodeToolError(f"amount {amount} has more than 8 decimals")
    return int(scaled)


def units_san(units: int) -> str:
    text = format(Decimal(units) / SAN_BASE, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def stake_record(api: str, address: str) -> dict:
    return get_json(api, f"/stake/{address}")


def current_stake_units(api: str, address: str) -> tuple[int, dict]:
    record = stake_record(api, address)
    return int(record.get("stake_units") or 0), record


def reconcile_stake(
    api: str,
    rpc: str,
    key_file: Path,
    address: str,
    target_units: int,
    timeout: float = 120.0,
) -> None:
    current, record = current_stake_units(api, address)
    log(f"stake before: {units_san(current)} SAN (target {units_san(target_units)} SAN)")
    if current == target_units:
        log("stake already matches the target; nothing to do")
        return

    if target_units > current:
        _deposit(api, rpc, key_file, address, target_units - current)
    else:
        if current > 0:
            log(
                "reducing stake: the node only supports an all-or-nothing "
                "undelegate, so this undelegates all, withdraws and re-stakes "
                f"{units_san(target_units)} SAN"
            )
            _withdraw_all(api, rpc, key_file, address, current, timeout)
        if target_units > 0:
            _deposit(api, rpc, key_file, address, target_units)

    final, _ = current_stake_units(api, address)
    log(f"stake after: {units_san(final)} SAN")
    if final != target_units:
        raise NodeToolError(
            f"stake reconciliation did not converge: expected "
            f"{units_san(target_units)} SAN, chain reports {units_san(final)} SAN"
        )


def _sancli_transaction(rpc: str, key_file: Path, command: list[str]) -> dict:
    result = sancli_json(["--rpc", rpc, "--key", str(key_file), *command])
    tx_id = result.get("tx_id", "")
    status = result.get("status", "")
    log(f"{command[0]}: status={status} tx={tx_id}")
    return result


def _deposit(api: str, rpc: str, key_file: Path, address: str, amount_units: int) -> None:
    before, _ = current_stake_units(api, address)
    amount = units_san(amount_units)
    log(f"depositing {amount} SAN")
    _sancli_transaction(rpc, key_file, ["stake", "--amount", amount])
    wait_for(
        lambda: current_stake_units(api, address)[0] >= before + amount_units,
        180.0,
        f"deposit of {amount} SAN to be committed",
    )
    log(f"deposit committed: stake is now {units_san(current_stake_units(api, address)[0])} SAN")


def _withdraw_all(api: str, rpc: str, key_file: Path, address: str, current_units: int, timeout: float) -> None:
    _sancli_transaction(rpc, key_file, ["undelegate"])
    wait_for(
        lambda: stake_record(api, address).get("release_height") is not None,
        timeout,
        "undelegate to be committed",
    )
    log("undelegate committed; unbonding period is 0 in dev, withdrawing now")
    _sancli_transaction(rpc, key_file, ["withdraw"])
    wait_for(
        lambda: current_stake_units(api, address)[0] == 0,
        timeout,
        "withdraw to return the stake to the wallet",
    )
    log(f"withdrew {units_san(current_units)} SAN (stake is now 0 SAN)")


# ---------------------------------------------------------------------- #
# Start / status
# ---------------------------------------------------------------------- #


def build_child_env(options: argparse.Namespace, address: str, allocation: str) -> dict:
    env = os.environ.copy()
    env["SAN_DB_BACKEND"] = "memory"
    env["SAN_HOST"] = options.host
    env["SAN_ADVERTISE_HOST"] = options.advertise or options.host
    env["SAN_API_PORT"] = str(options.api_port)
    env["SAN_P2P_PORT"] = str(options.p2p_port)
    env["SAN_PEER_PORT"] = str(options.peer_port)
    env["SAN_CONTROLLER_PORT"] = str(options.controller_port)
    env["SAN_DB_PATH"] = str(options.db)
    env["SAN_KEY_FILE"] = str(options.key)
    env["SAN_REWARD_ADDRESS"] = address
    env["SAN_BLOCK_THRESHOLD_FEE"] = str(options.block_threshold_fee)
    env["SAN_CONTROLLER_COUNT"] = str(options.controllers)
    if options.bootstrap:
        for name in GENESIS_PARAMETER_ENV:
            env.pop(name, None)
        env["SAN_BOOTSTRAP"] = options.bootstrap
    else:
        env["SAN_CHAIN_ID"] = options.chain_id
        env["SAN_GENESIS_ALLOCATION"] = allocation
        env["SAN_UNBONDING_PERIOD"] = str(options.unbonding)
        env["SAN_MIN_VALIDATOR_STAKE"] = options.min_stake
        env["SAN_BLOCK_REWARD"] = str(options.block_reward)
        env["SAN_MIN_BLOCK_INTERVAL_MS"] = "1000" if options.block_reward > 0 else "0"
    return env


def resolve_allocation(options: argparse.Namespace, address: str) -> str:
    entries: list[str] = []
    seen: set[str] = set()
    for raw in [f"{address}:{options.genesis_amount}", *options.genesis_alloc]:
        wallet, _, amount = raw.partition(":")
        wallet, amount = wallet.strip(), amount.strip()
        if not wallet or not amount:
            raise NodeToolError(f"malformed --genesis-alloc {raw!r}; expected ADDRESS:SAN")
        if wallet.lower() in seen:
            continue
        seen.add(wallet.lower())
        san_units(amount)
        entries.append(f"{wallet}:{amount}")
    return ",".join(entries)


def start_node(options: argparse.Namespace, address: str, allocation: str) -> None:
    command = [
        str(binary("sannode")),
        "--address", address,
        "--key", str(options.key),
        "--chain-id", options.chain_id,
        "--api-port", str(options.api_port),
        "--db", str(options.db),
        "--host", options.host,
        "--genesis-amount", str(options.genesis_amount),
        "--block-reward", str(options.block_reward),
        "--stake", "0",
    ]
    if options.bootstrap:
        command += ["--bootstrap", options.bootstrap]
    if options.genesis_alloc:
        command += ["--genesis-alloc", allocation]
    if options.expect_genesis_hash:
        command += ["--expect-genesis-hash", options.expect_genesis_hash]

    options.log.parent.mkdir(parents=True, exist_ok=True)
    options.pid_file.parent.mkdir(parents=True, exist_ok=True)
    log_file = open(options.log, "ab")
    kwargs: dict = {}
    if IS_WINDOWS:
        kwargs["creationflags"] = subprocess.DETACHED_PROCESS | subprocess.CREATE_NEW_PROCESS_GROUP
    else:
        kwargs["start_new_session"] = True
    process = subprocess.Popen(
        command,
        cwd=str(ROOT),
        env=build_child_env(options, address, allocation),
        stdout=log_file,
        stderr=subprocess.STDOUT,
        **kwargs,
    )
    options.pid_file.write_text(str(process.pid), encoding="utf-8")
    log(f"started sannode (pid {process.pid}, log {options.log})")

    api = api_url(options.api_port)
    try:
        wait_for(lambda: node_healthy(api), options.timeout, "the node to become healthy")
    except NodeToolError:
        log_file.close()
        stop_node(options.data_dir, options.pid_file, quiet=True)
        raise NodeToolError(
            f"the node did not become healthy; inspect the log: {options.log}"
        )
    log_file.close()


def enter_stake(api: str, options: argparse.Namespace, address: str) -> None:
    if options.stake is None:
        return
    reconcile_stake(
        api,
        api_url(options.api_port),
        options.key,
        address,
        san_units(options.stake),
    )


def print_status(options: argparse.Namespace, address: str | None) -> int:
    api = api_url(options.api_port)
    pid = read_pid(options.pid_file)
    running = pid > 0 and process_alive(pid) and node_healthy(api)
    if not running:
        log("node is not running")
        if address:
            log(f"wallet: {address} (key {options.key})")
            log(f"start it with: python scripts/go_node.py --wallet {address}")
        return 0

    health = get_json(api, "/health")
    log(f"node is running (pid {pid}) at {api}")
    if address:
        account = get_json(api, f"/account/{address}")
        stake, record = current_stake_units(api, address)
        active = any(
            validator.get("address") == address
            for validator in (get_json(api, "/validators").get("validators") or [])
        )
        log(f"  wallet           : {address}")
        log(f"  height           : {health.get('height')} (finalized {health.get('finalized_height')})")
        log(f"  peers            : {health.get('peers')} (validators {health.get('validators')})")
        log(f"  balance          : {account.get('balance')} SAN")
        log(
            f"  stake            : {units_san(stake)} SAN"
            f" ({stake} units){' [active validator]' if active else ''}"
        )
        if record.get("release_height") is not None:
            log(f"  unbonding        : releases at height {record.get('release_height')}")
    return 0


# ---------------------------------------------------------------------- #
# CLI
# ---------------------------------------------------------------------- #


def default_data_dir() -> Path:
    return DEFAULT_DATA_DIR


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="go_node.py",
        description="Start (or manage) the Go SAN node with one command.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "examples:\n"
            "  python scripts/go_node.py --wallet 0x...\n"
            "  python scripts/go_node.py --wallet 0x... --stake 100\n"
            "  python scripts/go_node.py --status\n"
            "  python scripts/go_node.py --stop\n"
        ),
    )
    parser.add_argument("--wallet", default=None, help="node wallet address (0x...); defaults to the key file address")
    parser.add_argument("--stake", default=None, help="desired on-chain stake in SAN (reconciled on start)")
    parser.add_argument("--stop", action="store_true", help="stop the node started from this data directory")
    parser.add_argument("--status", action="store_true", help="show height, address, stake and balance")
    parser.add_argument("--data-dir", default=None, help=f"node data directory (default {DEFAULT_DATA_DIR})")
    parser.add_argument("--key", default=None, help="node identity key file (default <data-dir>/san_key.json)")
    parser.add_argument("--api-port", type=int, default=DEFAULT_API_PORT)
    parser.add_argument("--p2p-port", type=int, default=DEFAULT_P2P_PORT)
    parser.add_argument("--peer-port", type=int, default=DEFAULT_PEER_PORT)
    parser.add_argument("--controller-port", type=int, default=DEFAULT_CONTROLLER_PORT)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--advertise", default=None, help="advertised host for peers (defaults to --host)")
    parser.add_argument("--chain-id", default="san-devnet-1")
    parser.add_argument("--bootstrap", default=None, help="seed REST endpoint host:port (joiner mode)")
    parser.add_argument("--expect-genesis-hash", default=None, help="pin the seed's genesis hash")
    parser.add_argument("--genesis-amount", default="10000", help="SAN funded to the wallet in founder mode")
    parser.add_argument(
        "--genesis-alloc",
        action="append",
        default=[],
        metavar="ADDRESS:SAN",
        help="extra founder allocation (repeatable), e.g. --genesis-alloc 0xabc:500",
    )
    parser.add_argument("--block-reward", type=float, default=2.0, help="SAN minted per block in founder mode")
    parser.add_argument("--unbonding", default="0", help="unbonding period in blocks (dev default 0)")
    parser.add_argument("--min-stake", default="0", help="minimum active validator stake in SAN (dev default 0)")
    parser.add_argument("--block-threshold-fee", default="0.0001", help="fee threshold that triggers a block")
    parser.add_argument(
        "--controllers",
        type=int,
        default=0,
        help="controller approval nodes (0 = accept blocks locally, the dev default)",
    )
    parser.add_argument("--log", default=None, help="node log file (default <data-dir>/node.log)")
    parser.add_argument("--pid-file", default=None, help="pid file (default <data-dir>/node.pid)")
    parser.add_argument("--timeout", type=float, default=90.0, help="seconds to wait for health")
    parser.add_argument("--no-build", action="store_true", help="never run go build")
    return parser


def resolve_paths(options: argparse.Namespace) -> None:
    data_dir = Path(options.data_dir).resolve() if options.data_dir else default_data_dir()
    options.data_dir = data_dir
    options.key = Path(options.key).resolve() if options.key else data_dir / "san_key.json"
    options.db = data_dir / "node.db"
    options.log = Path(options.log).resolve() if options.log else data_dir / "node.log"
    options.pid_file = Path(options.pid_file).resolve() if options.pid_file else data_dir / "node.pid"


def main(argv: list[str] | None = None) -> int:
    options = build_parser().parse_args(argv)
    resolve_paths(options)

    if not options.no_build:
        try:
            ensure_binaries()
        except NodeToolError as exc:
            print(f"[go_node] error: {exc}", file=sys.stderr)
            return 1

    if options.stop:
        stop_node(options.data_dir, options.pid_file)
        return 0

    address = None
    if options.key.exists():
        try:
            address = key_address(options.key)
        except NodeToolError as exc:
            print(f"[go_node] error: {exc}", file=sys.stderr)
            return 1
    elif not options.status:
        try:
            address, created = ensure_key(options.key)
        except NodeToolError as exc:
            print(f"[go_node] error: {exc}", file=sys.stderr)
            return 1
    else:
        address = None

    if options.status:
        try:
            return print_status(options, address)
        except NodeToolError as exc:
            print(f"[go_node] error: {exc}", file=sys.stderr)
            return 1

    try:
        address, created = ensure_key(options.key)
        if options.wallet:
            check_wallet(options.key, address, options.wallet.lower(), created)
        api = api_url(options.api_port)

        pid = read_pid(options.pid_file)
        already_running = pid > 0 and process_alive(pid) and node_healthy(api)
        if already_running:
            log(f"node already running (pid {pid}); reconciling stake only")
        else:
            allocation = resolve_allocation(options, address)
            log(f"wallet : {address}")
            if options.bootstrap:
                log(f"joining : {options.bootstrap}")
            else:
                log(f"founder: genesis allocation {allocation}")
            start_node(options, address, allocation)

        enter_stake(api, options, address)
        print_status(options, address)
        log("next: sancli --rpc http://127.0.0.1:%d health" % options.api_port)
        return 0
    except NodeToolError as exc:
        print(f"[go_node] error: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("[go_node] interrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
