"""Storage layer tests: KV backends, chain store, indexes, snapshots, migration."""

import pytest

from blockchain import crypto
from blockchain.Block import Block
from blockchain.persistence import SCHEMA_VERSION, ChainStore, transaction_id
from blockchain.storage import LMDBStore, MemoryStore, SchemaMismatch, WriteBatch
from blockchain.Transaction import Transaction

CHAIN_ID = "san-test"


def make_block(index, previous_hash, transactions=None):
    return Block(
        index=index,
        previous_block_hash=previous_hash,
        validator="v",
        validator_signature="s",
        transactions=transactions or [],
        chain_id=CHAIN_ID,
        state_root=None,
    )


def signed_tx(nonce: int) -> dict:
    public_key, private_key = crypto.generate_keypair()
    payload = {
        "chain_id": CHAIN_ID,
        "sender": public_key.hex(),
        "nonce": nonce,
        "receiver": "0x" + "ab" * 20,
        "value": 1,
    }
    payload["signature"] = Transaction.sign_payload(payload, private_key)
    return Transaction(payload, 1).to_dict()


# ---------------------------------------------------------------------- #
# Key-value backends
# ---------------------------------------------------------------------- #

def test_memory_store_basics_and_batch():
    store = MemoryStore()
    batch = WriteBatch()
    batch.put(b"a:1", b"one")
    batch.put(b"a:2", b"two")
    store.write_batch(batch)

    assert store.get(b"a:1") == b"one"
    assert [key for key, _ in store.prefix_iterator(b"a:")] == [b"a:1", b"a:2"]
    assert [key for key, _ in store.prefix_iterator(b"a:", start=b"a:2")] == [b"a:2"]

    batch = WriteBatch()
    batch.delete(b"a:1")
    store.write_batch(batch)
    assert store.get(b"a:1") is None


def test_lmdb_store_iteration_and_auto_grow(tmp_path):
    store = LMDBStore(str(tmp_path / "kv.db"), map_size=1 << 20)
    try:
        store.put(b"n:000000000001", b"a")
        store.put(b"n:000000000002", b"b")
        assert [value for _, value in store.prefix_iterator(b"n:")] == [b"a", b"b"]
        assert [value for _, value in store.prefix_iterator(b"n:", start=b"n:000000000002")] == [b"b"]

        batch = WriteBatch()
        batch.put(b"b:1", b"x")
        batch.delete(b"n:000000000001")
        store.write_batch(batch)
        assert store.get(b"b:1") == b"x" and store.get(b"n:000000000001") is None

        # Writing past the map size must grow transparently.
        big = b"z" * (1 << 20)
        for index in range(4):
            store.put(f"big:{index}".encode(), big)
        assert store.get(b"big:3") == big
    finally:
        store.close()


# ---------------------------------------------------------------------- #
# Chain store
# ---------------------------------------------------------------------- #

def test_chain_store_indexes_receipts_and_pruning(tmp_path):
    store = ChainStore(str(tmp_path / "chain.db"))
    try:
        genesis = make_block(0, "0", ["hello"])
        store.append_block(genesis, receipts=[], state={"balances": {"a": 1}}, storage={}, head=0)

        tx = signed_tx(0)
        block1 = make_block(1, genesis.current_block_hash, [tx])
        receipts = [
            {
                "tx_index": 0,
                "tx_id": transaction_id(tx),
                "status": "success",
                "gas_used": 21,
                "logs": [],
            }
        ]
        store.append_block(block1, receipts=receipts, state={"balances": {"a": 2}}, storage={}, head=1)

        assert store.highest_height() == 1
        assert store.count_blocks() == 2
        assert store.block_hash_at(1) == block1.current_block_hash
        assert store.block_by_hash(block1.current_block_hash).index == 1

        assert [block.index for block in store.load_chain()] == [0, 1]
        assert [block.index for block in store.load_chain(1)] == [1]
        assert [block.index for block in store.load_chain(0, 1)] == [0]

        location = store.tx_lookup(transaction_id(tx))
        assert location == {"block_hash": block1.current_block_hash, "block_index": 1, "tx_index": 0}
        assert store.receipts_for_block(1) == receipts
        assert store.load_state() == {"balances": {"a": 2}}

        # add a second block, then prune everything below it
        block2 = make_block(2, block1.current_block_hash, [signed_tx(1)])
        store.append_block(block2, receipts=[], state={"balances": {"a": 3}}, storage={}, head=2)
        removed = store.prune_blocks_below(2)
        assert removed == 1
        assert store.block_hash_at(1) is None
        assert store.tx_lookup(transaction_id(tx)) is None  # index pruned with the block
        assert [block.index for block in store.load_chain()] == [0, 2]
        assert store.get_meta("pruned_below") == "2"
    finally:
        store.close()


