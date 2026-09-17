"""In-memory key-value store (tests, ephemeral nodes)."""

from __future__ import annotations

import threading
from typing import Iterator, Tuple

from blockchain.storage.base import KeyValueStore, WriteBatch


class MemoryStore(KeyValueStore):
    def __init__(self, path: str = ":memory:"):
        self._path = path
        self._data: dict[bytes, bytes] = {}
        self._lock = threading.RLock()

    @property
    def path(self) -> str:
        return self._path

    def get(self, key: bytes) -> bytes | None:
        with self._lock:
            return self._data.get(key)

    def put(self, key: bytes, value: bytes) -> None:
        with self._lock:
            self._data[key] = value

    def delete(self, key: bytes) -> None:
        with self._lock:
            self._data.pop(key, None)

    def write_batch(self, batch: WriteBatch) -> None:
        with self._lock:
            # Validate first so the batch is all-or-nothing.
            for key, value in batch.ops:
                if not isinstance(key, bytes):
                    raise TypeError("batch keys must be bytes")
            for key, value in batch.ops:
                if value is None:
                    self._data.pop(key, None)
                else:
                    self._data[key] = value

    def prefix_iterator(
        self, prefix: bytes, start: bytes | None = None
    ) -> Iterator[Tuple[bytes, bytes]]:
        with self._lock:
            items = sorted(
                (key, value)
                for key, value in self._data.items()
                if key.startswith(prefix) and (start is None or key >= start)
            )
        yield from items

    def close(self) -> None:
        with self._lock:
            self._data.clear()
