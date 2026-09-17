#!/usr/bin/env python3
"""Genesis bootstrap for a brand-new SAN devnet.

A new chain has no coins: the genesis allocation *is* the premine. This helper
generates the node identities FIRST (a node that auto-generates its key at
startup uses a random key that cannot be premined), prints the shared
``SAN_GENESIS_ALLOCATION`` string, and the exact commands to start each node.

Examples::

    # one founder node, 1,000,000 SAN premined to itself
    python scripts/genesis_bootstrap.py --count 1

    # a five-node devnet (each node premined 1,000,000 SAN), custom ports
    python scripts/genesis_bootstrap.py --count 5 --chain-id san-devnet-1

    # write keys + env files under ./devnet
    python scripts/genesis_bootstrap.py --count 3 --dir devnet --write-env

Then start the seed node with its printed command, and every other node with
``--bootstrap <seed-api-port>`` (also printed).
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from blockchain.address import address_from_public_key  # noqa: E402
from blockchain.identity import NodeIdentity  # noqa: E402


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--count", type=int, default=1, help="number of nodes")
    parser.add_argument("--dir", default="devnet-keys", help="key output directory")
    parser.add_argument("--chain-id", default="san-devnet-1")
    parser.add_argument(
        "--amount",
        default="1000000",
        help="SAN premined to every node in the genesis allocation",
    )
    parser.add_argument("--api-port", type=int, default=8000, help="first API port")
    parser.add_argument("--write-env", action="store_true", help="write devnet.env files")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.count < 1:
        print("--count must be at least 1", file=sys.stderr)
        return 2

    key_dir = Path(args.dir)
    key_dir.mkdir(parents=True, exist_ok=True)

    identities = []
    for index in range(1, args.count + 1):
        key_file = key_dir / f"key{index}.json"
        identity = NodeIdentity.load(str(key_file))
        identities.append(identity)
        print(
            f"node {index}: key={key_file}  "
            f"address={address_from_public_key(identity.public_key)}"
        )
        print(f"          pubkey={identity.public_key_hex}")

    allocation = ",".join(f"{ident.public_key_hex}:{args.amount}" for ident in identities)
    seed_port = args.api_port

    print("\n# 1) Everyone must share this exact string (it defines genesis):")
    print(f'export SAN_GENESIS_ALLOCATION="{allocation}"')
    print(f'export SAN_CHAIN_ID="{args.chain_id}"')

    print("\n# 2) Start the seed node (node 1):")
    print(
        "SAN_API_PORT={api} SAN_P2P_PORT={p2p} SAN_PEER_PORT={peer} "
        "SAN_CONTROLLER_PORT={ctrl} \\\n"
        'SAN_CHAIN_ID="{chain}" SAN_DB_PATH="{dir}/node1.kv" '
        'SAN_KEY_FILE="{dir}/key1.json" \\\n'
        'SAN_GENESIS_ALLOCATION="{alloc}" SAN_BLOCK_THRESHOLD_FEE=0.0001 \\\n'
        "python run.py".format(
            api=seed_port,
            p2p=seed_port + 700,
            peer=seed_port + 710,
            ctrl=seed_port + 720,
            chain=args.chain_id,
            dir=key_dir,
            alloc=allocation,
        )
    )

    for index in range(2, args.count + 1):
        api = seed_port + index - 1
        print(f"\n# 3.{index - 1}) Start node {index} (bootstrap to node 1):")
        print(
            "SAN_API_PORT={api} SAN_P2P_PORT={p2p} SAN_PEER_PORT={peer} "
            "SAN_CONTROLLER_PORT={ctrl} \\\n"
            'SAN_CHAIN_ID="{chain}" SAN_DB_PATH="{dir}/node{index}.kv" '
            'SAN_KEY_FILE="{dir}/key{index}.json" \\\n'
            'SAN_GENESIS_ALLOCATION="{alloc}" SAN_BLOCK_THRESHOLD_FEE=0.0001 \\\n'
            "SAN_BOOTSTRAP=127.0.0.1:{seed} \\\n"
            "python run.py".format(
                api=api,
                p2p=api + 700,
                peer=api + 710,
                ctrl=api + 720,
                chain=args.chain_id,
                dir=key_dir,
                index=index,
                alloc=allocation,
                seed=seed_port,
            )
        )

    print(
        "\n# 4) Activate validators (each node, from its own key directory):\n"
        f"#    python -m sdk.cli --rpc http://127.0.0.1:{seed_port} "
        f"--key {key_dir}/key1.json stake --amount 1000\n"
        "#    Once >=1 validator is active, /finality starts advancing."
    )

    if args.write_env:
        for index, identity in enumerate(identities, start=1):
            api = seed_port + index - 1
            env_file = key_dir / f"node{index}.env"
            lines = [
                f"SAN_CHAIN_ID={args.chain_id}",
                "SAN_HOST=127.0.0.1",
                "SAN_ADVERTISE_HOST=127.0.0.1",
                f"SAN_API_PORT={api}",
                f"SAN_P2P_PORT={api + 700}",
                f"SAN_PEER_PORT={api + 710}",
                f"SAN_CONTROLLER_PORT={api + 720}",
                f"SAN_DB_PATH={key_dir}/node{index}.kv",
                f"SAN_KEY_FILE={key_dir}/key{index}.json",
                f'SAN_GENESIS_ALLOCATION="{allocation}"',
                "SAN_BLOCK_THRESHOLD_FEE=0.0001",
            ]
            if index > 1:
                lines.append(f"SAN_BOOTSTRAP=127.0.0.1:{seed_port}")
            env_file.write_text("\n".join(lines) + "\n")
            print(f"wrote {env_file}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