def test_chain_store_snapshots_replace_and_delete(tmp_path):
    store = ChainStore(str(tmp_path / "snap.db"))
    try:
        genesis = make_block(0, "0", ["hello"])
        store.append_block(genesis, receipts=[], state={"balances": {}}, storage={}, head=0)
        for index in (1, 2, 3):
            previous = store.block_by_hash(store.block_hash_at(index - 1))
            store.append_block(
                make_block(index, previous.current_block_hash),
                receipts=[],
                head=index,
            )

        store.save_snapshot(2, store.block_hash_at(2), {"balances": {"x": 1}}, {})
        snapshot = store.load_latest_snapshot()
        assert snapshot["height"] == 2 and snapshot["state"] == {"balances": {"x": 1}}

        store.delete_blocks_after(1)
        assert [block.index for block in store.load_chain()] == [0, 1]

        replacement = make_block(1, store.block_hash_at(0))
        store.replace_chain([store.block_by_hash(store.block_hash_at(0)), replacement])
        assert store.load_chain()[1].current_block_hash == replacement.current_block_hash
    finally:
        store.close()


def test_schema_version_guard():
    store = MemoryStore()
    store.put(b"m:schema_version", str(SCHEMA_VERSION + 1).encode("ascii"))
    with pytest.raises(SchemaMismatch):
        ChainStore(":memory:", store=store)


def test_finality_bookkeeping_survives_restart(tmp_path):
    from blockchain.identity import NodeIdentity
    from network.config import NodeConfig
    from network.Node import Node

    public_key, private_key = crypto.generate_keypair()
    path = str(tmp_path / "finality.kv")
    config = NodeConfig(
        chain_id=CHAIN_ID,
        genesis_allocations={public_key.hex(): 10 ** 8},
        db_path=path,
    )
    identity = NodeIdentity(private_key=private_key, public_key=public_key)

    node = Node(config, identity=identity)
    try:
        node._finality_sets[1] = {"0xaa": 10}
        node._finality_votes[1] = {"hash": {"0xaa": {"signature": "s"}}}
        node._persist_finality()
    finally:
        node.store.close()

    fresh = Node(config, identity=identity)
    try:
        assert fresh._finality_sets[1] == {"0xaa": 10}
        assert fresh._finality_votes[1]["hash"]["0xaa"]["signature"] == "s"
    finally:
        fresh.store.close()


def test_malformed_finality_blob_is_ignored(tmp_path):
    from blockchain.identity import NodeIdentity
    from network.config import NodeConfig
    from network.Node import Node

    public_key, private_key = crypto.generate_keypair()
    path = str(tmp_path / "malformed.kv")
    config = NodeConfig(
        chain_id=CHAIN_ID,
        genesis_allocations={public_key.hex(): 10 ** 8},
        db_path=path,
    )
    identity = NodeIdentity(private_key=private_key, public_key=public_key)

    node = Node(config, identity=identity)
    node.store.set_meta("finality_state", "[1, 2, 3]")
    node.store.close()

    fresh = Node(config, identity=identity)
    try:
        assert fresh._finality_sets == {} and fresh._finality_votes == {}
    finally:
        fresh.store.close()


def test_persisted_block_without_state_root_is_refused(tmp_path):
    from blockchain.Block import Block
    from blockchain.identity import NodeIdentity
    from network.config import NodeConfig
    from network.Node import Node

    public_key, private_key = crypto.generate_keypair()
    path = str(tmp_path / "no_root.kv")
    store = ChainStore(path)
    genesis = make_block(0, "0")
    store.append_block(genesis, receipts=[], state={"balances": {}}, storage={}, head=0)
    store.append_block(
        Block(
            index=1,
            previous_block_hash=genesis.current_block_hash,
            validator="v",
            validator_signature="s",
            transactions=[],
            chain_id=CHAIN_ID,
            state_root=None,
        ),
        receipts=[],
        head=1,
    )
    store.close()

    with pytest.raises(RuntimeError):
        Node(
            NodeConfig(
                chain_id=CHAIN_ID,
                genesis_allocations={public_key.hex(): 10 ** 8},
                db_path=path,
            ),
            identity=NodeIdentity(private_key=private_key, public_key=public_key),
        )


def test_finality_checkpoint_never_moves_backwards(tmp_path):
    import asyncio

    from blockchain.identity import NodeIdentity
    from network.config import NodeConfig
    from network.Node import Node

    public_key, private_key = crypto.generate_keypair()
    path = str(tmp_path / "monotonic.kv")
    config = NodeConfig(
        chain_id=CHAIN_ID,
        genesis_allocations={public_key.hex(): 10 ** 8},
        db_path=path,
    )
    identity = NodeIdentity(private_key=private_key, public_key=public_key)

    async def produce(node):
        for _ in range(3):
            assert node._commit_block(node._build_block()) is True
        tip = node.blockchain.tip
        voter = "0x" + "aa" * 20
        node._finality_sets[tip.index] = {voter: 100}
        node._finality_votes[tip.index] = {tip.current_block_hash: {voter: {"signature": "s"}}}
        node._tally_finality(tip.index, tip.current_block_hash)
        assert node.finalized_height == 3

    node = Node(config, identity=identity)
    asyncio.run(produce(node))
    # Simulate a crash between the finality blob write and the metadata write.
    node.store.set_meta("finalized_height", "1")
    node.store.set_meta("finalized_hash", node.store.block_hash_at(1))
    node.store.close()

    fresh = Node(config, identity=identity)
    try:
        assert fresh.finalized_height == 3
    finally:
        fresh.store.close()
