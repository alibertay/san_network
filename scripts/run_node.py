#!/usr/bin/env python3
"""One-command devnet node.

Create (or join) a SAN devnet with a single command and have every block
reward and transaction tip credited to the address you choose:

    # founder: start a brand-new chain, premine the node, auto-stake
    python scripts/run_node.py --address 0xYourRewardAddress

    # joiner: same command plus the seed's REST endpoint
    python scripts/run_node.py --address 0xYourRewardAddress \\
        --bootstrap 127.0.0.1:8000

What it does for you:

1. Loads (or generates) the node identity key file.
2. Founder mode: writes the genesis allocation (premine) for the node so it
   can stake, sets ``SAN_BLOCK_REWARD`` and routes rewards to ``--address``.
   Joiner mode: fetches ``/genesis`` from the seed and reproduces it exactly.
3. Starts the node (REST API + gRPC P2P) with a persistent LMDB database.
4. Waits for the node to be healthy and submits a validator ``deposit`` so
   finality starts advancing, unless ``--stake 0`` is given.

Everything can still be overridden with the usual SAN_* environment
variables; values already present in the environment win.
"""

from __future__ import annotations

import argparse
import os
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from blockchain.address import AddressError, normalize_address  # noqa: E402
from blockchain.economics import units_to_san  # noqa: E402
from blockchain.identity import IdentityError, NodeIdentity  # noqa: E402


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--address", required=True, help="reward address (0x...)")
    parser.add_argument("--key", default="node_key.json", help="node identity key file")
    parser.add_argument("--chain-id", default="san-devnet-1")
    parser.add_argument("--api-port", type=int, default=None, help="REST port (default 8000)")
    parser.add_argument("--db", default=None, help="LMDB database file (default data/<key>.kv)")
    parser.add_argument("--bootstrap", default=None, help="seed REST endpoint host:port")
    parser.add_argument(
        "--expect-genesis-hash",
        default=None,
        help="pin the expected genesis hash (recommended when joining)",
    )
    parser.add_argument(
        "--genesis-amount",
        default="1000000",
        help="SAN premined to this node in founder mode",
    )
    parser.add_argument("--genesis-alloc", default=None, help="shared allocation (overrides fetching)")
    parser.add_argument("--stake", type=float, default=1000.0, help="validator deposit in SAN (0 = skip)")
    parser.add_argument("--block-reward", type=float, default=2.0, help="SAN minted per block (founder only)")
    parser.add_argument("--host", default="127.0.0.1")
    return parser.parse_args()


def env_default(name: str, value: str) -> None:
    if name not in os.environ or not os.environ[name]:
        os.environ[name] = value


def fetch_genesis(bootstrap: str, timeout: float = 5.0) -> dict | None:
    """Fetch the public genesis data from the seed's /genesis endpoint."""
    import requests

    url = bootstrap if "://" in bootstrap else f"http://{bootstrap}"
    try:
        response = requests.get(f"{url.rstrip('/')}/genesis", timeout=timeout)
        response.raise_for_status()
        payload = response.json()
    except Exception as exc:  # noqa: BLE001 - the caller decides what to do
        print(f"[run_node] could not fetch genesis from {url}: {exc}")
        return None

    if not payload.get("genesis_allocation"):
        print("[run_node] seed reported an empty genesis allocation")
        return None
    print(
        f"[run_node] fetched genesis from {url} "
        f"(chain_id={payload.get('chain_id')}, "
        f"{len(payload['genesis_allocation'])} allocation(s))"
    )
    return payload


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


def check_genesis_environment() -> None:
    """Refuse to join when local env would build a different genesis."""
    conflicting = [name for name in GENESIS_PARAMETER_ENV if os.environ.get(name)]
    if conflicting:
        raise SystemExit(
            "[run_node] these environment variables would override the seed's "
            f"genesis parameters: {', '.join(conflicting)}. Unset them or pass "
            "--genesis-alloc explicitly."
        )


