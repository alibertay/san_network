#!/usr/bin/env python3
"""End-to-end verification of the Go SAN node (test-only).

This harness is NOT required to run a node; it exists to prove the Go
implementation works. It launches three nodes through scripts/go_node.py,
exercises transfers, SANRC20 tokens, a custom contract and the stake
reconciliation (0 -> 100 -> 70 with restarts), then cleans everything up.

    python tools/go_e2e_check.py

Exits 0 only when every step passes.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import time
from decimal import Decimal
from pathlib import Path
from urllib import error as urlerror
from urllib import request as urlrequest

ROOT = Path(__file__).resolve().parents[1]
BRINGUP = ROOT / "scripts" / "go_node.py"
SANCLI = ROOT / "bin" / ("sancli.exe" if os.name == "nt" else "sancli")
DATA_ROOT = ROOT / "data" / "go-e2e"
SAN_BASE = 100_000_000

A = {
    "name": "A-seed",
    "data": DATA_ROOT / "a",
    "api": 18000,
    "p2p": 18700,
    "peer": 18710,
    "controller": 18720,
}
B = {
    "name": "B",
    "data": DATA_ROOT / "b",
    "api": 18010,
    "p2p": 18730,
    "peer": 18740,
    "controller": 18750,
}
C = {
    "name": "C",
    "data": DATA_ROOT / "c",
    "api": 18020,
    "p2p": 18760,
    "peer": 18770,
    "controller": 18780,
}

RESULTS: list[tuple[str, bool, str]] = []


def api_url(node: dict) -> str:
    return f"http://127.0.0.1:{node['api']}"


def log(message: str) -> None:
    print(message, flush=True)


def get_json(node: dict, path: str, timeout: float = 10.0) -> dict:
    url = f"{api_url(node)}{path}"
    with urlrequest.urlopen(url, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def try_get(node: dict, path: str, timeout: float = 2.0) -> dict | None:
    try:
        return get_json(node, path, timeout=timeout)
    except (urlerror.URLError, OSError, ValueError, json.JSONDecodeError):
        return None


def wait_until(predicate, timeout: float, description: str, interval: float = 0.4):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval)
    raise TimeoutError(f"timed out after {timeout:.0f}s waiting for {description} (last={last!r})")


def san(units: int) -> str:
    text = format(Decimal(units) / (Decimal(10) ** 8), "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def balance_units(node: dict, address: str) -> int:
    return int(get_json(node, f"/account/{address}")["balance_units"])


def stake_units(node: dict, address: str) -> int:
    return int(get_json(node, f"/stake/{address}").get("stake_units") or 0)


def run_bringup(node: dict, *extra: str) -> subprocess.CompletedProcess:
    command = [
        sys.executable,
        str(BRINGUP),
        "--data-dir", str(node["data"]),
        "--api-port", str(node["api"]),
        "--p2p-port", str(node["p2p"]),
        "--peer-port", str(node["peer"]),
        "--controller-port", str(node["controller"]),
        *extra,
    ]
    completed = subprocess.run(command, cwd=str(ROOT), capture_output=True, text=True)
    for line in (completed.stdout + completed.stderr).splitlines():
        log(f"    | {line}")
    if completed.returncode != 0:
        raise RuntimeError(f"go_node.py exited with {completed.returncode}")
    return completed


def sancli(node: dict, key: Path | None, *args: str) -> str:
    command = [str(SANCLI), "--rpc", api_url(node)]
    if key is not None:
        command += ["--key", str(key)]
    completed = subprocess.run(
        [*command, *args],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"sancli {' '.join(args)} failed: {completed.stderr.strip() or completed.stdout.strip()}")
    return completed.stdout.strip()


def wallet_address(key: Path) -> str:
    completed = subprocess.run(
        [str(SANCLI), "wallet", "address", "--key", str(key)],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"wallet address failed: {completed.stderr.strip()}")
    return json.loads(completed.stdout)["address"]


def ensure_key(key: Path) -> None:
    if key.exists():
        return
    completed = subprocess.run(
        [str(SANCLI), "wallet", "new", "--out", str(key)],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        raise RuntimeError(f"wallet new failed: {completed.stderr.strip()}")


def query(node: dict, contract_id: str, function: str, params: list) -> object:
    args = ["query", "--id", contract_id, "--function", function]
    for param in params:
        args += ["--param", str(param)]
    output = sancli(node, None, *args)
    return json.loads(output)


def call(node: dict, key: Path, contract_id: str, function: str, params: list) -> str:
    args = ["call", "--id", contract_id, "--function", function]
    for param in params:
        args += ["--param", str(param)]
    return sancli(node, key, *args)


def wait_synced(node: dict, timeout: float = 60.0) -> None:
    """Wait until a node's local view reaches the seed's height.

    sancli reads the nonce from the node it talks to, so a lagging joiner can
    otherwise sign a duplicate nonce right after a transaction was committed
    somewhere else in the devnet.
    """

    def target() -> int:
        return int((try_get(A, "/health") or {}).get("height", 0))

    wait_until(
        lambda: int((try_get(node, "/health") or {}).get("height", -1)) >= target(),
        timeout,
        f"{node['name']} to sync with the seed",
    )


def stop_all() -> None:
    for node in (A, B, C):
        subprocess.run(
            [
                sys.executable, str(BRINGUP),
                "--data-dir", str(node["data"]),
                "--api-port", str(node["api"]),
                "--stop",
            ],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
        )


def ports_closed() -> bool:
    for port in (A["api"], B["api"], C["api"]):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.5)
            if probe.connect_ex(("127.0.0.1", port)) == 0:
                return False
    return True


def assert_ports_free() -> None:
    busy = []
    for node in (A, B, C):
        for port in (node["api"], node["p2p"], node["peer"], node["controller"]):
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
                probe.settimeout(0.5)
                if probe.connect_ex(("127.0.0.1", port)) == 0:
                    busy.append(port)
    if busy:
        raise RuntimeError(f"ports already in use (another node?): {sorted(busy)}")


# ---------------------------------------------------------------------- #
# Steps
# ---------------------------------------------------------------------- #


def step(name: str, action) -> bool:
    log(f"\n=== {name} ===")
    try:
        action()
    except Exception as exc:  # noqa: BLE001 - the report needs every failure
        RESULTS.append((name, False, str(exc)))
        log(f"FAIL {name}: {exc}")
        return False
    RESULTS.append((name, True, ""))
    log(f"PASS {name}")
    return True


def setup() -> dict:
    stop_all()
    assert_ports_free()
    if DATA_ROOT.exists():
        import shutil

        shutil.rmtree(DATA_ROOT, ignore_errors=True)
    for node in (A, B, C):
        node["data"].mkdir(parents=True, exist_ok=True)
        node["key"] = node["data"] / "san_key.json"
    run_bringup(A, "--status")
    for node in (A, B, C):
        ensure_key(node["key"])
        node["address"] = wallet_address(node["key"])
    return {node["name"]: node["address"] for node in (A, B, C)}


def step0_start_devnet() -> None:
    keys = {node["name"]: node["address"] for node in (A, B, C)}
    log(f"wallets: A={keys['A-seed']} B={keys['B']} C={keys['C']}")

    log("\n-- starting the founder seed A with --stake 0 (bootstrap proposer) --")
    run_bringup(
        A,
        "--wallet", A["address"],
        "--stake", "0",
        "--genesis-alloc", f"{B['address']}:500",
        "--genesis-alloc", f"{C['address']}:500",
    )
    wait_until(lambda: (try_get(A, "/health") or {}).get("height", 0) >= 1, 60, "seed A to produce a block")
    before = balance_units(A, A["address"])
    wait_until(lambda: balance_units(A, A["address"]) > before, 30,
               "seed A to earn a block reward at 0 stake")
    after = balance_units(A, A["address"])
    log(
        f"0-stake block reward proof: A balance {san(before)} -> {san(after)} SAN "
        f"(+{san(after - before)}; with no active validator any node may propose and the "
        "subsidy/tips go to its reward address)"
    )

    log("\n-- reconciling A to --stake 100 while the node keeps running --")
    run_bringup(A, "--wallet", A["address"], "--stake", "100")
    wait_until(lambda: stake_units(A, A["address"]) == 100 * SAN_BASE, 60, "A stake to reach 100 SAN")

    log("\n-- starting joiners B and C via --bootstrap --")
    run_bringup(B, "--wallet", B["address"], "--bootstrap", f"127.0.0.1:{A['api']}", "--stake", "0")
    run_bringup(C, "--wallet", C["address"], "--bootstrap", f"127.0.0.1:{A['api']}", "--stake", "0")
    for node in (B, C):
        wait_until(lambda n=node: (try_get(n, "/health") or {}).get("height", -1) >= 1, 90,
                   f"{node['name']} to sync")
    log(f"heights: A={get_json(A, '/health')['height']} B={get_json(B, '/health')['height']} C={get_json(C, '/health')['height']}")


def step1_transfers() -> None:
    before_b = balance_units(A, B["address"])
    log(f"A -> B 250 SAN; B before = {san(before_b)} SAN")
    wait_synced(A)
    result = json.loads(sancli(A, A["key"], "send", "--to", B["address"], "--value", "250"))
    wait_until(lambda: balance_units(A, B["address"]) == before_b + 250 * SAN_BASE, 60, "B balance +250")
    log(f"tx1 {result.get('tx_id')} status={result.get('status')}; B after = {san(balance_units(A, B['address']))} SAN")
    assert balance_units(A, B["address"]) == before_b + 250 * SAN_BASE

    before_c = balance_units(A, C["address"])
    log(f"B -> C 100 SAN; C before = {san(before_c)} SAN")
    wait_synced(B)
    result = json.loads(sancli(B, B["key"], "send", "--to", C["address"], "--value", "100"))
    wait_until(lambda: balance_units(A, C["address"]) == before_c + 100 * SAN_BASE, 60, "C balance +100")
    log(f"tx2 {result.get('tx_id')} status={result.get('status')}; C after = {san(balance_units(A, C['address']))} SAN")
    assert balance_units(A, C["address"]) == before_c + 100 * SAN_BASE


def step2_sanrc20() -> None:
    source = ROOT / "PENA" / "examples" / "SANRC20" / "SANRC20.pena"
    result = json.loads(sancli(A, A["key"], "deploy", "--id", "sanrc20", "--file", str(source)))
    wait_until(lambda: "sanrc20" in get_json(A, "/contracts")["contracts"], 60, "sanrc20 deploy")
    log(f"deploy tx {result.get('tx_id')} status={result.get('status')}")

    call(A, A["key"], "sanrc20", "init", ["SAN Token", "SANRC20", 18, 1_000_000, A["address"]])
    wait_until(lambda: query(A, "sanrc20", "name", []) == "SAN Token", 60, "sanrc20 init")
    log(
        "init: name=%s symbol=%s totalSupply=%s balanceOf(A)=%s"
        % (
            query(A, "sanrc20", "name", []),
            query(A, "sanrc20", "symbol", []),
            query(A, "sanrc20", "totalSupply", []),
            query(A, "sanrc20", "balanceOf", [A["address"]]),
        )
    )

    call(A, A["key"], "sanrc20", "transfer", [A["address"], B["address"], 150])
    wait_until(lambda: query(A, "sanrc20", "balanceOf", [B["address"]]) == 150, 60, "token transfer")
    log(
        "transfer 150: balanceOf(A)=%s balanceOf(B)=%s"
        % (query(A, "sanrc20", "balanceOf", [A["address"]]), query(A, "sanrc20", "balanceOf", [B["address"]]))
    )
    assert query(A, "sanrc20", "balanceOf", [A["address"]]) == 999_850
    assert query(A, "sanrc20", "balanceOf", [B["address"]]) == 150

    call(A, A["key"], "sanrc20", "mint", [A["address"], C["address"], 500])
    wait_until(lambda: query(A, "sanrc20", "balanceOf", [C["address"]]) == 500, 60, "token mint")
    log(
        "mint 500: totalSupply=%s balanceOf(C)=%s"
        % (query(A, "sanrc20", "totalSupply", []), query(A, "sanrc20", "balanceOf", [C["address"]]))
    )
    assert query(A, "sanrc20", "totalSupply", []) == 1_000_500
    assert query(A, "sanrc20", "balanceOf", [C["address"]]) == 500


CUSTOM_CONTRACT = """data := {}
counter := 0

