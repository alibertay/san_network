"""Node identity and post-quantum key management.

A SAN node signs blocks and consensus votes with an ML-DSA-44 (Dilithium2)
key pair. Keys are resolved in this order:

1. an explicit :class:`NodeIdentity` passed to the node (embedders/tests)
2. ``SAN_KEY_FILE`` / ``NodeConfig.key_file`` (JSON, see :meth:`save`)
3. ``PRIVATE_KEY`` / ``PUBLIC_KEY`` environment variables (hex)
4. a freshly generated ephemeral key (a warning is logged; not persisted)

Generate a persistent key file with::

    python -m blockchain.identity --output san_key.json
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import stat
from dataclasses import dataclass

from blockchain import crypto

logger = logging.getLogger(__name__)


class IdentityError(Exception):
    """Invalid or unusable node identity."""


def _check_size(name: str, value: bytes | None, expected: int | None) -> None:
    if value is None or expected is None:
        return
    if len(value) != expected:
        raise IdentityError(
            f"{name} must be {expected} bytes for the loaded backend "
            f"({crypto.BACKEND_NAME}); got {len(value)}"
        )


@dataclass(frozen=True)
class NodeIdentity:
    private_key: bytes | None = None
    public_key: bytes | None = None

    # ------------------------------------------------------------------ #
    # Construction
    # ------------------------------------------------------------------ #

    def __post_init__(self):
        _check_size("private_key", self.private_key, crypto.SECRET_KEY_SIZE)
        _check_size("public_key", self.public_key, crypto.PUBLIC_KEY_SIZE)
        if self.private_key is not None and self.public_key is None:
            logger.warning(
                "A private key without a public key cannot be used as a full "
                "identity (ML-DSA secret keys do not embed the public key)"
            )

    @classmethod
    def generate(cls) -> "NodeIdentity":
        public_key, private_key = crypto.generate_keypair()
        return cls(private_key=private_key, public_key=public_key)

    @classmethod
    def from_env(cls, env=None) -> "NodeIdentity | None":
        env = env or os.environ
        private_hex = (env.get("PRIVATE_KEY") or "").strip()
        public_hex = (env.get("PUBLIC_KEY") or "").strip()
        if not private_hex and not public_hex:
            return None
        try:
            private_key = bytes.fromhex(private_hex) if private_hex else None
            public_key = bytes.fromhex(public_hex) if public_hex else None
        except ValueError as exc:
            raise IdentityError("PRIVATE_KEY/PUBLIC_KEY must be hex encoded") from exc
        logger.warning(
            "Loading keys from environment variables; prefer SAN_KEY_FILE "
            "with 0600 permissions for production"
        )
        return cls(private_key=private_key, public_key=public_key)

    @classmethod
    def from_file(cls, path: str) -> "NodeIdentity":
        try:
            mode = os.stat(path).st_mode
        except OSError as exc:
            raise IdentityError(f"Cannot read key file {path}: {exc}") from exc

        if mode & (stat.S_IRGRP | stat.S_IROTH):
            logger.warning(
                "Key file %s is readable by group/others; run: chmod 600 %s",
                path,
                path,
            )

        try:
            with open(path, "r", encoding="utf-8") as handle:
                payload = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise IdentityError(f"Invalid key file {path}: {exc}") from exc

        if not isinstance(payload, dict):
            raise IdentityError(f"Invalid key file {path}: expected a JSON object")

        try:
            private_key = bytes.fromhex(payload["private_key"]) if payload.get("private_key") else None
            public_key = bytes.fromhex(payload["public_key"]) if payload.get("public_key") else None
        except ValueError as exc:
            raise IdentityError(f"Invalid key file {path}: keys must be hex") from exc

        if private_key is None and public_key is None:
            raise IdentityError(f"Key file {path} contains no keys")
        return cls(private_key=private_key, public_key=public_key)

    @classmethod
    def load(cls, key_file: str | None = None, *, allow_generate: bool = True) -> "NodeIdentity":
        """Resolve the node identity with a well-defined precedence."""
        if key_file:
            if os.path.exists(key_file):
                return cls.from_file(key_file)
            if not allow_generate:
                raise IdentityError(f"Key file {key_file} does not exist")
            identity = cls.generate()
            identity.save(key_file)
            logger.warning(
                "Key file %s was missing; generated and saved a new identity %s",
                key_file,
                identity.public_key_hex,
            )
            return identity

        env_identity = cls.from_env()
        if env_identity is not None:
            return env_identity

        if not allow_generate:
            raise IdentityError(
                "No node identity configured (set SAN_KEY_FILE or PRIVATE_KEY/PUBLIC_KEY)"
            )

        identity = cls.generate()
        logger.warning(
            "No node key configured; generated an ephemeral identity %s. "
            "Blocks signed by it stay valid, but the address changes on restart. "
            "Use 'python -m blockchain.identity' for a persistent key.",
            identity.public_key_hex,
        )
        return identity

    # ------------------------------------------------------------------ #
    # Persistence
    # ------------------------------------------------------------------ #

    def save(self, path: str) -> None:
        payload = {
            "private_key": self.private_key.hex() if self.private_key else None,
            "public_key": self.public_key.hex() if self.public_key else None,
            "algorithm": crypto.BACKEND_NAME,
        }
        directory = os.path.dirname(os.path.abspath(path))
        os.makedirs(directory, exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2)
        os.chmod(path, 0o600)
        logger.info("Key file written to %s (0600)", path)

    # ------------------------------------------------------------------ #
    # Usage
    # ------------------------------------------------------------------ #

    @property
    def public_key_hex(self) -> str | None:
        return self.public_key.hex() if self.public_key else None

    @property
    def can_sign(self) -> bool:
        return self.private_key is not None

    def sign(self, message: bytes) -> bytes | None:
        if self.private_key is None:
            return None
        return crypto.sign(message, self.private_key)

    def sign_hex(self, message: bytes) -> str | None:
        signature = self.sign(message)
        return signature.hex() if signature is not None else None

    @staticmethod
    def verify(message: bytes, signature_hex: str | None, public_key_hex: str | None) -> bool:
        if not signature_hex or not public_key_hex:
            return False
        try:
            signature = bytes.fromhex(signature_hex)
            public_key = bytes.fromhex(public_key_hex)
        except ValueError:
            return False
        return crypto.verify(message, signature, public_key)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="Generate a SAN node key file")
    parser.add_argument("-o", "--output", default="san_key.json", help="output file path")
    parser.add_argument("--force", action="store_true", help="overwrite an existing file")
    parser.add_argument("--public-only", action="store_true", help="do not write the private key")
    args = parser.parse_args(argv)

    if os.path.exists(args.output) and not args.force:
        parser.error(f"{args.output} already exists (use --force to overwrite)")

    identity = NodeIdentity.generate()
    if args.public_only:
        logger.warning("--public-only writes a file that cannot sign blocks")
    identity.save(args.output)
    print(f"public key: {identity.public_key_hex}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
