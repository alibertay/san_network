"""Chain persistence over a namespaced key-value store.

Layout (single LMDB file, byte prefixes act as column families):

    m:<name>            metadata (schema version, finalized checkpoint, genesis)
    h:<block_hash>      block header (hashed fields + signature)
    b:<block_hash>      block body (transactions)
    n:<height>          canonical index: height -> block hash
    r:<block_hash>      execution receipts
    t:<tx_id>           transaction index: tx id -> {block, position}
    S:state             ledger state snapshot
    S:storage           contract storage snapshot
    k:<height>          finalized state snapshots

Every block commit writes header, body, canonical index, receipts, the
transaction index and the resulting ledger state in **one atomic batch**, so a
crash can never leave a half-written block behind (the same contract geth
offers with its atomic block insert).
"""

from __future__ import annotations

import hashlib
import json
import logging
import threading
from typing import Any, Iterable

from blockchain.Block import Block
from blockchain.storage import KeyValueStore, LMDBStore, MemoryStore, SchemaMismatch, WriteBatch
from blockchain.Transaction import Transaction

logger = logging.getLogger(__name__)

SCHEMA_VERSION = 1
KEY_META = b"m:"
KEY_HEADER = b"h:"
KEY_BODY = b"b:"
KEY_CANON = b"n:"
KEY_RECEIPTS = b"r:"
KEY_TX = b"t:"
KEY_STATE = b"m:__state"      # keeps the single-writer state next to metadata
KEY_STORAGE = b"m:__storage"
KEY_SNAPSHOT = b"k:"


def _dump(payload) -> bytes:
    return json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")


def _load(raw: bytes):
    return json.loads(raw.decode("utf-8"))


def _height_key(height: int) -> bytes:
    return KEY_CANON + f"{int(height):012d}".encode("ascii")


def transaction_id(tx: dict) -> str:
    return hashlib.sha256(Transaction.serialize_message(tx)).hexdigest()


