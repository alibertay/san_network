"""Three real nodes over HTTP/WebSocket: bootstrap, gossip, consensus,
contract deployment, read-only queries and restart catch-up.

Run from the repository root:

    python tests/e2e_network.py
"""
import asyncio
import json
import logging
import os
import sys
import tempfile
import time
from dataclasses import replace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import requests  # noqa: E402
from helpers import (  # noqa: E402
    CHAIN_ID,
    SAN_BASE,
    check,
    free_port,
    sample_address,
    summary,
)

from blockchain.identity import NodeIdentity  # noqa: E402
from blockchain.Transaction import Transaction  # noqa: E402
from network.config import NodeConfig  # noqa: E402
from network.Node import Node  # noqa: E402


def make_config(**overrides) -> NodeConfig:
    base = dict(
        chain_id=CHAIN_ID,
        host="127.0.0.1",
        advertise_host="127.0.0.1",
        api_port=free_port(),
        p2p_port=free_port(),
        peer_port=free_port(),
        controller_port=free_port(),
        block_threshold_fee=0.0001,
        peer_check_interval=1.0,
        ws_timeout=3.0,
    )
    base.update(overrides)
    return NodeConfig(**base)


async def start_api(node: Node):
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


async def stop_api(server, task):
    server.should_exit = True
    await task


async def get_json(url, **params):
    return await asyncio.to_thread(
        lambda: requests.get(url, params=params or None, timeout=5).json()
    )


async def post_json(url, payload):
    response = await asyncio.to_thread(
        lambda: requests.post(url, json=payload, timeout=10)
    )
    return response.status_code, response.json()


async def wait_until(description, predicate, timeout=15.0, interval=0.25):
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


def signed_transaction(identity: NodeIdentity, nonce: int, **fields) -> dict:
    payload = {"chain_id": CHAIN_ID, "sender": identity.public_key_hex, "nonce": nonce}
    payload.update(fields)
    payload["signature"] = Transaction.sign_payload(payload, identity.private_key)
    return payload


