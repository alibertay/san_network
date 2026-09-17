"""Faz D verification: state/tx commitments, light proofs, snapshots, pruning.

Run from the repository root:

    python tests/smoke_p4.py
"""
import asyncio
import json
import os
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import requests  # noqa: E402
from helpers import (  # noqa: E402
    SAN_BASE,
    check,
    make_config,
    sample_address,
    setup_identity,
    signed_payload,
    summary,
)

from blockchain.merkle import state_root as compute_state_root  # noqa: E402
from blockchain.merkle import verify_merkle_proof  # noqa: E402
from network.Node import Node  # noqa: E402


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


async def get_json(url, **params):
    return await asyncio.to_thread(
        lambda: requests.get(url, params=params or None, timeout=5).json()
    )


async def post_json(url, payload):
    response = await asyncio.to_thread(lambda: requests.post(url, json=payload, timeout=10))
    return response.status_code, response.json()


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
    print("\n=== D1/D2 merkle commitments and light proofs ===")
    with tempfile.TemporaryDirectory() as tmp:
        config = make_config(
            db_path=os.path.join(tmp, "node.db"),
            snapshot_interval=2,
            prune_keep=2,
            min_validator_stake=100 * SAN_BASE,
        )
        node = Node(config)
        await node.start()
        server, task = await start_api(node)
        base = f"http://127.0.0.1:{config.api_port}"

        # Stake so the single validator finalizes its own blocks.
        status, body = await post_json(
            f"{base}/transaction",
            signed_payload(
                public_key,
                private_key,
                0,
                extra={"validator": {"command": "deposit", "amount": 500 * SAN_BASE}},
            ),
        )
        check("stake committed", status == 200 and body.get("status") == "committed", json.dumps(body))

        receiver = sample_address("d4")
        for nonce in range(1, 5):
            await post_json(
                f"{base}/transaction",
                signed_payload(public_key, private_key, nonce, receiver=receiver, value=1),
            )

        await wait_until(
            "chain advanced to height 5",
            lambda: _at_least(f"{base}/health", "height", 5),
        )
        await wait_until(
            "finality checkpoint passed height 4",
            lambda: _at_least(f"{base}/finality", "finalized_height", 4),
        )
        health = await get_json(f"{base}/health")
        check("health exposes the tip state root", bool(health.get("state_root")), str(health.get("state_root"))[:20])

        # --- account proof ---
        proof = await get_json(f"{base}/proof/account/{receiver}")
        check(
            "account proof verifies against the tip state root",
            proof.get("root") == health["state_root"]
            and verify_merkle_proof(proof["root"], proof["leaf"], proof["proof"], proof["index"]),
            json.dumps({k: proof.get(k) for k in ("index", "root")}),
        )
        check(
            "account proof binds the real balance",
            proof["leaf"]["balance"] == 4 * SAN_BASE,
            str(proof["leaf"]),
        )
        tampered = {**proof["leaf"], "balance": 10 ** 18}
        check(
            "tampered account proof fails",
            verify_merkle_proof(proof["root"], tampered, proof["proof"], proof["index"]) is False,
        )

        # --- transaction proof ---
        tx_proof = await get_json(f"{base}/proof/tx/1/0")
        check(
            "transaction proof verifies against the block tx_root",
            verify_merkle_proof(
                tx_proof["tx_root"], tx_proof["transaction"], tx_proof["proof"], tx_proof["tx_index"]
            ),
            json.dumps({"tx_index": tx_proof.get("tx_index")}),
        )

        # --- headers for light clients ---
        headers = (await get_json(f"{base}/headers", from_index=0, limit=10))["headers"]
        check(
            "headers carry roots and omit transactions",
            bool(headers)
            and "transactions" not in headers[0]
            and bool(headers[0]["tx_root"])
            and bool(headers[0]["state_root"]),
        )
        check(
            "header hashes match the local chain",
            all(
                header["current_block_hash"]
                == node.blockchain.chain[header["index"]].current_block_hash
                for header in headers
            ),
        )

        # --- snapshot ---
        snapshot = await get_json(f"{base}/snapshot")
        check(
            "snapshot is finalized and bound to its header",
            snapshot["height"] <= node.finalized_height
            and snapshot["state_root"] == node.blockchain.chain[snapshot["height"]].state_root,
            f"height={snapshot.get('height')} finalized={node.finalized_height}",
        )
        state = snapshot["state"]
        recomputed_root = compute_state_root(
            state["balances"],
            state["nonces"],
            state["validators"],
            state["total_slashed"],
            snapshot["storage"],
            state.get("parameters"),
            state.get("base_fee", 1),
            state.get("total_burned", 0),
        )
        check(
            "snapshot state verifies against its state root",
            recomputed_root == snapshot["state_root"],
            f"recomputed={recomputed_root[:16]} announced={snapshot['state_root'][:16]}",
        )

        # --- pruning ---
        stored = node.store.count_blocks()
        check(
            "old block bodies pruned below the keep window",
            stored < node.blockchain.tip.index + 1,
            f"stored={stored} chain={node.blockchain.tip.index + 1}",
        )
        check("genesis is always kept", node.store.load_chain()[0].index == 0)
        check(
            "the node keeps operating after pruning",
            len(node.blockchain.chain) == node.blockchain.tip.index + 1,
        )

        tip_index = node.blockchain.tip.index
        tip_root = node.current_state_root()
        balances_before = dict(node.blockchain.SAN)

        await node.stop()
        server.should_exit = True
        await task

        # --- pruned restart: the node anchors on its snapshot and replays ---
        restored = Node(config)
        check(
            "pruned node restarts from its snapshot window",
            restored.blockchain.chain[0].index > 0
            and restored.blockchain.tip.index == tip_index,
            f"window starts at {restored.blockchain.chain[0].index}, tip {restored.blockchain.tip.index}",
        )
        check("pruned restart rebuilds the same state", restored.current_state_root() == tip_root)
        check("pruned restart restores balances", restored.blockchain.SAN == balances_before)
        check(
            "pruned restart keeps the finality checkpoint",
            restored.finalized_height == tip_index,
            f"finalized={restored.finalized_height}",
        )
        restored.store.close()


async def _at_least(url, field, value):
    try:
        payload = await get_json(url)
    except Exception:  # noqa: BLE001
        return False
    return payload.get(field, 0) >= value


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    asyncio.run(main(PUBLIC_KEY, PRIVATE_KEY))
    sys.exit(summary())
