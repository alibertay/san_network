"""P2 verification: identity, peer security, controllers, TLS, persistence.

Run from the repository root:

    python tests/smoke_p2.py
"""
import asyncio
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
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

from blockchain import crypto  # noqa: E402
from blockchain.Block import SCHEMA_VERSION, Block  # noqa: E402
from blockchain.identity import IdentityError, NodeIdentity  # noqa: E402
from network.Node import Node  # noqa: E402

# ---------------------------------------------------------------------- #
# P2-9 identity
# ---------------------------------------------------------------------- #

def identity_checks():
    print("\n=== P2-9 identity and key management ===")

    identity = NodeIdentity.generate()
    check("generated identity can sign", identity.can_sign and bool(identity.public_key_hex))

    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "key.json")
        identity.save(path)
        mode = os.stat(path).st_mode & 0o777
        loaded = NodeIdentity.from_file(path)
        check("key file roundtrip", loaded.public_key == identity.public_key)
        check("key file is 0600", mode == 0o600, oct(mode))

        broken = os.path.join(tmp, "broken.json")
        Path(broken).write_text('{"private_key": "zz"}')
        check("invalid hex key file rejected", _raises(IdentityError, NodeIdentity.from_file, broken))

        empty = os.path.join(tmp, "empty.json")
        Path(empty).write_text("{}")
        check("empty key file rejected", _raises(IdentityError, NodeIdentity.from_file, empty))

    public_key = identity.public_key
    check(
        "wrong-size key detected",
        _raises(IdentityError, NodeIdentity, b"short", public_key),
    )

    identity_without_private = NodeIdentity(private_key=None, public_key=public_key)
    check(
        "public-only identity cannot sign",
        identity_without_private.sign(b"x") is None and not identity_without_private.can_sign,
    )


def _raises(exc_type, func, *args, **kwargs):
    try:
        func(*args, **kwargs)
    except exc_type:
        return True
    except Exception:  # noqa: BLE001
        return False
    return False


# ---------------------------------------------------------------------- #
# P2-4 peer records
# ---------------------------------------------------------------------- #

async def peer_record_checks(public_key, private_key):
    print("\n=== P2-4 signed peer records, TTL, limits ===")
    node = Node(make_config())
    await node.start()

    record = node.self_peer_record()
    check("self record is signed", bool(record.get("signature")) and bool(record.get("public_key")))
    check("own record verifies", node._verify_peer_record(record) is True)

    tampered = dict(record)
    tampered["host"] = "evil.example.com"
    check("tampered peer record rejected", node._verify_peer_record(tampered) is False)

    stale = dict(record)
    stale["timestamp"] = time.time() - 10_000
    signature = node.identity.sign_hex(Node._peer_record_payload(stale))
    stale["signature"] = signature
    check("stale peer record rejected", node._verify_peer_record(stale) is False)

    unsigned = {key: value for key, value in record.items() if key != "signature"}
    check("unsigned peer record rejected", node._verify_peer_record(unsigned) is False)

    lenient = Node(make_config(require_block_signature=False))
    check(
        "unsigned records accepted only in explicit dev mode",
        lenient._verify_peer_record(unsigned) is True,
    )

    first_seen = node._seen_recently(record)
    second_seen = node._seen_recently(record)
    check("peer update dedupe", first_seen is False and second_seen is True)

    limited = Node(make_config(max_peers=1))
    identity_a = NodeIdentity.generate()
    identity_b = NodeIdentity.generate()
    record_a = _signed_peer(identity_a, api_port=free_port())
    record_b = _signed_peer(identity_b, api_port=free_port())
    check(
        "max_peers enforced",
        limited._add_peer(record_a) == 1 and limited._add_peer(record_b) == 0,
        f"peers={len(limited.PEERS)}",
    )

    check(
        "self record rejected as peer",
        node._add_peer(node.self_peer_record()) == 0,
    )

    await node.stop()


def _signed_peer(identity: NodeIdentity, api_port: int) -> dict:
    record = {
        "chain_id": CHAIN_ID,
        "host": "127.0.0.1",
        "api_port": api_port,
        "p2p_port": free_port(),
        "peer_port": free_port(),
        "controller_port": free_port(),
        "timestamp": time.time(),
        "tls": False,
        "public_key": identity.public_key_hex,
    }
    record["signature"] = identity.sign_hex(Node._peer_record_payload(record))
    return record


# ---------------------------------------------------------------------- #
# P2-5 controller selection
# ---------------------------------------------------------------------- #

