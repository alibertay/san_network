"""Storage backends for SAN Network.

The chain database is a namespaced key-value store (like Ethereum's
LevelDB/Pebble with column families): every write for a block happens in one
atomic batch, and indexes (canonical height -> hash, tx id -> location,
receipts) share the same commit as the block itself.

Backends:
    LMDBStore   production single-file, ACID, MVCC, crash-consistent
    MemoryStore in-process store for tests and ephemeral nodes
"""

from blockchain.storage.base import (
    KeyValueStore,
    SchemaMismatch,
    StorageError,
    WriteBatch,
)
from blockchain.storage.lmdb_store import LMDBStore
from blockchain.storage.memory import MemoryStore

__all__ = [
    "KeyValueStore",
    "WriteBatch",
    "StorageError",
    "SchemaMismatch",
    "LMDBStore",
    "MemoryStore",
]
