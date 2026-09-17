"""LMDB-backed key-value store.

LMDB gives us what a chain database needs without a server: single-file,
ACID transactions with MVCC readers, crash-consistent commits and ordered
iteration. The map size grows automatically when it fills up.
"""

from __future__ import annotations

import logging
import os
from typing import Iterator, Tuple

import lmdb

from blockchain.storage.base import KeyValueStore, StorageError, WriteBatch

logger = logging.getLogger(__name__)

DEFAULT_MAP_SIZE = 1 << 30  # 1 GiB, grown automatically when full
DEFAULT_MAX_READERS = 126


class LMDBStore(KeyValueStore):
    def __init__(
        self,
        path: str,
        *,
        map_size: int = DEFAULT_MAP_SIZE,
        max_readers: int = DEFAULT_MAX_READERS,
        sync: bool = False,
    ):
        self._path = path
        directory = os.path.dirname(os.path.abspath(path))
        os.makedirs(directory, exist_ok=True)
        self._map_size = max(int(map_size), 1 << 20)
        self._sync = bool(sync)
        try:
            self._env = lmdb.open(
                path,
                subdir=False,
                map_size=self._map_size,
                max_readers=max_readers,
                sync=self._sync,
                metasync=self._sync,
                readonly=False,
            )
        except lmdb.Error as exc:  # pragma: no cover - environment dependent
            raise StorageError(f"Could not open LMDB store {path}: {exc}") from exc

    @property
    def path(self) -> str:
        return self._path

    # ------------------------------------------------------------------ #
    # Reads
    # ------------------------------------------------------------------ #

    def get(self, key: bytes) -> bytes | None:
        with self._env.begin() as txn:
            return txn.get(key)

    def prefix_iterator(
        self, prefix: bytes, start: bytes | None = None
    ) -> Iterator[Tuple[bytes, bytes]]:
        with self._env.begin() as txn:
            cursor = txn.cursor()
            if not cursor.set_range(start or prefix):
                return
            for key, value in cursor:
                if not key.startswith(prefix):
                    break
                yield key, value

    # ------------------------------------------------------------------ #
    # Writes
    # ------------------------------------------------------------------ #

    def put(self, key: bytes, value: bytes) -> None:
        batch = WriteBatch()
        batch.put(key, value)
        self.write_batch(batch)

    def delete(self, key: bytes) -> None:
        batch = WriteBatch()
        batch.delete(key)
        self.write_batch(batch)

    def write_batch(self, batch: WriteBatch) -> None:
        if not batch.ops:
            return
        try:
            self._apply(batch)
        except lmdb.MapFullError:
            self._grow()
            self._apply(batch)

    def _apply(self, batch: WriteBatch) -> None:
        with self._env.begin(write=True) as txn:
            for key, value in batch.ops:
                if value is None:
                    txn.delete(key)
                else:
                    txn.put(key, value)

    def _grow(self) -> None:
        new_size = self._map_size * 2
        logger.warning("LMDB map is full; growing to %d bytes", new_size)
        try:
            self._env.set_mapsize(new_size)
            self._map_size = new_size
        except lmdb.Error as exc:  # pragma: no cover - environment dependent
            raise StorageError(f"Could not grow LMDB map: {exc}") from exc

    # ------------------------------------------------------------------ #
    # Lifecycle
    # ------------------------------------------------------------------ #

    def flush(self) -> None:
        self._env.sync()

    def close(self) -> None:
        try:
            self._env.sync()
        except lmdb.Error:  # pragma: no cover - best effort
            pass
        self._env.close()
