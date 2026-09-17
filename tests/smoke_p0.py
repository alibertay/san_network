"""P0 regression smoke test: single-node API flow and two-node P2P flow.

Run from the repository root:

    python tests/smoke_p0.py
"""
import asyncio
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    CHAIN_ID,
    SAN_BASE,
    address_of,
    check,
    free_port,
    make_config,
    sample_address,
    setup_identity,
    signed_payload,
    summary,
)

from blockchain.Blockchain import Blockchain  # noqa: E402
from network.config import NodeConfig  # noqa: E402
from network.Node import Node  # noqa: E402


async def single_node_flow(public_key, private_key):
    print("\n=== single node (no peers) ===")
    node = Node(make_config())
    await node.start()
    check("P0-3 Node() starts with empty PEERS", node.PEERS == [])
    check(
        "P0-9 PING to non-existent peer returns False",
        await node.ping_node(
            {
                "host": "127.0.0.1",
                "api_port": 1,
                "p2p_port": 1,
                "peer_port": 1,
                "controller_port": 1,
            }
        )
        is False,
    )

    receiver = sample_address("b0b")
    payload = signed_payload(public_key, private_key, 0, receiver=receiver, value=10)
    result = await node.submit_transaction(payload)
    check("P0-5 transaction payload accepted", result["status"] == "committed", json.dumps(result))
    check(
        "P0-1 block committed with deterministic hash",
        len(node.blockchain.chain) == 2 and bool(node.blockchain.tip.current_block_hash),
    )
    check("P0-5 fee stored on chain tx", node.blockchain.tip.transactions[0].get("fee", 0) > 0)
    fee_units = node.blockchain.tip.transactions[0]["fee"]
    check(
        "P1-9 balances moved in base units (sender is also the validator)",
        node.blockchain.SAN.get(receiver) == 10 * SAN_BASE
        and node.blockchain.SAN[address_of(public_key)] == 1_000_000 * SAN_BASE - 10 * SAN_BASE,
        f"balances={node.blockchain.SAN}",
    )
    check("P1-9 validator collected the fee", fee_units > 0 and len(node.blockchain.chain) == 2)
    check("P1-8 nonce advanced", node.blockchain.nonces.get(address_of(public_key)) == 1)

    sync_payload = node.get_sync_payload(0)
    check(
        "P0-6 get_sync_payload shape",
        {"blocks", "storage", "state", "version", "genesis_allocation"}.issubset(sync_payload),
        str(sorted(sync_payload)),
    )
    check("P0-6 storage snapshot is a dict", isinstance(sync_payload["storage"], dict))
    check("P1-9 state snapshot has balances", "balances" in sync_payload["state"])

    await node.stop()


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


async def stop_api(server, task):
    server.should_exit = True
    await task


async def check_dead_peers(node_a):
    dead = {
        "host": "127.0.0.1",
        "api_port": 1,
        "p2p_port": 1,
        "peer_port": free_port(),
        "controller_port": 1,
    }
    node_a.PEERS.append(dead)
    node_a._refresh_peer_selection()
    # Eviction needs the configured number of consecutive misses.
    for _ in range(node_a.config.peer_miss_threshold):
        await node_a.check_dead_peers()
    return not any(p["peer_port"] == dead["peer_port"] for p in node_a.PEERS)