class ChainStore:
    """Block/state/receipt storage with atomic per-block commits."""

    def __init__(
        self,
        path: str,
        *,
        backend: str = "lmdb",
        map_size: int | None = None,
        sync: bool = False,
        store: KeyValueStore | None = None,
    ):
        self.path = path
        self._lock = threading.RLock()

        if store is not None:
            self._kv: KeyValueStore = store
        elif backend == "memory":
            self._kv = MemoryStore(path)
        else:
            kwargs: dict[str, Any] = {"sync": sync}
            if map_size:
                kwargs["map_size"] = int(map_size)
            self._kv = LMDBStore(path, **kwargs)

        self._ensure_schema()

    # ------------------------------------------------------------------ #
    # Metadata and schema
    # ------------------------------------------------------------------ #

    def get_meta(self, key: str) -> str | None:
        raw = self._kv.get(KEY_META + key.encode("utf-8"))
        return raw.decode("utf-8") if raw is not None else None

    def set_meta(self, key: str, value: str) -> None:
        with self._lock:
            self._kv.put(KEY_META + key.encode("utf-8"), str(value).encode("utf-8"))

    def _ensure_schema(self) -> None:
        raw = self._kv.get(KEY_META + b"schema_version")
        if raw is None:
            self._kv.put(KEY_META + b"schema_version", str(SCHEMA_VERSION).encode("ascii"))
            return
        try:
            version = int(raw.decode("ascii"))
        except ValueError as exc:
            raise SchemaMismatch(f"Invalid schema version marker: {raw!r}") from exc
        if version > SCHEMA_VERSION:
            raise SchemaMismatch(
                f"On-disk schema {version} is newer than this build ({SCHEMA_VERSION}); "
                "upgrade the node or point SAN_DB_PATH at a fresh database"
            )

    # ------------------------------------------------------------------ #
    # Blocks
    # ------------------------------------------------------------------ #

    def is_empty(self) -> bool:
        return self.highest_height() is None

    def highest_height(self) -> int | None:
        last_key = None
        for key, _ in self._kv.prefix_iterator(KEY_CANON):
            last_key = key
        if last_key is None:
            return None
        return int(last_key[-12:].decode("ascii"))

    def block_hash_at(self, height: int) -> str | None:
        raw = self._kv.get(_height_key(height))
        return raw.decode("ascii") if raw is not None else None

    def block_by_hash(self, block_hash: str) -> Block | None:
        header_raw = self._kv.get(KEY_HEADER + block_hash.encode("ascii"))
        if header_raw is None:
            return None
        body_raw = self._kv.get(KEY_BODY + block_hash.encode("ascii"))
        payload = _load(header_raw)
        payload["transactions"] = _load(body_raw) if body_raw is not None else []
        return Block.from_dict(payload)

    def load_block(self, height: int) -> Block | None:
        block_hash = self.block_hash_at(height)
        return self.block_by_hash(block_hash) if block_hash else None

    def load_chain(self, from_height: int = 0, limit: int | None = None) -> list[Block]:
        chain: list[Block] = []
        for _, block_hash in self._kv.prefix_iterator(
            KEY_CANON, start=_height_key(from_height)
        ):
            block = self.block_by_hash(block_hash.decode("ascii"))
            if block is None:
                continue
            chain.append(block)
            if limit is not None and len(chain) >= limit:
                break
        return chain

    def count_blocks(self) -> int:
        return sum(1 for _ in self._canonical_entries())

    def append_block(
        self,
        block: Block,
        *,
        receipts: list | None = None,
        state: dict | None = None,
        storage: dict | None = None,
        head: int | None = None,
    ) -> None:
        """Atomically store a block plus its indexes, receipts and state."""
        block_hash = block.current_block_hash
        header = block.to_header_dict()
        body = [block._transaction_to_dict(tx) for tx in block.transactions]

        batch = WriteBatch()
        batch.put(KEY_HEADER + block_hash.encode("ascii"), _dump(header))
        batch.put(KEY_BODY + block_hash.encode("ascii"), _dump(body))
        batch.put(_height_key(block.index), block_hash.encode("ascii"))

        if receipts is not None:
            batch.put(KEY_RECEIPTS + block_hash.encode("ascii"), _dump(receipts))
            for receipt in receipts:
                tx_id = receipt.get("tx_id")
                if not tx_id:
                    continue
                batch.put(
                    KEY_TX + tx_id.encode("ascii"),
                    _dump(
                        {
                            "block_hash": block_hash,
                            "block_index": block.index,
                            "tx_index": receipt.get("tx_index"),
                        }
                    ),
                )

        if state is not None:
            batch.put(KEY_STATE, _dump(state))
        if storage is not None:
            batch.put(KEY_STORAGE, _dump(storage))

        batch.put(KEY_META + b"head", str(head if head is not None else block.index).encode("ascii"))

        with self._lock:
            self._kv.write_batch(batch)

    def save_block(self, block: Block) -> None:
        """Compatibility helper: append a block without state/receipts."""
        self.append_block(block)

    def replace_chain(
        self,
        blocks: Iterable[Block],
        *,
        state: dict | None = None,
        storage: dict | None = None,
        receipts_by_index: dict[int, list] | None = None,
    ) -> None:
        """Atomically replace the stored chain (used by reorgs and replay).

        Receipts and transaction indexes are rebuilt from the replacement
        blocks as part of the same batch (audit fix: a reorg used to drop
        them, so /tx and /receipt lookups broke after a restart).
        """
        blocks = list(blocks)
        batch = WriteBatch()
        # Keep receipts of blocks that survive unchanged (e.g. the anchor):
        # they are re-attached below instead of being dropped with the batch.
        old_receipts = {
            key: value for key, value in self._kv.prefix_iterator(KEY_RECEIPTS)
        }
        for key, _ in list(self._canonical_entries()):
            batch.delete(key)
        for key, _ in list(self._kv.prefix_iterator(KEY_HEADER)):
            batch.delete(key)
        for key, _ in list(self._kv.prefix_iterator(KEY_BODY)):
            batch.delete(key)
        for key, _ in list(self._kv.prefix_iterator(KEY_RECEIPTS)):
            batch.delete(key)
        for key, _ in list(self._kv.prefix_iterator(KEY_TX)):
            batch.delete(key)

        supplied = receipts_by_index or {}

        def put_receipts(block_hash: bytes, block_hash_text: str, index: int, receipts) -> None:
            batch.put(KEY_RECEIPTS + block_hash, _dump(receipts))
            for receipt in receipts:
                tx_id = receipt.get("tx_id")
                if not tx_id:
                    continue
                batch.put(
                    KEY_TX + tx_id.encode("ascii"),
                    _dump(
                        {
                            "block_hash": block_hash_text,
                            "block_index": index,
                            "tx_index": receipt.get("tx_index"),
                        }
                    ),
                )

        for block in blocks:
            block_hash = block.current_block_hash.encode("ascii")
            batch.put(KEY_HEADER + block_hash, _dump(block.to_header_dict()))
            batch.put(
                KEY_BODY + block_hash,
                _dump([block._transaction_to_dict(tx) for tx in block.transactions]),
            )
            batch.put(_height_key(block.index), block_hash)

            receipts = supplied.get(block.index)
            if receipts is None:
                raw = old_receipts.get(KEY_RECEIPTS + block_hash)
                if raw is None:
                    continue
                try:
                    stored = _load(raw)
                except ValueError:
                    continue
                put_receipts(block_hash, block.current_block_hash, block.index, stored)
                continue
            put_receipts(block_hash, block.current_block_hash, block.index, receipts)

        if blocks:
            batch.put(KEY_META + b"head", str(blocks[-1].index).encode("ascii"))
        if state is not None:
            batch.put(KEY_STATE, _dump(state))
        if storage is not None:
            batch.put(KEY_STORAGE, _dump(storage))

        with self._lock:
            self._kv.write_batch(batch)

    def delete_blocks_after(self, height: int) -> None:
        batch = WriteBatch()
        for key, block_hash in list(self._canonical_entries()):
            if int(key[-12:].decode("ascii")) > height:
                batch.delete(key)
                batch.delete(KEY_HEADER + block_hash)
                batch.delete(KEY_BODY + block_hash)
                batch.delete(KEY_RECEIPTS + block_hash)
        for key, value in list(self._kv.prefix_iterator(KEY_TX)):
            try:
                if int(_load(value).get("block_index", -1)) > height:
                    batch.delete(key)
            except (ValueError, AttributeError):
                continue
        batch.put(KEY_META + b"head", str(height).encode("ascii"))
        with self._lock:
            self._kv.write_batch(batch)

    def prune_blocks_below(self, height: int) -> int:
        """Drop block bodies/indexes below ``height`` (genesis is always kept).

        Returns the number of canonical entries removed.
        """
        removed = 0
        batch = WriteBatch()
        for key, block_hash in list(self._canonical_entries()):
            block_height = int(key[-12:].decode("ascii"))
            if block_height == 0 or block_height >= height:
                continue
            batch.delete(key)
            batch.delete(KEY_HEADER + block_hash)
            batch.delete(KEY_BODY + block_hash)
            batch.delete(KEY_RECEIPTS + block_hash)
            removed += 1
        for key, value in list(self._kv.prefix_iterator(KEY_TX)):
            try:
                if int(_load(value).get("block_index", -1)) < height:
                    batch.delete(key)
            except (ValueError, AttributeError):
                continue
        batch.put(KEY_META + b"pruned_below", str(int(height)).encode("ascii"))
        with self._lock:
            self._kv.write_batch(batch)
        return removed

    # ------------------------------------------------------------------ #
    # Receipts and transaction index
    # ------------------------------------------------------------------ #

    def receipts_for_block(self, height: int) -> list | None:
        block_hash = self.block_hash_at(height)
        if block_hash is None:
            return None
        raw = self._kv.get(KEY_RECEIPTS + block_hash.encode("ascii"))
        return _load(raw) if raw is not None else None

    def tx_lookup(self, tx_id: str) -> dict | None:
        raw = self._kv.get(KEY_TX + tx_id.encode("ascii"))
        return _load(raw) if raw is not None else None

    # ------------------------------------------------------------------ #
    # Ledger state
    # ------------------------------------------------------------------ #

    def save_state(self, state: dict) -> None:
        with self._lock:
            self._kv.put(KEY_STATE, _dump(state))

    def load_state(self) -> dict | None:
        raw = self._kv.get(KEY_STATE)
        return _load(raw) if raw is not None else None

    def save_storage(self, storage: dict) -> None:
        with self._lock:
            self._kv.put(KEY_STORAGE, _dump(storage))

    def load_storage(self) -> dict | None:
        raw = self._kv.get(KEY_STORAGE)
        return _load(raw) if raw is not None else None

    # ------------------------------------------------------------------ #
    # Snapshots
    # ------------------------------------------------------------------ #

    def save_snapshot(self, height: int, block_hash: str, state: dict, storage: dict) -> None:
        with self._lock:
            self._kv.put(
                KEY_SNAPSHOT + f"{int(height):012d}".encode("ascii"),
                _dump({"hash": block_hash, "state": state, "storage": storage}),
            )
            self._kv.put(KEY_META + b"snapshot_height", str(int(height)).encode("ascii"))

    def list_snapshots(self) -> list[dict]:
        """All snapshots, newest first."""
        snapshots = []
        for key, raw in self._kv.prefix_iterator(KEY_SNAPSHOT):
            payload = _load(raw)
            snapshots.append(
                {
                    "height": int(key[len(KEY_SNAPSHOT):].decode("ascii")),
                    "hash": payload["hash"],
                    "state": payload["state"],
                    "storage": payload["storage"],
                }
            )
        snapshots.sort(key=lambda item: item["height"], reverse=True)
        return snapshots

    def delete_mismatched_snapshots(self, canonical_hash_at) -> int:
        """Drop snapshots that are not on the canonical chain (audit fix)."""
        removed = 0
        batch = WriteBatch()
        for snapshot in self.list_snapshots():
            if canonical_hash_at(snapshot["height"]) != snapshot["hash"]:
                batch.delete(
                    KEY_SNAPSHOT + f"{int(snapshot['height']):012d}".encode("ascii")
                )
                removed += 1
        if removed:
            with self._lock:
                self._kv.write_batch(batch)
        return removed

    def load_latest_snapshot(self) -> dict | None:
        latest_key = None
        for key, _ in self._kv.prefix_iterator(KEY_SNAPSHOT):
            latest_key = key
        if latest_key is None:
            return None
        raw = self._kv.get(latest_key)
        if raw is None:  # pragma: no cover - deleted between iterations
            return None
        payload = _load(raw)
        return {
            "height": int(latest_key[len(KEY_SNAPSHOT):].decode("ascii")),
            "hash": payload["hash"],
            "state": payload["state"],
            "storage": payload["storage"],
        }

    # ------------------------------------------------------------------ #
    # Lifecycle
    # ------------------------------------------------------------------ #

    def flush(self) -> None:
        self._kv.flush()

    def close(self) -> None:
        self._kv.close()

    # ------------------------------------------------------------------ #
    # Internal helpers
    # ------------------------------------------------------------------ #

    def _canonical_entries(self) -> list[tuple[bytes, bytes]]:
        return list(self._kv.prefix_iterator(KEY_CANON))
