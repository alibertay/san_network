"""Post-quantum signature helper for SAN Network.

The project signs/verifies with CRYSTALS-Dilithium2, which has been
standardized by NIST as ML-DSA-44 (FIPS 204).  The ``pqcrypto`` package
changed its public API in 1.0.0:

* ``pqcrypto >= 1.0``  -> ``pqcrypto.sign.ml_dsa_44`` (``keygen``/``sign``/``verify``)
* ``pqcrypto 0.1.x``   -> ``pqcrypto.sign.dilithium2`` (``generate_keypair``/``sign``/``verify``)

This module hides that difference behind one stable interface so the rest of
the code base never talks to a specific backend version::

    generate_keypair() -> (public_key, secret_key)
    sign(message: bytes, secret_key: bytes) -> bytes
    verify(message: bytes, signature: bytes, public_key: bytes) -> bool

``verify`` never raises for a bad signature, it simply returns ``False``.
"""

from __future__ import annotations

import logging
from typing import Callable, Optional, Tuple

logger = logging.getLogger(__name__)

PUBLIC_KEY_SIZE: Optional[int] = None
SECRET_KEY_SIZE: Optional[int] = None
SIGNATURE_SIZE: Optional[int] = None
BACKEND_NAME: str = "unavailable"

_generate_keypair: Optional[Callable[[], Tuple[bytes, bytes]]] = None
_sign: Optional[Callable[[bytes, bytes], bytes]] = None
_verify: Optional[Callable[[bytes, bytes, bytes], bool]] = None


def _load_modern_backend() -> bool:
    """pqcrypto >= 1.0 (ML-DSA-44, FIPS 204)."""
    global PUBLIC_KEY_SIZE, SECRET_KEY_SIZE, SIGNATURE_SIZE
    global BACKEND_NAME, _generate_keypair, _sign, _verify

    try:
        from pqcrypto.sign import ml_dsa_44 as backend  # type: ignore
    except ImportError:
        return False

    if not hasattr(backend, "keygen"):
        return False

    def generate_keypair() -> Tuple[bytes, bytes]:
        return backend.keygen()

    def sign(message: bytes, secret_key: bytes) -> bytes:
        return backend.sign(secret_key, message)

    def verify(message: bytes, signature: bytes, public_key: bytes) -> bool:
        try:
            backend.verify(public_key, message, signature)
            return True
        except Exception:
            return False

    _generate_keypair, _sign, _verify = generate_keypair, sign, verify
    PUBLIC_KEY_SIZE = getattr(backend, "PUBLIC_KEY_SIZE", None)
    SECRET_KEY_SIZE = getattr(backend, "SECRET_KEY_SIZE", None)
    SIGNATURE_SIZE = getattr(backend, "SIGNATURE_SIZE", None)
    BACKEND_NAME = "pqcrypto.sign.ml_dsa_44"
    return True


def _load_legacy_backend() -> bool:
    """pqcrypto 0.1.x (CRYSTALS-Dilithium2)."""
    global PUBLIC_KEY_SIZE, SECRET_KEY_SIZE, SIGNATURE_SIZE
    global BACKEND_NAME, _generate_keypair, _sign, _verify

    try:
        from pqcrypto.sign import dilithium2 as backend  # type: ignore
    except ImportError:
        return False

    if not hasattr(backend, "generate_keypair"):
        return False

    def generate_keypair() -> Tuple[bytes, bytes]:
        return backend.generate_keypair()

    def sign(message: bytes, secret_key: bytes) -> bytes:
        return backend.sign(message, secret_key)

    def verify(message: bytes, signature: bytes, public_key: bytes) -> bool:
        try:
            result = backend.verify(signature, message, public_key)
        except Exception:
            return False
        return bool(result)

    _generate_keypair, _sign, _verify = generate_keypair, sign, verify
    PUBLIC_KEY_SIZE = getattr(backend, "PUBLIC_KEY_SIZE", None)
    SECRET_KEY_SIZE = getattr(backend, "SECRET_KEY_SIZE", None)
    SIGNATURE_SIZE = getattr(backend, "SIGNATURE_SIZE", None)
    BACKEND_NAME = "pqcrypto.sign.dilithium2"
    return True


if _load_modern_backend() or _load_legacy_backend():
    logger.debug("Post-quantum signature backend: %s", BACKEND_NAME)
else:  # pragma: no cover - only when dependency is missing
    logger.warning(
        "No post-quantum signature backend available. Install 'pqcrypto>=1.0'."
    )


def is_available() -> bool:
    return _sign is not None


def generate_keypair() -> Tuple[bytes, bytes]:
    if _generate_keypair is None:
        raise RuntimeError("No post-quantum signature backend available")
    return _generate_keypair()


def sign(message: bytes, secret_key: bytes) -> bytes:
    if _sign is None:
        raise RuntimeError("No post-quantum signature backend available")
    return _sign(message, secret_key)


def verify(message: bytes, signature: bytes, public_key: bytes) -> bool:
    if _verify is None:
        raise RuntimeError("No post-quantum signature backend available")
    return _verify(message, signature, public_key)

