"""Randomised consistency stress test across three real nodes.

Deterministic seed, mixed workload (staking, transfers, contract deploys and
calls). At the end every node must agree on height, finality checkpoint and
state root, and the money supply must be conserved.

Run from the repository root:

    python tests/stress_consistency.py
"""
import asyncio
import hashlib
import json
import random
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    CHAIN_ID,
    SAN_BASE,
    check,
    make_config,
    sample_address,
    setup_identity,
    summary,
)

from blockchain.address import address_from_public_key  # noqa: E402
from blockchain.identity import NodeIdentity  # noqa: E402
from network.Node import Node  # noqa: E402
from sdk import SanClient  # noqa: E402

SEED = 7
OPS = 10


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


async def offload(fn, *args, **kwargs):
    return await asyncio.to_thread(fn, *args, **kwargs)


async def wait_until(description, predicate, timeout=20.0, interval=0.2):
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


def expected_proposer(chain_id: str, height: int, active: dict[str, int]) -> str:
    ranked = sorted(active)
    seed = hashlib.sha256(f"{chain_id}:{height}".encode("utf-8")).digest()
    return ranked[int.from_bytes(seed[:8], "big") % len(ranked)]


async def main(public_key, private_key):
    print("\n=== stress: randomized workload across 3 nodes ===")
    identity_a = NodeIdentity(private_key=private_key, public_key=public_key)
    identity_b = NodeIdentity.generate()
    identity_c = NodeIdentity.generate()

    allocations = {
        identity.public_key_hex: 1_000_000 * SAN_BASE
        for identity in (identity_a, identity_b, identity_c)
    }
    config_a = make_config(
        min_validator_stake=100 * SAN_BASE,
        unbonding_period=5,
        genesis_allocations=allocations,
    )
    config_b = make_config(
        min_validator_stake=100 * SAN_BASE,
        unbonding_period=5,
        genesis_allocations=allocations,
        bootstrap=f"127.0.0.1:{config_a.api_port}",
    )
    config_c = make_config(
        min_validator_stake=100 * SAN_BASE,
        unbonding_period=5,
        genesis_allocations=allocations,
        bootstrap=f"127.0.0.1:{config_a.api_port}",
    )

    nodes = {
        "A": (Node(config_a, identity=identity_a), f"http://127.0.0.1:{config_a.api_port}"),
        "B": (Node(config_b, identity=identity_b), f"http://127.0.0.1:{config_b.api_port}"),
        "C": (Node(config_c, identity=identity_c), f"http://127.0.0.1:{config_c.api_port}"),
    }
    servers = {}
    for name, (node, _) in nodes.items():
        await node.start()
        servers[name] = await start_api(node)
    clients = {
        name: SanClient(url, node.identity)
        for name, (node, url) in nodes.items()
    }
    for name, (node, _) in nodes.items():
        if name != "A":
            node.add_peers([nodes["A"][0].self_peer_record()])
            await node.register_to_network()
    await wait_until(
        "all nodes know each other",
        lambda: all(len(node.PEERS) == 2 for node, _ in nodes.values()),
    )

    async def converged():
        states = []
        for node, _ in nodes.values():
            states.append((node.blockchain.tip.index, node.finalized_height, node.current_state_root()))
        return len(set(states)) == 1

    async def submit(fields) -> dict:
        """Route the transaction to the proposer of the next height."""
        probe = nodes["A"][0]
        active = probe.blockchain.active_validators()
        next_height = max(node.blockchain.tip.index for node, _ in nodes.values()) + 1
        if not active:
            target = "A"
        else:
            expected = expected_proposer(CHAIN_ID, next_height, active)
            target = next(name for name, (node, _) in nodes.items()
                          if address_from_public_key(node.identity.public_key_hex) == expected)
        return await offload(clients[target].send, **fields)

    # --- initialize validators (A, then B and C) ---
    result = await submit({"validator": {"command": "deposit", "amount": 500 * SAN_BASE}})
    check("A staked", result.get("status") == "committed", json.dumps(result))
    for name, amount in (("B", 200), ("C", 200)):
        identity = nodes[name][0].identity
        probe = nodes["A"][0]
        active = probe.blockchain.active_validators()
        next_height = max(node.blockchain.tip.index for node, _ in nodes.values()) + 1
        expected = expected_proposer(CHAIN_ID, next_height, active)
        target = next(
            n for n, (node, _) in nodes.items()
            if address_from_public_key(node.identity.public_key_hex) == expected
        )
        client = SanClient(nodes[target][1], identity)
        payload = {
            "chain_id": CHAIN_ID,
            "sender": identity.public_key_hex,
            "nonce": await offload(client.nonce),
            "validator": {"command": "deposit", "amount": amount * SAN_BASE},
        }
        from blockchain.Transaction import Transaction

        payload["signature"] = Transaction.sign_payload(payload, identity.private_key)
        result = await offload(client._post, "/transaction", payload)
        check(f"{name} staked", result.get("status") == "committed", json.dumps(result))
        await wait_until(f"{name} stake propagated", converged)

    await wait_until(
        "three validators active on every node",
        lambda: all(len(node.blockchain.active_validators()) == 3 for node, _ in nodes.values()),
    )

    # --- contract ---
    source = (
        "value = 0\n"
        "function get() { return value }\n"
        "function set(v) { value = v\n return value }\n"
    )
    result = await submit({"gas_limit": 2_000_000, "gas_price": 1,
                           "contract_code": {"command": "deploy", "contract_id": "stress", "pena_code": source}})
    check("contract deployed", result.get("status") == "committed", json.dumps(result))
    await wait_until("deploy propagated", converged)

    # --- randomized workload ---
    rng = random.Random(SEED)
    last_value = 0
    for index in range(OPS):
        if rng.random() < 0.5:
            target_address = sample_address(f"{rng.randrange(16):x}{rng.randrange(16):x}")
            value = rng.randrange(1, 4)
            result = await submit({"receiver": target_address, "value": value})
        else:
            last_value = rng.randrange(1, 1000)
            result = await submit(
                {
                    "gas_limit": 1_000_000,
                    "gas_price": 1,
                    "contract_code": {
                        "command": "run",
                        "contract_id": "stress",
                        "function_name": "set",
                        "params": [last_value],
                    },
                }
            )
        check(f"op {index + 1}/{OPS} committed", result.get("status") == "committed", json.dumps(result))
        await wait_until(f"op {index + 1} propagated", converged)

    # --- invariants ---
    await wait_until("finality kept up", lambda: all(
        node.finalized_height == node.blockchain.tip.index for node, _ in nodes.values()
    ))
    await wait_until("all nodes converged", converged)

    states = {
        name: (node.blockchain.tip.index, node.finalized_height, node.current_state_root())
        for name, (node, _) in nodes.items()
    }
    check("all nodes share height, finality and state root", len(set(states.values())) == 1, json.dumps(states))

    node = nodes["A"][0]
    supply = 3 * 1_000_000 * SAN_BASE
    locked = sum(node.blockchain.active_validators().values())
    circulating = sum(node.blockchain.SAN.values())
    check(
        "money supply is conserved",
        circulating + locked + node.blockchain.total_burned + node.blockchain.total_slashed == supply,
        f"circulating={circulating} locked={locked} burned={node.blockchain.total_burned} slashed={node.blockchain.total_slashed}",
    )

    values = {}
    for name, (_, url) in nodes.items():
        values[name] = await offload(SanClient(url).contract_query, "stress", "get")
    check("contract state identical everywhere", set(values.values()) == {last_value}, str(values))

    metrics = await offload(clients["A"].metrics_text)
    check(
        "metrics reflect the workload",
        "san_blocks_committed" in metrics and "san_votes_received" in metrics,
    )

    for name, (node, _) in nodes.items():
        await node.stop()
        servers[name][0].should_exit = True
        await servers[name][1]


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    asyncio.run(main(PUBLIC_KEY, PRIVATE_KEY))
    sys.exit(summary())