function set(k, v) {
  data[k] = v
}
function get(k) {
  return data[k]
}
function inc() {
  counter = counter + 1
  return counter
}
function count() {
  return counter
}
"""


def step3_custom_contract() -> None:
    source_path = DATA_ROOT / "kv.pena"
    source_path.write_text(CUSTOM_CONTRACT, encoding="utf-8")
    wait_synced(C)
    result = json.loads(sancli(C, C["key"], "deploy", "--id", "kv", "--file", str(source_path)))
    wait_until(lambda: "kv" in get_json(A, "/contracts")["contracts"], 60, "kv deploy")
    wait_until(lambda: "kv" in get_json(C, "/contracts")["contracts"], 60, "kv deploy on C")
    log(f"deploy tx {result.get('tx_id')} status={result.get('status')}")

    address = C["address"]
    expected_nonce = 1

    def send_next(*args: str) -> None:
        nonlocal expected_nonce
        wait_synced(C)
        wait_until(
            lambda: int(get_json(C, f"/account/{address}")["nonce"]) >= expected_nonce,
            60,
            "C nonce to catch up",
        )
        sancli(C, C["key"], *args)
        expected_nonce += 1

    send_next("call", "--id", "kv", "--function", "set", "--param", "alpha", "--param", "42")
    wait_until(lambda: query(A, "kv", "get", ["alpha"]) == 42, 60, "kv set/get")
    send_next("call", "--id", "kv", "--function", "inc")
    wait_until(lambda: query(A, "kv", "count", []) == 1, 60, "kv inc")
    send_next("call", "--id", "kv", "--function", "inc")
    wait_until(lambda: query(A, "kv", "count", []) == 2, 60, "kv inc again")
    log(
        "custom contract kv: set('alpha',42) -> get('alpha')=%s; inc()x2 -> count()=%s"
        % (query(A, "kv", "get", ["alpha"]), query(A, "kv", "count", []))
    )
    assert query(A, "kv", "get", ["alpha"]) == 42
    assert query(A, "kv", "count", []) == 2


def step4_stake_demo() -> None:
    address = B["address"]
    log(f"on-chain stake for B ({address}) before step 4: {san(stake_units(A, address))} SAN")

    log("\n-- run 1: B already at --stake 0 --")
    run_bringup(B, "--wallet", address, "--bootstrap", f"127.0.0.1:{A['api']}", "--stake", "0")
    assert stake_units(A, address) == 0
    log(f"on-chain stake after 0: {san(stake_units(A, address))} SAN")

    log("\n-- run 2: restart B with --stake 100 (joiner re-syncs from the seed) --")
    run_bringup(B, "--stop")
    run_bringup(B, "--wallet", address, "--bootstrap", f"127.0.0.1:{A['api']}", "--stake", "100")
    wait_until(lambda: stake_units(A, address) == 100 * SAN_BASE, 90, "B stake 100")
    log(f"on-chain stake after 100: {san(stake_units(A, address))} SAN (active validator: "
        f"{any(v['address'] == address for v in get_json(A, '/validators')['validators'])})")

    log("\n-- run 3: restart B with --stake 70 (undelegate-all + withdraw + re-stake 70) --")
    run_bringup(B, "--stop")
    run_bringup(B, "--wallet", address, "--bootstrap", f"127.0.0.1:{A['api']}", "--stake", "70")
    wait_until(lambda: stake_units(A, address) == 70 * SAN_BASE, 90, "B stake 70")
    log(f"on-chain stake after 70: {san(stake_units(A, address))} SAN; validators: "
        f"{[v['address'][:10] + '...' for v in get_json(A, '/validators')['validators']]}")
    assert stake_units(A, address) == 70 * SAN_BASE

    log(
        "\n0-stake reward note: in step 0 the seed earned SAN at stake 0 because a chain "
        "with no active validator lets any node propose; the subsidy and tips are credited "
        "to the block's reward address. Once A is an active validator it is the only "
        "proposer, so B/C at 0 stake do not earn rewards themselves."
    )


def main() -> int:
    started = time.monotonic()
    log("SAN Network Go node e2e check")
    wallets = {}
    try:
        wallets = setup()
        step0_start_devnet()
        steps = [
            ("1. wallet transfer", step1_transfers),
            ("2. SANRC20 token", step2_sanrc20),
            ("3. custom contract", step3_custom_contract),
            ("4. stake 0 -> 100 -> 70", step4_stake_demo),
        ]
        for name, action in steps:
            step(name, action)
    except Exception as exc:  # noqa: BLE001 - report setup failures too
        log(f"\nFATAL during setup: {exc}")
        RESULTS.append(("setup/devnet", False, str(exc)))
    finally:
        log("\n=== cleanup ===")
        stop_all()
        for _ in range(40):
            if ports_closed():
                break
            time.sleep(0.25)
        log(f"ports closed: {ports_closed()}")

    log("\n=== summary ===")
    failures = 0
    for name, ok, detail in RESULTS:
        log(f"{'PASS' if ok else 'FAIL'} {name}" + (f" - {detail}" if detail else ""))
        failures += 0 if ok else 1
    log(f"{len(RESULTS) - failures} passed, {failures} failed in {time.monotonic() - started:.1f}s")
    return 0 if failures == 0 and RESULTS else 1


if __name__ == "__main__":
    raise SystemExit(main())