async def main():
    private_a = NodeIdentity.generate()
    private_b = NodeIdentity.generate()
    private_c = NodeIdentity.generate()
    allocations = {
        identity.public_key_hex: 1_000_000 * SAN_BASE
        for identity in (private_a, private_b, private_c)
    }

    with tempfile.TemporaryDirectory() as tmp:
        db_c = os.path.join(tmp, "c.db")

        config_a = make_config(genesis_allocations=allocations)
        config_b = make_config(
            genesis_allocations=allocations,
            bootstrap=f"127.0.0.1:{config_a.api_port}",
        )
        config_c = replace(
            make_config(genesis_allocations=allocations),
            db_path=db_c,
            bootstrap=f"127.0.0.1:{config_a.api_port}",
        )
        config_c_restart = replace(
            make_config(genesis_allocations=allocations),
            db_path=db_c,
            bootstrap=f"127.0.0.1:{config_a.api_port}",
        )

        node_a = Node(config_a, identity=private_a)
        node_b = Node(config_b, identity=private_b)
        node_c = Node(config_c, identity=private_c)

        await node_a.start()
        server_a, task_a = await start_api(node_a)
        await node_b.start()
        server_b, task_b = await start_api(node_b)
        await node_c.start()
        server_c, task_c = await start_api(node_c)

        base_a = f"http://127.0.0.1:{config_a.api_port}"
        base_b = f"http://127.0.0.1:{config_b.api_port}"
        base_c = f"http://127.0.0.1:{config_c.api_port}"

        # ---------------- discovery / gossip ---------------- #
        await wait_until(
            "A knows B and C",
            lambda: len(node_a.PEERS) >= 2,
        )
        await wait_until(
            "C learns B through gossip",
            lambda: any(p["api_port"] == config_b.api_port for p in node_c.PEERS),
        )
        check(
            "all nodes discovered each other",
            len(node_a.PEERS) == 2 and len(node_b.PEERS) == 2 and len(node_c.PEERS) == 2,
            f"A={len(node_a.PEERS)} B={len(node_b.PEERS)} C={len(node_c.PEERS)}",
        )

        health = await get_json(f"{base_a}/health")
        check(
            "A exposes health with peers",
            health.get("status") == "ok" and health.get("peers") == 2,
            json.dumps(health),
        )

        # ---------------- transfer + consensus ---------------- #
        receiver = sample_address("ec")
        status_code, result = await post_json(
            f"{base_a}/transaction",
            signed_transaction(private_a, 0, receiver=receiver, value=5),
        )
        check(
            "transfer committed with controller quorum",
            status_code == 200 and result.get("status") == "committed",
            f"{status_code} {result}",
        )

        heights = {}

        async def all_at_height(expected):
            nonlocal heights
            heights = {
                "A": (await get_json(f"{base_a}/health"))["height"],
                "B": (await get_json(f"{base_b}/health"))["height"],
                "C": (await get_json(f"{base_c}/health"))["height"],
            }
            return all(height == expected for height in heights.values())

        await wait_until("block gossiped to B and C", lambda: all_at_height(1))
        check(
            "all nodes at the same height",
            len(set(heights.values())) == 1 and heights.get("A") == 1,
            str(heights),
        )
        tips = {
            "A": (await get_json(f"{base_a}/health"))["tip_hash"],
            "B": (await get_json(f"{base_b}/health"))["tip_hash"],
            "C": (await get_json(f"{base_c}/health"))["tip_hash"],
        }
        check("all nodes share the same tip hash", len(set(tips.values())) == 1, str(tips))

        account = await get_json(f"{base_b}/account/{receiver}")
        check(
            "B sees the transferred balance",
            account.get("balance_units") == 5 * SAN_BASE,
            json.dumps(account),
        )
        block = await get_json(f"{base_c}/block/1")
        check("C serves block 1", block.get("index") == 1 and bool(block.get("current_block_hash")))

        # ---------------- contract deployment ---------------- #
        contract_source = (
            "value = 1\n"
            "function get() {\n  return value\n}\n"
            "function set(v) {\n  value = v\n  return value\n}\n"
        )
        status_code, result = await post_json(
            f"{base_a}/transaction",
            signed_transaction(
                private_a,
                1,
                gas_limit=1_000_000,
                gas_price=1,
                contract_code={
                    "command": "deploy",
                    "contract_id": "kv",
                    "pena_code": contract_source,
                },
            ),
        )
        check(
            "contract deploy committed",
            status_code == 200 and result.get("status") == "committed",
            f"{status_code} {result}",
        )
        await wait_until("deploy block propagated", lambda: all_at_height(2))

        contracts = await get_json(f"{base_b}/contracts")
        check("B lists the deployed contract", contracts.get("contracts") == ["kv"], str(contracts))

        query = await post_json(
            f"{base_b}/contract/query",
            {"contract_id": "kv", "function_name": "get", "params": []},
        )
        check("B reads contract state", query[1].get("result") == 1, str(query))

        # ---------------- state-changing contract call ---------------- #
        status_code, result = await post_json(
            f"{base_a}/transaction",
            signed_transaction(
                private_a,
                2,
                gas_limit=1_000_000,
                gas_price=1,
                contract_code={
                    "command": "run",
                    "contract_id": "kv",
                    "function_name": "set",
                    "params": [42],
                },
            ),
        )
        check(
            "contract call committed",
            status_code == 200 and result.get("status") == "committed",
            f"{status_code} {result}",
        )
        await wait_until("contract call propagated", lambda: all_at_height(3))

        results = {}
        for name, base in (("A", base_a), ("B", base_b), ("C", base_c)):
            _, body = await post_json(
                f"{base}/contract/query",
                {"contract_id": "kv", "function_name": "get", "params": []},
            )
            results[name] = body.get("result")
        check("all nodes execute the contract identically", results == {"A": 42, "B": 42, "C": 42}, str(results))

        # ---------------- restart + catch-up ---------------- #
        await node_c.stop()
        await stop_api(server_c, task_c)
        await wait_until(
            "A evicts the stopped node from its controller set",
            lambda: len(node_a.PEERS) == 1,
        )

        node_d = Node(config_c_restart, identity=private_c)
        check(
            "C restores its chain from disk",
            len(node_d.blockchain.chain) == 4,
            f"height={node_d.blockchain.tip.index}",
        )
        restored = await post_json(
            f"{base_a}/transaction",
            signed_transaction(private_a, 3, receiver=sample_address("af"), value=1),
        )
        check("A produces a block while C is down", restored[1].get("status") == "committed")

        await node_d.start()
        server_d, task_d = await start_api(node_d)
        base_d = f"http://127.0.0.1:{config_c_restart.api_port}"

        async def caught_up():
            return (await get_json(f"{base_d}/health"))["height"] == 4

        await wait_until("restarted C catches up via sync", caught_up)
        check(
            "restarted node synced to the network tip",
            (await get_json(f"{base_d}/health"))["tip_hash"]
            == (await get_json(f"{base_a}/health"))["tip_hash"],
        )

        await node_d.stop()
        await stop_api(server_d, task_d)
        await node_b.stop()
        await stop_api(server_b, task_b)
        await node_a.stop()
        await stop_api(server_a, task_a)


if __name__ == "__main__":
    logging.basicConfig(
        level=os.getenv("E2E_LOG_LEVEL", "WARNING").upper(),
        format="%(levelname)s %(name)s: %(message)s",
    )
    logging.getLogger("websockets").setLevel(logging.WARNING)
    logging.getLogger("httpx").setLevel(logging.WARNING)
    asyncio.run(main())
    sys.exit(summary())
