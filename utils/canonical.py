"""Canonical JSON helpers.

Every signature, hash and persistence snapshot must serialize bytes
identically on every node, so all call sites go through these helpers.
"""

from __future__ import annotations

import json
from typing import Any

CANONICAL_JSON_KWARGS: dict[str, Any] = {"sort_keys": True, "separators": (",", ":")}


def dumps(payload: Any) -> str:
    return json.dumps(payload, **CANONICAL_JSON_KWARGS)


def dumps_bytes(payload: Any) -> bytes:
    return dumps(payload).encode("utf-8")
