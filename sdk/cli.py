"""Command line wallet / explorer for SAN Network.

Examples::

    python -m sdk.cli --rpc http://127.0.0.1:8000 health
    python -m sdk.cli balance --address 0x...
    python -m sdk.cli --key san_key.json send --to 0x... --value 5
    python -m sdk.cli --key san_key.json deploy --id kv --file kv.pena
    python -m sdk.cli query --id kv --function get
    python -m sdk.cli metrics
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from blockchain.identity import NodeIdentity
from sdk.client import SanClient, SanClientError


def _coerce(value: str):
    try:
        return int(value)
    except ValueError:
        return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="sdk.cli", description="SAN Network wallet")
    parser.add_argument("--rpc", default="http://127.0.0.1:8000", help="node REST URL")
    parser.add_argument("--key", help="key file used to sign transactions")
    parser.add_argument("--timeout", type=float, default=10.0)

    commands = parser.add_subparsers(dest="command", required=True)

    commands.add_parser("health")
    commands.add_parser("validators")
    commands.add_parser("finality")
    commands.add_parser("metrics")

    balance = commands.add_parser("balance")
    balance.add_argument("--address")

    send = commands.add_parser("send")
    send.add_argument("--to", required=True)
    send.add_argument("--value", required=True)
    send.add_argument("--nonce", type=int)

    stake = commands.add_parser("stake")
    stake.add_argument("--amount", required=True)
    commands.add_parser("undelegate")
    commands.add_parser("withdraw")

    deploy = commands.add_parser("deploy")
    deploy.add_argument("--id", required=True)
    deploy.add_argument("--file", required=True)
    deploy.add_argument("--gas", type=int, default=2_000_000)

    call = commands.add_parser("call")
    call.add_argument("--id", required=True)
    call.add_argument("--function", required=True)
    call.add_argument("--param", action="append", default=[])
    call.add_argument("--gas", type=int, default=1_000_000)

    query = commands.add_parser("query")
    query.add_argument("--id", required=True)
    query.add_argument("--function", required=True)
    query.add_argument("--param", action="append", default=[])

    gov = commands.add_parser("gov")
    gov.add_argument("--name", required=True)
    gov.add_argument("--value", required=True, type=int)

    proof = commands.add_parser("proof")
    proof.add_argument("--address")

    receipt = commands.add_parser("receipt")
    receipt.add_argument("--block", required=True, type=int)
    receipt.add_argument("--tx", required=True, type=int)

    tx = commands.add_parser("tx")
    tx.add_argument("--id", required=True)

    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)

    try:
        identity = NodeIdentity.from_file(args.key) if args.key else None
    except Exception as exc:  # noqa: BLE001
        print(f"key error: {exc}", file=sys.stderr)
        return 1

    client = SanClient(args.rpc, identity, timeout=args.timeout)
    sign_commands = {"send", "stake", "undelegate", "withdraw", "deploy", "call", "gov"}
    if args.command in sign_commands and identity is None:
        print("--key is required for transaction commands", file=sys.stderr)
        return 1

    try:
        if args.command == "health":
            output = client.health()
        elif args.command == "validators":
            output = client.validators()
        elif args.command == "finality":
            output = client.finality()
        elif args.command == "metrics":
            print(client.metrics_text(), end="")
            return 0
        elif args.command == "balance":
            output = client.account(args.address or client.address)
        elif args.command == "send":
            output = client.transfer(args.to, float(args.value), nonce=args.nonce)
        elif args.command == "stake":
            output = client.deposit_stake(float(args.amount))
        elif args.command == "undelegate":
            output = client.undelegate()
        elif args.command == "withdraw":
            output = client.withdraw_stake()
        elif args.command == "deploy":
            source = Path(args.file).read_text()
            output = client.deploy_contract(args.id, source, gas_limit=args.gas)
        elif args.command == "call":
            output = client.call_contract(
                args.id,
                args.function,
                [_coerce(param) for param in args.param],
                gas_limit=args.gas,
            )
        elif args.command == "query":
            output = client.contract_query(
                args.id, args.function, [_coerce(param) for param in args.param]
            )
        elif args.command == "gov":
            nonce = client.nonce()
            approval = client.governance_approval(args.name, args.value, nonce=nonce)
            output = client.governance_set_param(
                args.name, args.value, [approval], nonce=nonce
            )
        elif args.command == "proof":
            output = client.account_proof(args.address)
        elif args.command == "receipt":
            output = client.receipt(args.block, args.tx)
        elif args.command == "tx":
            output = client.transaction(args.id)
        else:  # pragma: no cover - argparse guarantees a command
            return 2
    except SanClientError as exc:
        print(json.dumps({"error": str(exc)}), file=sys.stderr)
        return 1

    print(json.dumps(output, indent=2, default=str))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
