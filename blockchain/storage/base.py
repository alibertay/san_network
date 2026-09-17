"""Key-value storage interface shared by all backends."""

from __future__ import annotations

from abc import ABC, abstractmethod
from typing import Iterator, Tuple


class StorageError(Exception):
    """Base class for storage failures."""


class SchemaMismatch(StorageError):
    """The on-disk schema is newer than this build understands."""


class WriteBatch:
    """A group of writes that must be applied atomically."""

    __slots__ = ("ops",)

    def __init__(self):
        # (key, value) with value=None meaning delete.
        self.ops: list[Tuple[bytes, bytes | None]] = []

    def put(self, key: bytes, value: bytes) -> None:
        if not isinstance(key, bytes) or not isinstance(value, bytes):
            raise TypeError("batch keys and values must be bytes")
        self.ops.append((key, value))

    def delete(self, key: bytes) -> None:
        self.ops.append((key, None))

    def __len__(self) -> int:
        return len(self.ops)


class KeyValueStore(ABC):
    """Minimal ordered key-value store with atomic batches."""

    @property
    @abstractmethod
    def path(self) -> str:
        ...

    @abstractmethod
    def get(self, key: bytes) -> bytes | None:
        ...

    @abstractmethod
    def put(self, key: bytes, value: bytes) -> None:
        ...

    @abstractmethod
    def delete(self, key: bytes) -> None:
        ...

    @abstractmethod
    def write_batch(self, batch: WriteBatch) -> None:
        """Apply every operation or none of them."""

    @abstractmethod
    def prefix_iterator(
        self, prefix: bytes, start: bytes | None = None
    ) -> Iterator[Tuple[bytes, bytes]]:
        """Iterate ``(key, value)`` pairs whose key starts with ``prefix``.

        ``start`` (optional) begins the iteration at that full key, which makes
        height-indexed range scans (fast sync) O(range) instead of O(all).
        """

    @abstractmethod
    def close(self) -> None:
        ...

    def flush(self) -> None:
        """Force durability (no-op for stores that never buffer)."""
