"""P2+ extras: mempool pull, orphan parent fetch and paged sync.

Run from the repository root:

    python tests/smoke_p6.py
"""
import asyncio
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from helpers import (  # noqa: E402
    check,
    make_config,
    sample_address,
    setup_identity,
    signed_payload,
    summary,
)

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
    # ------------------------------------------------------------------ #
    # 1) mempool pull: a node joining later refills its pool via GET_TXS
    # ------------------------------------------------------------------ #
    print("\n=== mempool pull (GET_TXS) ===")
    config_pool = make_config(block_threshold_fee=1_000_000)
    node_pool = Node(config_pool)
    await node_pool.start()
    server, task = await start_api(node_pool)

    payload = signed_payload(public_key, private_key, 0, receiver=sample_address("61"), value=1)
    result = await node_pool.submit_transaction(payload)
    check("transaction stays pooled on the host", result["status"] == "pooled", json.dumps(result))

    joiner_config = make_config(
        block_threshold_fee=1_000_000,
        bootstrap=f"127.0.0.1:{config_pool.api_port}",
    )
    joiner = Node(joiner_config)
    await joiner.start()
    await wait_until(
        "joining node pulled the pooled transaction",
        lambda: any(tx.tx_id == result["tx_id"] for tx in joiner.transaction_pool),
    )
    check(
        "mempool refilled from the peer",
        any(tx.tx_id == result["tx_id"] for tx in joiner.transaction_pool),
        f"joiner pool={[tx.tx_id[:10] for tx in joiner.transaction_pool]}",
    )
    await joiner.stop()
    await node_pool.stop()
    server.should_exit = True
    await task

    # ------------------------------------------------------------------ #
    # 2) paged sync and orphan parent fetch
    # ------------------------------------------------------------------ #
    print("\n=== paged sync + orphan parent fetch (GET_BLOCK) ===")
    config_a = make_config()
    node_a = Node(config_a)
    await node_a.start()
    server_a, task_a = await start_api(node_a)

    for nonce in range(3):
        await node_a.submit_transaction(
            signed_payload(public_key, private_key, nonce, receiver=sample_address("62"), value=1)
        )
    await wait_until("host produced 3 blocks", lambda: node_a.blockchain.tip.index == 3)
    blocks = {block.index: block for block in node_a.blockchain.chain}
    host_tip = node_a.blockchain.tip.current_block_hash
    host_root = node_a.current_state_root()

    # Paged sync: two blocks per request, three blocks to fetch.
    syncer = Node(make_config(sync_batch_size=2))
    await syncer.start()
    syncer.add_peers([node_a.self_peer_record()])
    synced = await syncer.synchronize()
    check(
        "paged sync caught up in more than one request",
        synced and syncer.blockchain.tip.index == 3,
        f"height={syncer.blockchain.tip.index}",
    )
    check("paged sync reached the same tip", syncer.blockchain.tip.current_block_hash == host_tip)
    check("paged sync rebuilt the same state", syncer.current_state_root() == host_root)
    await syncer.stop()

    # Orphan fetch: a fresh node receives only the newest block, then asks the
    # peer for the missing parents until the branch connects.
    orphan = Node(make_config())
    await orphan.start()
    orphan.add_peers([node_a.self_peer_record()])
    orphan._refresh_peer_selection()

    orphaned = orphan._process_incoming_block(blocks[3])
    check("newest block buffered as an orphan", orphaned and orphan.blockchain.tip.index == 0)

    for height in (2, 1):
        fetched = await orphan._request_block(blocks[height].current_block_hash)
        check(
            f"requested parent block {height}",
            fetched or orphan.block_at(height) is not None,
            f"tip={orphan.blockchain.tip.index}",
        )

    await wait_until(
        "orphan branch connected after parents arrived",
        lambda: orphan.blockchain.tip.index == 3,
    )
    check(
        "orphan node assembled the full branch",
        orphan.blockchain.tip.current_block_hash == host_tip
        and orphan.current_state_root() == host_root,
        f"tip={orphan.blockchain.tip.index}",
    )

    await orphan.stop()
    await node_a.stop()
    server_a.should_exit = True
    await task_a


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    asyncio.run(main(PUBLIC_KEY, PRIVATE_KEY))
    sys.exit(summary())