def apply_genesis_parameters(payload: dict) -> str:
    """Reproduce the seed's genesis parameters locally (env defaults only)."""
    parameters = payload.get("parameters") or {}
    units_to_env = {
        "block_reward": "SAN_BLOCK_REWARD",
        "min_validator_stake": "SAN_MIN_VALIDATOR_STAKE",
    }
    for key, env_name in units_to_env.items():
        if key in parameters:
            env_default(env_name, str(units_to_san(int(parameters[key]))))
    int_to_env = {
        "unbonding_period": "SAN_UNBONDING_PERIOD",
        "slash_bps": "SAN_SLASH_BPS",
        "block_gas_limit": "SAN_BLOCK_GAS_LIMIT",
    }
    for key, env_name in int_to_env.items():
        if key in parameters:
            env_default(env_name, str(int(parameters[key])))
    if "proposer_timeout_ms" in parameters:
        env_default("SAN_PROPOSER_TIMEOUT", str(int(parameters["proposer_timeout_ms"]) / 1000))
    if "min_block_interval_ms" in parameters:
        env_default("SAN_MIN_BLOCK_INTERVAL_MS", str(int(parameters["min_block_interval_ms"])))
    if payload.get("chain_id"):
        env_default("SAN_CHAIN_ID", str(payload["chain_id"]))

    return ",".join(
        f"{address}:{units_to_san(int(units))}"
        for address, units in payload["genesis_allocation"].items()
    )


def _seed_height(bootstrap: str | None) -> int | None:
    """Best-height hint from the seed's REST API (None when unreachable)."""
    if not bootstrap:
        return None
    import requests

    url = bootstrap if "://" in bootstrap else f"http://{bootstrap}"
    try:
        response = requests.get(f"{url.rstrip('/')}/health", timeout=3)
        response.raise_for_status()
        return int(response.json().get("height", 0))
    except Exception:  # noqa: BLE001 - progress reporting is best effort
        return None


def supervise_node(
    api_port: int,
    key_file: str,
    amount: float,
    expected_genesis: str | None = None,
    bootstrap: str | None = None,
    genesis_pinned: bool = False,
) -> None:
    """Wait for health, verify the chain, report sync progress, then stake."""
    from sdk.client import SanClient, SanClientError

    base_url = f"http://127.0.0.1:{api_port}"
    client = SanClient(base_url=base_url)
    identity = NodeIdentity.load(key_file)

    for _ in range(240):
        try:
            if client.health():
                break
        except Exception:  # noqa: BLE001 - node still starting
            time.sleep(0.5)
    else:
        print("[run_node] node never became healthy; check the log", file=sys.stderr)
        return

    if expected_genesis:
        try:
            local = SanClient(base_url=base_url).genesis()
        except Exception as exc:  # noqa: BLE001
            print(f"[run_node] could not read the local genesis: {exc}")
            os._exit(1)
        if local.get("genesis_hash") != expected_genesis:
            print(
                "[run_node] FATAL: local genesis hash "
                f"{local.get('genesis_hash')} does not match the expected "
                f"{expected_genesis}; refusing to run on the wrong chain",
                file=sys.stderr,
            )
            os._exit(1)
        source = "the pinned hash" if genesis_pinned else "the seed's reported hash"
        print(f"[run_node] local genesis verified against {source}")

    # Discovery + longest-chain sync report (the node itself discovers peers,
    # picks the longest compatible chain and replays it; we just show progress).
    if bootstrap is None:
        # Founder node: nothing to sync against; report the fresh state once.
        health = client.health()
        print(
            "[run_node] node is up and producing: "
            f"height={health.get('height')} finalized={health.get('finalized_height')}"
        )
    seed_height = _seed_height(bootstrap)
    last_height = -1
    stable = 0
    for _ in range(300 if bootstrap is not None else 0):
        try:
            health = client.health()
        except Exception:  # noqa: BLE001
            time.sleep(0.5)
            continue
        height = int(health.get("height", 0))
        peers = int(health.get("peers", 0))
        if height != last_height:
            target = f" (best peer: {seed_height})" if seed_height is not None else ""
            print(f"[run_node] syncing... height={height}{target} peers={peers}")
            stable = 0
        else:
            stable += 1
        last_height = height
        if seed_height is not None and height >= seed_height:
            break
        if seed_height is None and stable >= 4:
            break
        time.sleep(0.5)

    if bootstrap is not None:
        finished = client.health()
        print(
            "[run_node] node is up and synced: "
            f"height={finished.get('height')} peers={finished.get('peers')} "
            f"finalized={finished.get('finalized_height')}"
        )

    if amount <= 0:
        return

    try:
        client = SanClient(base_url=base_url, identity=identity)
        address = identity_address(identity)
        account = client.account(address)
        balance_san = float(account.get("balance", 0))
        active = {v["address"] for v in client.validators().get("validators", [])}
        if address in active:
            print("[run_node] validator already active; stake skipped")
            return
        if balance_san < amount:
            print(
                f"[run_node] balance {balance_san:.6f} SAN is below the stake "
                f"({amount} SAN); ask a funded account to send you coins, then "
                f"run: python -m sdk.cli --key {key_file} stake --amount {amount}"
            )
            return
        result = client.deposit_stake(amount)
        print(f"[run_node] validator stake submitted: {result}")
    except SanClientError as exc:
        print(f"[run_node] staking failed: {exc}")