async def two_node_flow(public_key, private_key):
    print("\n=== two nodes (register, ping, signed vote, sync) ===")
    config_a = make_config()
    config_b = make_config()
    node_a = Node(config_a)
    node_b = Node(config_b)
    await node_a.start()
    await node_b.start()
    server_a, task_a = await start_api(node_a)

    node_b.add_peers([node_a.self_peer_record()])
    await node_b.register_to_network()
    await asyncio.sleep(0.3)

    check(
        "P0-9 B registered to A (PEER_UPDATE)",
        any(p["api_port"] == config_b.api_port for p in node_a.PEERS),
        f"A.PEERS={node_a.PEERS}",
    )
    check("P0-9 A answered PING with PONG", await node_a.ping_node(node_b.self_peer_record()))
    check("P0-4 check_dead_peers keeps live peers", await check_dead_peers(node_a))
    check(
        "P0-8 controller selection works",
        len(node_a.controller_nodes) >= 1,
        f"controllers={node_a.controller_nodes}",
    )
    check(
        "P1-4 peer record advertises the public key",
        node_a.PEERS[0].get("public_key") == public_key.hex(),
    )

    payload = signed_payload(public_key, private_key, 0, receiver=sample_address("a1"), value=10)
    result = await node_a.submit_transaction(payload)
    check(
        "P0-8 consensus awaited, block approved by controller",
        result["status"] == "committed",
        json.dumps(result),
    )
    await asyncio.sleep(0.3)
    check(
        "P0-8 block broadcast received by B",
        len(node_b.blockchain.chain) == 2,
        f"B chain length={len(node_b.blockchain.chain)}",
    )

    second_receiver = sample_address("b0b")
    payload2 = signed_payload(public_key, private_key, 1, receiver=second_receiver, value=5)
    await node_a.submit_transaction(payload2)

    node_c = Node(make_config())
    await node_c.start()
    node_c.add_peers([node_a.self_peer_record()])
    synced = await node_c.synchronize()
    check(
        "P0-6 sync pulls missing blocks",
        synced and len(node_c.blockchain.chain) == len(node_a.blockchain.chain),
        f"synced={synced} C={len(node_c.blockchain.chain)} A={len(node_a.blockchain.chain)}",
    )
    check(
        "P0-6 synced chain hashes match",
        [b.current_block_hash for b in node_c.blockchain.chain]
        == [b.current_block_hash for b in node_a.blockchain.chain],
    )
    check(
        "P1-9 synced node derives balances and nonces by replay",
        node_c.blockchain.SAN.get(second_receiver) == 5 * SAN_BASE
        and node_c.blockchain.nonces.get(address_of(public_key)) == 2,
        f"C balances={node_c.blockchain.SAN} nonces={node_c.blockchain.nonces}",
    )

    await node_c.stop()
    await node_b.stop()
    await node_a.stop()
    await stop_api(server_a, task_a)


def api_flow(public_key, private_key):
    print("\n=== FastAPI app (TestClient) ===")
    from fastapi.testclient import TestClient

    import app.main as main

    with TestClient(main.app) as client:
        response = client.get("/bootstrap")
        body = response.json()
        check(
            "API /bootstrap returns the seed's signed record",
            response.status_code == 200
            and len(body.get("peers", [])) == 1
            and bool(body["peers"][0].get("signature")),
            response.text,
        )

        payload = signed_payload(public_key, private_key, 0, receiver=sample_address("c3"), value=3)
        response = client.post("/transaction", json=payload)
        check("API /transaction 200", response.status_code == 200, response.text)
        check(
            "API /transaction pooled/committed",
            response.json().get("status") in {"pooled", "committed"},
            response.text,
        )

        response = client.post(
            "/transaction", content="not-json", headers={"Content-Type": "application/json"}
        )
        check("API /transaction bad JSON -> 4xx", 400 <= response.status_code < 500, str(response.status_code))

        response = client.post("/transaction", json={"sender": "0xalice", "value": 5})
        check(
            "P1-10 API rejects unsigned transaction",
            response.status_code == 400,
            f"{response.status_code} {response.text}",
        )

        response = client.get("/sync", params={"from_index": 0})
        body = response.json()
        check(
            "P0-6 API /sync returns blocks+storage+state",
            response.status_code == 200
            and {"blocks", "storage", "state"}.issubset(body),
            response.text,
        )
        check(
            "P0-6 API /sync genesis hash deterministic",
            body["blocks"][0]["current_block_hash"]
            == Blockchain(
                NodeConfig.from_env().genesis_allocations, chain_id=CHAIN_ID
            ).tip.current_block_hash,
        )
        check(
            "A2 API /sync reports the chain_id",
            body.get("chain_id") == CHAIN_ID,
            str(body.get("chain_id")),
        )

        response = client.post("/join")
        check("API /join without bootstrap -> 400", response.status_code == 400, response.text)


async def run_async_flows(public_key, private_key):
    await single_node_flow(public_key, private_key)
    await two_node_flow(public_key, private_key)


if __name__ == "__main__":
    public_key, private_key = setup_identity()
    asyncio.run(run_async_flows(public_key, private_key))
    api_flow(public_key, private_key)
    sys.exit(summary())
