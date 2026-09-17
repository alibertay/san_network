"""Account address derivation.

A public key (1312 bytes for ML-DSA-44) is far too large to be a ledger key,
so the account address is the truncated hash of the public key::

    address = "0x" + sha3_256(public_key)[:20].hex()

Addresses are the canonical keys for balances, nonces, contract ownership and
API lookups. Transactions still carry the full public key in ``sender`` so
signatures can be verified; the node derives the address from it.
"""

from __future__ import annotations

import hashlib
import re

ADDRESS_BYTES = 20
ADDRESS_REGEX = re.compile(r"^0x[0-9a-f]{40}$")


class AddressError(ValueError):
    """Invalid address or public key."""


def address_from_public_key(public_key) -> str:
    """Derive the canonical ``0x`` address from a public key (bytes or hex)."""
    if isinstance(public_key, str):
        raw = public_key.strip()
        if raw.startswith(("0x", "0X")):
            raw = raw[2:]
        try:
            key_bytes = bytes.fromhex(raw)
        except ValueError as exc:
            raise AddressError(f"Public key is not hex: {public_key!r}") from exc
    elif isinstance(public_key, (bytes, bytearray)):
        key_bytes = bytes(public_key)
    else:
        raise AddressError(f"Unsupported public key type: {type(public_key)!r}")

    if not key_bytes:
        raise AddressError("Public key is empty")

    digest = hashlib.sha3_256(key_bytes).digest()
    return "0x" + digest[:ADDRESS_BYTES].hex()


def is_valid_address(value) -> bool:
    return isinstance(value, str) and bool(ADDRESS_REGEX.fullmatch(value.strip().lower()))


def normalize_address(value) -> str:
    """Validate and return the lowercase ``0x`` form of an address."""
    if not is_valid_address(value):
        raise AddressError(f"Invalid address: {value!r}")
    return value.strip().lower()


def try_address_from_public_key(public_key) -> str | None:
    try:
        return address_from_public_key(public_key)
    except AddressError:
        return None
