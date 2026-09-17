"""Faz E verification: receipts, governance, metrics, SDK and CLI.

Run from the repository root:

    python tests/smoke_p5.py
"""
import asyncio
import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    SAN_BASE,
    check,
    make_config,
    sample_address,
    setup_identity,
    summary,
)

from blockchain.identity import NodeIdentity  # noqa: E402
from network.Node import Node  # noqa: E402
from sdk import SanClient, SanClientError  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parent.parent


async def start_api(node):
    import uvicorn
    from fastapi import FastAPI

    from app.routes import router

    app = FastAPI()
    app.include_router(router)
    app.state.node = node

    server = uvicorn.Server(
        uvicorn.Config(app, host="127.0.0.1", port=node.config.api_port, log_level="warning")
    )
    task = asyncio.create_task(server.serve())
    while not server.started:
        await asyncio.sleep(0.05)
    return server, task


async def finalized_at_least(client, target) -> bool:
    health = await offload(client.health)
    return int(health.get("finalized_height", 0) or 0) >= target


async def contract_state_is(client, expected) -> bool:
    value = await offload(client.contract_query, "sdkc", "get")
    return value == expected


async def offload(fn, *args, **kwargs):
    """Run a blocking SDK call without stalling the node's event loop."""
    return await asyncio.to_thread(fn, *args, **kwargs)


async def wait_until(description, predicate, timeout=15.0, interval=0.2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        outcome = predicate()
        if asyncio.iscoroutine(outcome):
            outcome = await outcome
        if outcome:
            return True
        await asyncio.sleep(interval)
    check(description, False, "timed out")
    return False


async def main(public_key, private_key):
    print("\n=== E: receipts, governance, metrics, SDK, CLI ===")
    identity = NodeIdentity(private_key=private_key, public_key=public_key)
    receiver = sample_address("e5")

    with tempfile.TemporaryDirectory() as tmp:
        key_path = os.path.join(tmp, "key.json")
        identity.save(key_path)

        node = Node(
            make_config(
                min_validator_stake=100 * SAN_BASE,
                unbonding_period=5,
                snapshot_interval=2,
            )
        )
        await node.start()
        server, task = await start_api(node)
        client = SanClient(f"http://127.0.0.1:{node.config.api_port}", identity)

        health = await offload(client.health)
        check("health exposes chain id and base fee", health["chain_id"] and health["base_fee"] >= 1)

        # ---------------- staking + transfer ---------------- #
        result = await offload(client.deposit_stake, 500)
        check("SDK stake deposit committed", result.get("status") == "committed", json.dumps(result))
        validators = await offload(client.validators)
        check(
            "the SDK wallet is an active validator",
            any(v["address"] == client.address for v in validators["validators"]),
            json.dumps(validators)[:200],
        )
        result = await offload(client.transfer, receiver, 3)
        check("SDK transfer committed", result.get("status") == "committed", json.dumps(result))
        account = await offload(client.account, receiver)
        check("receiver balance visible", account["balance"] == "3")
        await wait_until("finality advanced past the transfer", lambda: finalized_at_least(client, 2))

        # ---------------- contracts + receipts ---------------- #
        source = (
            "value = 1\n"
            "function get() { return value }\n"
            'function set(v) { value = v\n print("set called")\n return value }\n'
        )
        deploy = await offload(client.deploy_contract, "sdkc", source)
        check("SDK contract deploy committed", deploy.get("status") == "committed", json.dumps(deploy))
        check("contract query reads state", await offload(client.contract_query, "sdkc", "get") == 1)

        call_result = await offload(client.call_contract, "sdkc", "set", [7])
        check("SDK contract call committed", call_result.get("status") == "committed", json.dumps(call_result))
        await wait_until("contract state updated", lambda: contract_state_is(client, 7))

        receipt = await offload(client.receipt, call_result["block_index"], 0)
        check(
            "receipt has status, gas and logs",
            receipt["status"] == "success"
            and receipt["gas_used"] > 0
            and {"event": "print", "value": "set called"} in receipt["logs"],
            json.dumps(receipt),
        )

        # failed execution still produces a receipt
        failed = await offload(client.call_contract, "sdkc", "set", [999], gas_limit=1)
        check("tiny-gas contract call is committed with a failure", failed.get("status") == "committed")
        failed_receipt = await offload(client.receipt, failed["block_index"], 0)
        check(
            "failed receipt records the error",
            failed_receipt["status"] == "failed" and bool(failed_receipt["failure"]),
            json.dumps(failed_receipt),
        )

        # ---------------- governance ---------------- #
        nonce = await offload(client.nonce)
        approval = await offload(client.governance_approval, "unbonding_period", 7, nonce=nonce)
        change = await offload(client.governance_set_param, "unbonding_period", 7, [approval], nonce=nonce)
        check("validator-approved parameter change committed", change.get("status") == "committed", json.dumps(change))
        on_chain = await offload(client.validators)
        check(
            "parameter is visible on-chain",
            on_chain["parameters"]["unbonding_period"] == 7,
            json.dumps(on_chain["parameters"]),
        )

        rejected = False
        try:
            change_nonce = await offload(client.nonce)
            await offload(client.governance_set_param, "unbonding_period", 9, [], nonce=change_nonce)
        except SanClientError:
            rejected = True
        check("parameter change without approvals is rejected", rejected)

        # ---------------- proofs + metrics ---------------- #
        proof = await offload(client.account_proof, receiver)
        tip_health = await offload(client.health)
        check(
            "SDK verifies the account proof against the tip state root",
            SanClient.verify_account_proof(proof, expected_root=tip_health["state_root"]),
        )
        metrics = await offload(client.metrics_text)
        check(
            "metrics endpoint exposes counters and gauges",
            "san_height" in metrics and "san_blocks_committed" in metrics,
            metrics.splitlines()[0] if metrics else "",
        )

        # ---------------- CLI ---------------- #
        env = {**os.environ, "PYTHONPATH": str(REPO_ROOT)}
        balance_run = await asyncio.to_thread(
            subprocess.run,
            [sys.executable, "-m", "sdk.cli", "--rpc", client.base_url, "balance", "--address", receiver],
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=60,
        )
        check(
            "CLI balance prints JSON",
            balance_run.returncode == 0 and json.loads(balance_run.stdout)["balance"] == "3",
            balance_run.stdout + balance_run.stderr,
        )

        value = f"0.{int(time.time()) % 100 or 1:02d}"
        send_run = await asyncio.to_thread(
            subprocess.run,
            [
                sys.executable, "-m", "sdk.cli",
                "--rpc", client.base_url,
                "--key", key_path,
                "send", "--to", sample_address("c1"), "--value", value,
            ],
            cwd=REPO_ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=60,
        )
        check(
            "CLI send committed a transaction",
            send_run.returncode == 0 and json.loads(send_run.stdout).get("status") == "committed",
            send_run.stdout + send_run.stderr,
        )

        await node.stop()
        server.should_exit = True
        await task


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    asyncio.run(main(PUBLIC_KEY, PRIVATE_KEY))
    sys.exit(summary())