def controller_checks():
    print("\n=== P2-5 deterministic controller selection ===")
    node = Node(make_config(controller_count=2, epoch_length=1))
    identities = [NodeIdentity.generate() for _ in range(3)]
    peers = [_signed_peer(identity, api_port=free_port()) for identity in identities]
    node.PEERS = list(peers)

    first = node._select_controllers()
    shuffled = [peers[2], peers[0], peers[1]]
    node.PEERS = shuffled
    second = node._select_controllers()
    check(
        "selection is independent of peer order",
        [p["public_key"] for p in first] == [p["public_key"] for p in second],
    )
    check("controller_count respected", len(first) == 2)

    node.blockchain.add_block("GENESIS_VALIDATOR", "sig", [])
    third = node._select_controllers()
    check(
        "epoch change re-ranks deterministically",
        len(third) == 2 and len({p["public_key"] for p in third}) == 2,
    )

    keyed = Node(make_config(controller_min_stake=10.0))
    keyed.PEERS = list(peers)
    check("stake requirement excludes unfunded peers", keyed._select_controllers() == [])
    keyed.blockchain.SAN[address_of(identities[0].public_key)] = 10 * SAN_BASE
    selected = keyed._select_controllers()
    check(
        "funded peer becomes eligible",
        [p["public_key"] for p in selected] == [identities[0].public_key_hex],
    )

    unkeyed = Node(make_config())
    unkeyed.PEERS = [{"host": "127.0.0.1", "api_port": free_port(), "p2p_port": 1,
                      "peer_port": 1, "controller_port": 1}]
    check("peers without a public key cannot be controllers", unkeyed._select_controllers() == [])


# ---------------------------------------------------------------------- #
# P2-11 schema versioning
# ---------------------------------------------------------------------- #

def schema_checks():
    print("\n=== P2-11 schema versioning ===")
    block = Block(0, "0", "v", "s", [], timestamp=0.0)
    payload = block.to_dict()
    check("blocks carry a schema version", payload.get("version") == SCHEMA_VERSION)

    future = dict(payload)
    future["version"] = SCHEMA_VERSION + 1
    check("future block version rejected", _raises(ValueError, Block.from_dict, future))

    node = Node(make_config())
    sync_payload = node.get_sync_payload(0)
    check("sync payload carries a schema version", sync_payload.get("version") == SCHEMA_VERSION)


# ---------------------------------------------------------------------- #
# P2-9/P1 multi-key end to end
# ---------------------------------------------------------------------- #

async def multi_key_flow():
    print("\n=== P2-9 multi-key nodes (distinct identities) ===")
    public_a, private_a = crypto.generate_keypair()
    public_b, private_b = crypto.generate_keypair()
    third_public, third_private = crypto.generate_keypair()

    allocations = {
        public_a.hex(): 1_000_000 * SAN_BASE,
        public_b.hex(): 1_000_000 * SAN_BASE,
    }
    identity_a = NodeIdentity(private_key=private_a, public_key=public_a)
    identity_b = NodeIdentity(private_key=private_b, public_key=public_b)

    node_a = Node(make_config(genesis_allocations=allocations), identity=identity_a)
    node_b = Node(make_config(genesis_allocations=allocations), identity=identity_b)
    await node_a.start()
    await node_b.start()

    node_b.add_peers([node_a.self_peer_record()])
    await node_b.register_to_network()
    await asyncio.sleep(0.3)

    check(
        "peer records are verified against each node's own key",
        any(p.get("public_key") == public_b.hex() for p in node_a.PEERS),
        f"A.PEERS count={len(node_a.PEERS)}",
    )
    check(
        "controller set uses the advertised key",
        [p.get("public_key") for p in node_a.controller_nodes] == [public_b.hex()],
    )

    payload = signed_payload(public_a, private_a, 0, receiver=sample_address("ec"), value=5)
    result = await node_a.submit_transaction(payload)
    check("A's signed block is approved by B", result["status"] == "committed", json.dumps(result))
    await asyncio.sleep(0.3)
    check(
        "B verified A's validator signature with A's key",
        len(node_b.blockchain.chain) == 2
        and node_b.blockchain.tip.validator == public_a.hex(),
    )

    tip = node_b.blockchain.tip
    forged = Block(
        index=tip.index + 1,
        previous_block_hash=tip.current_block_hash,
        validator=public_a.hex(),
        validator_signature=None,
        transactions=[],
        chain_id=CHAIN_ID,
    )
    forged.validator_signature = crypto.sign(
        forged.current_block_hash.encode(), third_private
    ).hex()
    check(
        "block forged with a different key is rejected",
        node_b.verify_block(forged) is False,
    )

    await node_b.stop()
    await node_a.stop()