def identity_address(identity: NodeIdentity) -> str:
    from blockchain.address import address_from_public_key

    return address_from_public_key(identity.public_key)


def main() -> int:
    args = parse_args()

    try:
        args.address = normalize_address(args.address)
    except AddressError as exc:
        print(f"[run_node] invalid --address: {exc}", file=sys.stderr)
        return 2

    try:
        identity = NodeIdentity.load(args.key)
    except IdentityError as exc:
        print(f"[run_node] cannot load identity {args.key}: {exc}", file=sys.stderr)
        return 2

    api_port = args.api_port or int(os.environ.get("SAN_API_PORT", "8000"))
    db_path = args.db or f"data/{Path(args.key).stem}.kv"

    allocation = args.genesis_alloc
    payload: dict | None = None
    if args.bootstrap:
        check_genesis_environment()
        if not allocation or args.expect_genesis_hash:
            payload = fetch_genesis(args.bootstrap)
            if not payload and not allocation:
                print(
                    "[run_node] joiner mode needs the seed's genesis: pass "
                    "--genesis-alloc or make the seed's REST endpoint reachable",
                    file=sys.stderr,
                )
                return 2
        if payload:
            if args.expect_genesis_hash:
                if payload.get("genesis_hash") != args.expect_genesis_hash:
                    print(
                        "[run_node] genesis hash mismatch: seed reports "
                        f"{payload.get('genesis_hash')}, expected "
                        f"{args.expect_genesis_hash}",
                        file=sys.stderr,
                    )
                    return 2
                print(f"[run_node] genesis hash matches the pinned {args.expect_genesis_hash}")
            elif not allocation:
                print(
                    "[run_node] WARNING: joining on trust-on-first-use; pin the "
                    "seed with --expect-genesis-hash for a verified join"
                )
            if not allocation:
                allocation = apply_genesis_parameters(payload)
    else:
        allocation = f"{identity.public_key_hex}:{args.genesis_amount}"
        print(f"[run_node] founder mode: premine {args.genesis_amount} SAN to the node identity")

    env_default("SAN_CHAIN_ID", args.chain_id)
    env_default("SAN_HOST", args.host)
    env_default("SAN_ADVERTISE_HOST", args.host)
    env_default("SAN_API_PORT", str(api_port))
    env_default("SAN_P2P_PORT", str(api_port + 700))
    env_default("SAN_PEER_PORT", str(api_port + 710))
    env_default("SAN_CONTROLLER_PORT", str(api_port + 720))
    env_default("SAN_DB_PATH", str(db_path))
    env_default("SAN_KEY_FILE", args.key)
    env_default("SAN_GENESIS_ALLOCATION", allocation)
    env_default("SAN_REWARD_ADDRESS", args.address)
    env_default("SAN_BLOCK_THRESHOLD_FEE", "0.0001")
    if not args.bootstrap:
        env_default("SAN_BLOCK_REWARD", str(args.block_reward))
        if args.block_reward > 0:
            env_default("SAN_MIN_BLOCK_INTERVAL_MS", "1000")
    if args.bootstrap:
        env_default("SAN_BOOTSTRAP", args.bootstrap)

    import logging

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s [%(name)s] %(message)s")

    print(
        "[run_node] starting\n"
        f"  reward address : {args.address}\n"
        f"  node identity  : {args.key}\n"
        f"  REST API       : http://{args.host}:{api_port}\n"
        f"  P2P (gRPC)     : {args.host}:{api_port + 700}, peer {api_port + 710}, controller {api_port + 720}\n"
        f"  database       : {db_path}\n"
        f"  chain id       : {os.environ['SAN_CHAIN_ID']}\n"
        f"  block reward   : {os.environ.get('SAN_BLOCK_REWARD', '0')} SAN"
    )

    pinned_genesis = args.expect_genesis_hash or (payload or {}).get("genesis_hash")

    threading.Thread(
        target=supervise_node,
        args=(
            api_port,
            args.key,
            args.stake,
            pinned_genesis,
            args.bootstrap,
            bool(args.expect_genesis_hash),
        ),
        daemon=True,
    ).start()

    import uvicorn

    from app.main import app

    os.makedirs(os.path.dirname(db_path) or ".", exist_ok=True)
    uvicorn.run(app, host=args.host, port=api_port, workers=1)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
