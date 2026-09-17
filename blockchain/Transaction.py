import hashlib
import json
import logging

from blockchain import crypto
from blockchain.address import try_address_from_public_key
from blockchain.economics import MIN_FEE_PER_BYTE, MIN_GAS_PRICE, san_to_units
from utils import canonical

logger = logging.getLogger(__name__)

# Single canonical JSON representation used for signing, verification and
# block hashing. Every component must use these exact settings.


class Transaction:

    # Fields that are protocol metadata and are never part of a user signature.
    META_FIELDS = ("signature", "fee")

    # Chain binding: the payload must name the chain it is valid on, and the
    # node rejects any transaction whose id does not match its own.
    CHAIN_ID_FIELD = "chain_id"

    # Payload keys that trigger code execution (and therefore need gas).
    EXECUTION_FIELDS = ("bytecode", "contract_code")

    # System commands for the validator registry (staking).
    VALIDATOR_COMMANDS = ("deposit", "undelegate", "withdraw", "evidence")

    # On-chain parameter changes need a 2/3 validator approval set.
    GOVERNANCE_COMMANDS = ("set_param",)

    def __init__(self, data, fee_rate=None):
        self.payload = self._normalize_payload(data)
        # The client cannot pick its own fee; the protocol recomputes it.
        self.payload.pop("fee", None)

        if not self.chain_id_of(self.payload):
            raise ValueError(
                f"Transaction payload must include a non-empty '{self.CHAIN_ID_FIELD}'"
            )
        error = self.validate_gas_fields(self.payload)
        if error:
            raise ValueError(error)
        error = self.validate_validator_command(self.payload)
        if error:
            raise ValueError(error)
        error = self.validate_governance_command(self.payload)
        if error:
            raise ValueError(error)
        error = self.validate_contract_code(self.payload)
        if error:
            raise ValueError(error)

        rate = MIN_FEE_PER_BYTE if fee_rate is None else int(fee_rate)
        self.fee = self.expected_fee(self.payload, rate)
        self.payload["fee"] = self.fee
        self.data = canonical.dumps_bytes(self.payload)

    @staticmethod
    def validator_command_of(payload: dict):
        command = payload.get("validator")
        return command if isinstance(command, dict) else None

    @classmethod
    def validate_validator_command(cls, payload: dict) -> str | None:
        """Structural rules for staking/slashing system transactions."""
        command = payload.get("validator")
        if command is None:
            return None
        if not isinstance(command, dict):
            return "'validator' must be an object"

        name = command.get("command")
        if name not in cls.VALIDATOR_COMMANDS:
            return f"unknown validator command: {name!r}"
        if cls.has_execution(payload):
            return "validator commands must not contain execution payloads"
        if "value" in payload or "receiver" in payload:
            return "validator commands must not be transfers"

        if name == "deposit":
            amount = command.get("amount")
            if isinstance(amount, bool) or not isinstance(amount, int) or amount <= 0:
                return "validator deposit requires a positive integer 'amount'"

        if name == "evidence":
            for field in ("vote_a", "vote_b"):
                vote = command.get(field)
                if not isinstance(vote, dict):
                    return f"evidence requires a '{field}' vote object"
                if not vote.get("public_key") or not vote.get("signature"):
                    return f"evidence '{field}' must be a signed vote"
        return None

    @staticmethod
    def validate_contract_code(payload: dict) -> str | None:
        """Structural rules for deploy/run payloads (audit fix: a non-string
        contract id used to crash execution and poison the mempool)."""
        code = payload.get("contract_code")
        if code is None:
            return None
        if not isinstance(code, dict):
            return "'contract_code' must be an object"

        command = code.get("command")
        if command not in ("deploy", "run"):
            return f"unknown contract command: {command!r}"

        contract_id = code.get("contract_id")
        if not isinstance(contract_id, str) or not contract_id:
            return "contract requires a non-empty string 'contract_id'"

        if command == "deploy":
            if not isinstance(code.get("pena_code"), str) and not isinstance(
                code.get("bytecode"), list
            ):
                return "deploy requires 'pena_code' or 'bytecode'"
            return None

        if not isinstance(code.get("function_name"), str) or not code["function_name"]:
            return "run requires a non-empty 'function_name'"
        if not isinstance(code.get("params", []), list):
            return "'params' must be a list"
        return None

    @staticmethod
    def governance_command_of(payload: dict):
        command = payload.get("governance")
        return command if isinstance(command, dict) else None

    @classmethod
    def validate_governance_command(cls, payload: dict) -> str | None:
        """Structural rules for on-chain parameter changes."""
        command = payload.get("governance")
        if command is None:
            return None
        if not isinstance(command, dict):
            return "'governance' must be an object"

        name = command.get("command")
        if name not in cls.GOVERNANCE_COMMANDS:
            return f"unknown governance command: {name!r}"
        if cls.has_execution(payload):
            return "governance commands must not contain execution payloads"
        if "value" in payload or "receiver" in payload:
            return "governance commands must not be transfers"
        if payload.get("validator") is not None:
            return "governance and validator commands cannot be combined"

        if not isinstance(command.get("name"), str) or not command.get("name"):
            return "governance set_param requires a 'name'"
        value = command.get("value")
        if isinstance(value, bool) or not isinstance(value, int):
            return "governance set_param requires an integer 'value'"

        approvals = command.get("approvals")
        if not isinstance(approvals, list) or not approvals:
            return "governance set_param requires at least one approval"
        for approval in approvals:
            if not isinstance(approval, dict):
                return "governance approvals must be objects"
            if not approval.get("public_key") or not approval.get("signature"):
                return "governance approvals must be signed by validators"
        return None

    @staticmethod
    def chain_id_of(payload: dict):
        return payload.get(Transaction.CHAIN_ID_FIELD)

    @staticmethod
    def has_execution(payload: dict) -> bool:
        return any(field in payload for field in Transaction.EXECUTION_FIELDS)

    @staticmethod
    def _int_field(payload: dict, name: str) -> int:
        value = payload.get(name, 0)
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            raise ValueError(f"'{name}' must be a non-negative integer")
        return value

    @staticmethod
    def gas_limit_of(payload: dict) -> int:
        return Transaction._int_field(payload, "gas_limit")

    @staticmethod
    def gas_price_of(payload: dict) -> int:
        return Transaction._int_field(payload, "gas_price")

    @classmethod
    def validate_gas_fields(cls, payload: dict) -> str | None:
        """Gas rules: execution pays for computation, plain transfers do not."""
        try:
            gas_limit = cls._int_field(payload, "gas_limit")
            gas_price = cls._int_field(payload, "gas_price")
        except ValueError as exc:
            return str(exc)

        if cls.has_execution(payload):
            if gas_limit < 1:
                return "execution transactions require 'gas_limit' >= 1"
            if gas_price < MIN_GAS_PRICE:
                return f"'gas_price' must be at least {MIN_GAS_PRICE}"
        else:
            if gas_limit != 0 or gas_price != 0:
                return "transactions without execution must not set gas fields"
        return None

    @staticmethod
    def _normalize_payload(data) -> dict:
        """Accept a dict, a JSON string or JSON bytes and return a dict."""
        if isinstance(data, dict):
            return dict(data)
        if isinstance(data, (bytes, bytearray)):
            data = bytes(data).decode("utf-8")
        if isinstance(data, str):
            decoded = json.loads(data)
            if not isinstance(decoded, dict):
                raise ValueError("Transaction payload must be a JSON object")
            return decoded
        raise TypeError(f"Unsupported transaction payload type: {type(data)!r}")

    def to_dict(self) -> dict:
        return dict(self.payload)

    # ------------------------------------------------------------------ #
    # Accessors used by the node for validation
    # ------------------------------------------------------------------ #

    @property
    def sender(self):
        return self.payload.get("sender")

    @property
    def sender_address(self):
        sender = self.sender
        if not isinstance(sender, str) or not sender:
            return None
        return try_address_from_public_key(sender)

    @property
    def nonce(self):
        return self.payload.get("nonce")

    @property
    def tx_id(self) -> str:
        """Replay-protection id: hash of the signed part of the payload."""
        return hashlib.sha256(self.serialize_message(self.payload)).hexdigest()

    def value_units(self) -> int:
        if "value" not in self.payload:
            return 0
        return san_to_units(self.payload["value"])

    # ------------------------------------------------------------------ #
    # Canonical serialization
    # ------------------------------------------------------------------ #

    @staticmethod
    def serialize_message(message: dict) -> bytes:
        """
        Serializes the message in canonical JSON format.

        ``signature`` and ``fee`` are protocol metadata: they are excluded so
        that signing and verification always reconstruct the exact same bytes.
        """
        message_to_sign = {
            key: value
            for key, value in message.items()
            if key not in Transaction.META_FIELDS
        }
        return canonical.dumps_bytes(message_to_sign)

    @staticmethod
    def _fee_basis(payload: dict) -> bytes:
        """Canonical bytes the protocol fee is metered on.

        Includes the signature (the real serialized size of a transaction) but
        excludes the protocol-set ``fee`` field, so every node can recompute the
        exact same fee.
        """
        basis = {key: value for key, value in payload.items() if key != "fee"}
        return canonical.dumps_bytes(basis)

    @staticmethod
    def expected_fee(payload: dict, fee_rate: int) -> int:
        """Deterministic protocol fee: size fee plus maximum gas cost.

        The full gas escrow (``gas_limit * gas_price``) is charged up front and
        the unused part is refunded after execution, so every node can verify
        the fee without running the code first.
        """
        size_fee = len(Transaction._fee_basis(payload)) * int(fee_rate)
        try:
            gas_limit = Transaction._int_field(payload, "gas_limit")
            gas_price = Transaction._int_field(payload, "gas_price")
        except ValueError:
            gas_limit = 0
            gas_price = 0
        return size_fee + gas_limit * gas_price

    @staticmethod
    def sign_payload(payload: dict, private_key: bytes) -> str:
        """Sign a transaction payload and return the hex signature.

        Convenience helper so clients and tests use exactly the same canonical
        representation as the verifier.
        """
        return crypto.sign(Transaction.serialize_message(payload), private_key).hex()

    # ------------------------------------------------------------------ #
    # Verification
    # ------------------------------------------------------------------ #

    @staticmethod
    def verify_transaction(tx_bytes) -> bool:
        """
        Verifies a transaction signature with the post-quantum backend.

        The transaction data must contain "sender" (the public key in hex) and
        "signature" (hex). Returns True only when the signature is valid;
        malformed or unsigned data returns False instead of raising, so a single
        bad transaction can never crash a block validator.
        """
        try:
            if isinstance(tx_bytes, (bytes, bytearray)):
                tx = json.loads(bytes(tx_bytes).decode("utf-8"))
            elif isinstance(tx_bytes, str):
                tx = json.loads(tx_bytes)
            elif isinstance(tx_bytes, dict):
                tx = tx_bytes
            else:
                return False
        except Exception as exc:  # noqa: BLE001 - never trust network input
            logger.warning("Transaction decoding failed: %s", exc)
            return False

        if not isinstance(tx, dict) or "sender" not in tx or "signature" not in tx:
            logger.warning("Transaction is missing 'sender' or 'signature'")
            return False

        try:
            public_key = bytes.fromhex(tx["sender"])
            signature = bytes.fromhex(tx["signature"])
        except Exception as exc:  # noqa: BLE001
            logger.warning("Public key/signature conversion failed: %s", exc)
            return False

        message_bytes = Transaction.serialize_message(tx)
        return crypto.verify(message_bytes, signature, public_key)
