import hashlib
import json
import time

from blockchain.merkle import EMPTY_ROOT, merkle_root

# Bump when the serialized block/transaction shape changes.
# 2: chain_id became part of the hashed block header.
# 3: tx_root and state_root became part of the hashed block header.
# 4: round (proposer fallback) became part of the hashed block header.
# 5: reward_address became part of the hashed block header, so the proposer's
#    reward destination is consensus-visible instead of node-local config.
SCHEMA_VERSION = 5


class Block:
    def __init__(self, index, previous_block_hash, validator, validator_signature,
                 transactions, timestamp=None, chain_id=None, state_root=None, round=0,
                 reward_address=None):
        self.chain_id = chain_id
        self.index = index  # Block index
        self.previous_block_hash = previous_block_hash  # Previous block hash
        # Timestamp must exist before hashing; it is part of the block header.
        self.timestamp = timestamp if timestamp is not None else time.time()
        self.validator = validator  # validator public key (hex)
        self.validator_signature = validator_signature
        self.transactions = transactions  # Transactions
        # tx_root commits to the exact transaction set; state_root commits to
        # the ledger state produced by this block (set by the proposer).
        self.tx_root = merkle_root(
            [self._transaction_to_dict(tx) for tx in self.transactions]
        ) if self.transactions else EMPTY_ROOT
        self.state_root = state_root
        # Proposer round: a height can be retried with the next proposer when
        # the current one is offline (see Node.expected_proposer).
        self.round = int(round or 0)
        # Where this block's tips and subsidy go. None keeps the historical
        # behaviour (the proposer's derived address).
        self.reward_address = reward_address
        self.current_block_hash = self.calculate_hash()  # Current block hash

    @staticmethod
    def _transaction_to_dict(transaction):
        """Normalize a transaction so hashing and serialization are deterministic.

        Blocks carry plain JSON dicts (what nodes exchange over the network).
        Legacy ``Transaction`` objects are accepted for backwards compatibility.
        """
        if isinstance(transaction, dict):
            return transaction
        if hasattr(transaction, "to_dict"):
            return transaction.to_dict()
        return {"data": str(transaction)}

    def header_dict(self):
        return {
            "chain_id": self.chain_id,
            "index": self.index,
            "previous_block_hash": self.previous_block_hash,
            "timestamp": self.timestamp,
            "validator": self.validator,
            "transactions": [self._transaction_to_dict(tx) for tx in self.transactions],
            "tx_root": self.tx_root,
            "state_root": self.state_root,
            "round": self.round,
            "reward_address": self.reward_address,
        }

    def calculate_hash(self):
        """Deterministic SHA3-256 hash over the canonical block header.

        ``sort_keys`` + compact separators guarantee that every node computes
        the exact same hash for the same block data, regardless of dict
        insertion order or platform. ``chain_id`` is part of the header, so
        blocks (and their signatures) can never be replayed on another chain.
        """
        block_data = json.dumps(
            self.header_dict(), sort_keys=True, separators=(",", ":")
        )
        return hashlib.sha3_256(block_data.encode("utf-8")).hexdigest()

    def to_dict(self):
        return {
            **self.header_dict(),
            "validator_signature": self.validator_signature,
            "current_block_hash": self.current_block_hash,
            "version": SCHEMA_VERSION,
        }

    def to_header_dict(self) -> dict:
        """Light-client header: everything the block hash commits to."""
        return {
            "version": SCHEMA_VERSION,
            "chain_id": self.chain_id,
            "index": self.index,
            "previous_block_hash": self.previous_block_hash,
            "timestamp": self.timestamp,
            "validator": self.validator,
            "validator_signature": self.validator_signature,
            "tx_root": self.tx_root,
            "state_root": self.state_root,
            "round": self.round,
            "reward_address": self.reward_address,
            "current_block_hash": self.current_block_hash,
        }

    @classmethod
    def from_dict(cls, data):
        version = data.get("version")
        if version is not None:
            try:
                version = int(version)
            except (TypeError, ValueError) as exc:
                raise ValueError(f"Invalid block schema version: {data.get('version')!r}") from exc
            if version > SCHEMA_VERSION:
                raise ValueError(
                    f"Unsupported block schema version {version} (this node supports {SCHEMA_VERSION})"
                )

        block = cls(
            index=data["index"],
            previous_block_hash=data["previous_block_hash"],
            validator=data.get("validator"),
            validator_signature=data.get("validator_signature"),
            transactions=data.get("transactions") or [],
            timestamp=data["timestamp"],
            chain_id=data.get("chain_id"),
            state_root=data.get("state_root"),
            round=data.get("round", 0),
            reward_address=data.get("reward_address"),
        )
        announced_tx_root = data.get("tx_root")
        if announced_tx_root is not None and announced_tx_root != block.tx_root:
            raise ValueError(
                f"Block {block.index}: tx_root mismatch "
                f"(announced {announced_tx_root}, computed {block.tx_root})"
            )
        expected_hash = data.get("current_block_hash")
        if expected_hash is not None and expected_hash != block.current_block_hash:
            raise ValueError(
                f"Block {block.index}: hash mismatch "
                f"(announced {expected_hash}, computed {block.current_block_hash})"
            )
        return block
