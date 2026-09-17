"""Merkle trees for transaction and state commitments.

Both trees use SHA3-256, canonical JSON leaves and node promotion on odd
levels (no duplicated hashes), so roots and proofs are deterministic on every
node. Proofs are inclusion proofs: a leaf plus its sibling path up to the root.
"""

from __future__ import annotations

import hashlib
from typing import Any, Iterable, Sequence

from utils import canonical

EMPTY_ROOT = hashlib.sha3_256(b"san-empty-tree").hexdigest()


def leaf_hash(leaf: Any) -> str:
    """Hash a leaf value (canonical JSON for anything non-string)."""
    if isinstance(leaf, bytes):
        payload = leaf
    elif isinstance(leaf, str):
        payload = leaf.encode("utf-8")
    else:
        payload = canonical.dumps_bytes(leaf)
    return hashlib.sha3_256(payload).hexdigest()


def _parent(left: str, right: str) -> str:
    return hashlib.sha3_256(f"{left}{right}".encode("utf-8")).hexdigest()


def merkle_root(leaves: Iterable[Any]) -> str:
    """Root of the Merkle tree over ``leaves`` (empty tree has a fixed root)."""
    level = [leaf_hash(leaf) for leaf in leaves]
    if not level:
        return EMPTY_ROOT

    while len(level) > 1:
        next_level = []
        for index in range(0, len(level) - 1, 2):
            next_level.append(_parent(level[index], level[index + 1]))
        if len(level) % 2 == 1:
            next_level.append(level[-1])  # promote the odd node unchanged
        level = next_level
    return level[0]


def merkle_proof(leaves: Sequence[Any], index: int) -> list[dict]:
    """Inclusion proof for ``leaves[index]``.

    Each step is ``{"position": "left"|"right", "hash": ...}`` where
    ``position`` is the side of the *provided* hash.
    """
    if not 0 <= index < len(leaves):
        raise IndexError(f"leaf index {index} out of range")

    level = [leaf_hash(leaf) for leaf in leaves]
    position = index
    proof: list[dict] = []

    while len(level) > 1:
        sibling = position ^ 1
        if sibling < len(level):
            proof.append(
                {
                    "position": "left" if sibling < position else "right",
                    "hash": level[sibling],
                }
            )
        # else: the odd node is promoted, no sibling at this level

        next_level = []
        for i in range(0, len(level) - 1, 2):
            next_level.append(_parent(level[i], level[i + 1]))
        if len(level) % 2 == 1:
            next_level.append(level[-1])
        level = next_level
        position //= 2

    return proof


def verify_merkle_proof(root: str, leaf: Any, proof: Sequence[dict], index: int) -> bool:
    """Verify an inclusion proof produced by :func:`merkle_proof`."""
    if not isinstance(root, str) or not root:
        return False
    if index < 0:
        return False

    current = leaf_hash(leaf)
    position = index

    for step in proof:
        try:
            sibling = step["hash"]
            side = step["position"]
        except (KeyError, TypeError):
            return False
        if not isinstance(sibling, str) or side not in ("left", "right"):
            return False

        if side == "left":
            current = _parent(sibling, current)
        else:
            current = _parent(current, sibling)

        position //= 2

    return current == root


# ---------------------------------------------------------------------- #
# Ledger state commitment
# ---------------------------------------------------------------------- #

def state_entries(
    balances,
    nonces,
    validators,
    total_slashed,
    storage,
    parameters=None,
    base_fee=1,
    total_burned=0,
) -> list[dict]:
    """Ordered, canonical state leaves (accounts, contracts, slashed counter)."""
    entries: list[dict] = []

    addresses = sorted(set(balances) | set(nonces) | set(validators))
    for address in addresses:
        entry: dict[str, Any] = {
            "key": f"acct:{address}",
            "address": address,
            "balance": int(balances.get(address, 0)),
            "nonce": int(nonces.get(address, 0)),
        }
        info = validators.get(address)
        if info:
            entry["validator"] = {
                "public_key": info.get("public_key"),
                "stake": int(info.get("stake", 0)),
                "joined_height": int(info.get("joined_height", 0)),
                "release_height": info.get("release_height"),
            }
        entries.append(entry)

    contracts = (storage or {}).get("contracts", {}) if isinstance(storage, dict) else {}
    for contract_id in sorted(contracts):
        record = contracts[contract_id] or {}
        entries.append({"key": f"code:{contract_id}", "bytecode": record.get("bytecode")})
        entries.append({"key": f"store:{contract_id}", "storage": record.get("storage")})

    # Audit fix: raw bytecode can write the VM's global variable space and
    # function table, so both must be part of the state commitment.
    if isinstance(storage, dict):
        entries.append({"key": "vmdata", "data": storage.get("data", {})})
        entries.append({"key": "vmfuncs", "functions": storage.get("functions", {})})

    entries.append({"key": "total_slashed", "total_slashed": int(total_slashed)})
    entries.append({"key": "total_burned", "total_burned": int(total_burned)})
    entries.append({"key": "base_fee", "base_fee": int(base_fee)})
    if parameters:
        entries.append(
            {
                "key": "parameters",
                "parameters": {key: int(value) for key, value in sorted(parameters.items())},
            }
        )
    return entries


def state_root(
    balances, nonces, validators, total_slashed, storage,
    parameters=None, base_fee=1, total_burned=0,
) -> str:
    return merkle_root(
        state_entries(
            balances, nonces, validators, total_slashed, storage,
            parameters, base_fee, total_burned,
        )
    )


def state_entry_proof(
    balances, nonces, validators, total_slashed, storage, key: str,
    parameters=None, base_fee=1, total_burned=0,
) -> dict | None:
    entries = state_entries(
        balances, nonces, validators, total_slashed, storage,
        parameters, base_fee, total_burned,
    )
    for index, entry in enumerate(entries):
        if entry.get("key") == key:
            return {
                "key": key,
                "index": index,
                "leaf": entry,
                "proof": merkle_proof(entries, index),
                "root": merkle_root(entries),
            }
    return None


def account_key(address: str) -> str:
    return f"acct:{address}"