# ---------------------------------------------------------------------- #
# P2-10 TLS
# ---------------------------------------------------------------------- #

async def tls_flow():
    print("\n=== P2-10 TLS (self-signed) ===")
    if shutil.which("openssl") is None:
        print("[SKIP] openssl not available")
        return

    with tempfile.TemporaryDirectory() as tmp:
        cert = os.path.join(tmp, "cert.pem")
        key = os.path.join(tmp, "key.pem")
        subprocess.run(
            [
                "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                "-keyout", key, "-out", cert, "-days", "1", "-subj", "/CN=127.0.0.1",
                "-addext", "subjectAltName=IP:127.0.0.1",
            ],
            check=True,
            capture_output=True,
        )

        config_a = make_config(tls_cert=cert, tls_key=key, tls_ca=cert)
        config_b = make_config(tls_cert=cert, tls_key=key, tls_ca=cert)
        node_a = Node(config_a)
        node_b = Node(config_b)
        await node_a.start()
        await node_b.start()

        record = node_b.self_peer_record()
        check("peer record advertises TLS", record.get("tls") is True)
        check("PING over TLS works", await node_a.ping_node(record) is True)

        node_b.add_peers([node_a.self_peer_record()])
        await node_b.register_to_network()
        await asyncio.sleep(0.3)
        check(
            "PEER_UPDATE over wss works",
            any(p["api_port"] == config_a.api_port for p in node_b.PEERS),
        )

        await node_b.stop()
        await node_a.stop()


# ---------------------------------------------------------------------- #
# P2-2 bootstrap/join
# ---------------------------------------------------------------------- #

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


async def join_flow():
    print("\n=== P2-1/2/3 bootstrap and join ===")
    config_a = make_config()
    config_b = make_config(bootstrap=f"127.0.0.1:{config_a.api_port}")
    node_a = Node(config_a)
    node_b = Node(config_b)
    await node_a.start()
    server_a, task_a = await start_api(node_a)

    await node_b.start()
    await asyncio.sleep(0.3)

    check(
        "B discovered the seed via /bootstrap",
        any(p["api_port"] == config_a.api_port for p in node_b.PEERS),
        f"B.PEERS count={len(node_b.PEERS)}",
    )
    check(
        "B registered itself and A learned it",
        any(p["api_port"] == config_b.api_port for p in node_a.PEERS),
        f"A.PEERS count={len(node_a.PEERS)}",
    )

    await node_b.stop()
    await node_a.stop()
    await stop_api(server_a, task_a)


# ---------------------------------------------------------------------- #
# P2-8 persistence
# ---------------------------------------------------------------------- #

async def persistence_flow(public_key, private_key):
    print("\n=== P2-8 SQLite persistence ===")
    with tempfile.TemporaryDirectory() as tmp:
        db_path = os.path.join(tmp, "node.db")
        config = make_config(db_path=db_path)
        node = Node(config)
        await node.start()

        result = await node.submit_transaction(
            signed_payload(public_key, private_key, 0, receiver=sample_address("7e"), value=7)
        )
        check("transaction committed before restart", result["status"] == "committed")
        chain_len = len(node.blockchain.chain)
        balances = dict(node.blockchain.SAN)
        nonces = dict(node.blockchain.nonces)
        await node.stop()

        check("database file created", os.path.exists(db_path))

        restored = Node(make_config(db_path=db_path))
        check("chain restored from disk", len(restored.blockchain.chain) == chain_len)
        check("balances restored from disk", restored.blockchain.SAN == balances)
        check("nonces restored from disk", restored.blockchain.nonces == nonces)
        await restored.stop()

        # Genesis mismatch (different allocation) must be refused loudly.
        try:
            Node(
                make_config(
                    db_path=db_path,
                    genesis_allocations={sample_address("dead"): 5 * SAN_BASE},
                )
            )
            check("mismatched genesis is rejected", False, "no error raised")
        except RuntimeError:
            check("mismatched genesis is rejected", True)


# ---------------------------------------------------------------------- #
# P2-6 concurrency
# ---------------------------------------------------------------------- #

async def concurrency_flow():
    print("\n=== P2-6 concurrent submissions ===")
    public_1, private_1 = crypto.generate_keypair()
    public_2, private_2 = crypto.generate_keypair()
    allocations = {public_1.hex(): 1_000_000 * SAN_BASE, public_2.hex(): 1_000_000 * SAN_BASE}

    node = Node(make_config(genesis_allocations=allocations),
                identity=NodeIdentity(private_key=private_1, public_key=public_1))
    await node.start()

    shared = sample_address("5a")
    tx1 = signed_payload(public_1, private_1, 0, receiver=shared, value=1)
    tx2 = signed_payload(public_2, private_2, 0, receiver=shared, value=2)
    results = await asyncio.gather(
        node.submit_transaction(tx1), node.submit_transaction(tx2)
    )
    statuses = sorted(result["status"] for result in results)
    check("both concurrent transactions committed", statuses == ["committed", "committed"], str(results))
    check("chain contains both blocks", len(node.blockchain.chain) == 3)
    check(
        "balances are consistent",
        node.blockchain.SAN.get(shared) == 3 * SAN_BASE
        and node.blockchain.nonces.get(address_of(public_1)) == 1
        and node.blockchain.nonces.get(address_of(public_2)) == 1,
    )

    same_nonce = signed_payload(public_1, private_1, 1, receiver=shared, value=1)
    first = await node.submit_transaction(same_nonce)
    check("second nonce committed", first["status"] == "committed", json.dumps(first))
    replay_results = await asyncio.gather(
        node.submit_transaction(dict(same_nonce)),
        node.submit_transaction(dict(same_nonce)),
        return_exceptions=True,
    )
    check(
        "replayed transaction rejected under concurrency",
        all(isinstance(item, (ValueError, TypeError)) for item in replay_results),
        str(replay_results),
    )

    await node.stop()


async def run_async(public_key, private_key):
    await peer_record_checks(public_key, private_key)
    await multi_key_flow()
    await mempool_gossip_flow(public_key, private_key)
    await tls_flow()
    await join_flow()
    await persistence_flow(public_key, private_key)
    await concurrency_flow()




# ---------------------------------------------------------------------- #
# B2 mempool gossip
# ---------------------------------------------------------------------- #

async def mempool_gossip_flow(public_key, private_key):
    print("\n=== B2 mempool gossip ===")
    config_a = make_config(block_threshold_fee=1_000_000)
    config_b = make_config(block_threshold_fee=1_000_000)
    node_a = Node(config_a)
    node_b = Node(config_b)
    await node_a.start()
    await node_b.start()

    node_b.add_peers([node_a.self_peer_record()])
    await node_b.register_to_network()
    await asyncio.sleep(0.3)

    payload = signed_payload(
        public_key, private_key, 0, receiver=sample_address("90"), value=1
    )
    result = await node_a.submit_transaction(payload)
    check("transaction stays pooled (threshold not reached)", result["status"] == "pooled", json.dumps(result))

    async def gossiped():
        return any(tx.tx_id == result["tx_id"] for tx in node_b.transaction_pool)

    await wait_until("B received the transaction via gossip", gossiped)
    check(
        "transaction gossiped into B's mempool",
        any(tx.tx_id == result["tx_id"] for tx in node_b.transaction_pool),
    )
    check("no block was produced while pooled", len(node_a.blockchain.chain) == 1)

    await node_b.stop()
    await node_a.stop()


async def wait_until(description, predicate, timeout=10.0, interval=0.2):
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


# ---------------------------------------------------------------------- #
# B3 RPC limits
# ---------------------------------------------------------------------- #

def rpc_limit_checks():
    print("\n=== B3 RPC rate and body limits ===")
    from fastapi.testclient import TestClient

    import app.main as main

    overrides = {
        "SAN_RPC_RATE_LIMIT": "3",
        "SAN_RPC_RATE_WINDOW": "60",
        "SAN_RPC_MAX_BODY": "2000",
    }
    previous = {key: os.environ.get(key) for key in overrides}
    os.environ.update(overrides)
    try:
        limited_app = main.create_app()
        with TestClient(limited_app) as client:
            codes = [client.get("/health").status_code for _ in range(6)]
            check("rate limit returns 429", codes.count(429) >= 1, str(codes))

            oversized = client.post(
                "/transaction",
                content=b"x" * 5000,
                headers={"Content-Type": "application/json"},
            )
            check(
                "oversized request body rejected with 413",
                oversized.status_code == 413,
                str(oversized.status_code),
            )
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


if __name__ == "__main__":
    PUBLIC_KEY, PRIVATE_KEY = setup_identity()
    identity_checks()
    controller_checks()
    schema_checks()
    asyncio.run(run_async(PUBLIC_KEY, PRIVATE_KEY))
    rpc_limit_checks()
    sys.exit(summary())
