"""Minimal SAN Network client SDK.

Wraps the REST API for wallets and tools: queries, signed transaction
submission with automatic nonce/base-fee handling, contract deployment and
read-only calls, Merkle proof verification and staking/governance helpers.

Example::

    from blockchain.identity import NodeIdentity  # noqa: F401
    from sdk import SanClient

    client = SanClient("http://127.0.0.1:8000", NodeIdentity.from_file("san_key.json"))
    print(client.transfer("0x" + "ab" * 20, 5))
"""

from __future__ import annotations

from typing import Any

import requests

from blockchain.address import address_from_public_key, normalize_address
from blockchain.identity import NodeIdentity  # noqa: F401
from blockchain.merkle import verify_merkle_proof
from blockchain.Transaction import Transaction
from utils import canonical


class SanClientError(Exception):
    """Raised when the node rejects a request."""


class SanClient:
    def __init__(
        self,
        base_url: str = "http://127.0.0.1:8000",
        identity: NodeIdentity | None = None,
        timeout: float = 10.0,
    ):
        self.base_url = base_url.rstrip("/")
        # Read-only usage (queries, proofs) needs no identity at all.
        self.identity = identity
        self.timeout = timeout
        self._chain_id: str | None = None

    # ------------------------------------------------------------------ #
    # Transport
    # ------------------------------------------------------------------ #

    def _get(self, path: str, **params) -> Any:
        response = requests.get(
            f"{self.base_url}{path}", params=params or None, timeout=self.timeout
        )
        if response.status_code >= 400:
            raise SanClientError(f"GET {path} failed: {response.text}")
        return response.json()

    def _post(self, path: str, payload: dict) -> Any:
        response = requests.post(f"{self.base_url}{path}", json=payload, timeout=self.timeout)
        try:
            body = response.json()
        except ValueError:
            body = {"raw": response.text}
        if response.status_code >= 400:
            raise SanClientError(f"POST {path} failed: {body}")
        return body

    # ------------------------------------------------------------------ #
    # Queries
    # ------------------------------------------------------------------ #

    @property
    def chain_id(self) -> str:
        if self._chain_id is None:
            self._chain_id = str(self.health().get("chain_id"))
        return self._chain_id

    def _require_identity(self) -> NodeIdentity:
        if self.identity is None or self.identity.public_key_hex is None:
            raise SanClientError("This client has no signing identity")
        return self.identity

    @property
    def address(self) -> str:
        return address_from_public_key(self._require_identity().public_key_hex)

    def genesis(self) -> dict:
        return self._get("/genesis")

    def health(self) -> dict:
        return self._get("/health")

    def finality(self) -> dict:
        return self._get("/finality")

    def validators(self) -> dict:
        return self._get("/validators")

    def account(self, address: str) -> dict:
        return self._get(f"/account/{normalize_address(address)}")

    def nonce(self, address: str | None = None) -> int:
        return int(self.account(address or self.address).get("nonce", 0))

    def base_fee(self) -> int:
        return int(self.health().get("base_fee", 1) or 1)

    def mempool(self) -> dict:
        return self._get("/mempool")

    def contracts(self) -> list[str]:
        return self._get("/contracts").get("contracts", [])

    def contract_query(self, contract_id: str, function_name: str, params: list | None = None):
        body = self._post(
            "/contract/query",
            {"contract_id": contract_id, "function_name": function_name, "params": params or []},
        )
        return body.get("result")

    def receipt(self, block_index: int, tx_index: int) -> dict:
        return self._get(f"/receipt/{block_index}/{tx_index}")

    def transaction(self, tx_id: str) -> dict:
        """Transaction, its block and its receipt, via the tx index."""
        return self._get(f"/tx/{tx_id}")

    def receipt_for_tx(self, tx_id: str) -> dict:
        return self._get(f"/receipt/tx/{tx_id}")

    def metrics_text(self) -> str:
        response = requests.get(f"{self.base_url}/metrics", timeout=self.timeout)
        response.raise_for_status()
        return response.text

    def account_proof(self, address: str | None = None) -> dict:
        return self._get(f"/proof/account/{normalize_address(address or self.address)}")

    @staticmethod
    def verify_account_proof(proof: dict, expected_root: str | None = None) -> bool:
        """Verify a Merkle proof locally (optionally against a known root)."""
        if expected_root is not None and proof.get("root") != expected_root:
            return False
        return verify_merkle_proof(
            proof["root"], proof["leaf"], proof["proof"], int(proof["index"])
        )

    # ------------------------------------------------------------------ #
    # Transactions
    # ------------------------------------------------------------------ #

    def _sign(self, payload: dict) -> dict:
        if self.identity.private_key is None:
            raise SanClientError("This identity has no private key to sign with")
        payload["signature"] = Transaction.sign_payload(payload, self.identity.private_key)
        return payload

    def send(self, *, nonce: int | None = None, **fields) -> dict:
        """Sign and submit a transaction; nonce is filled in automatically."""
        identity = self._require_identity()
        if nonce is None:
            nonce = self.nonce()
        payload = {
            "chain_id": self.chain_id,
            "sender": identity.public_key_hex,
            "nonce": int(nonce),
        }
        payload.update(fields)
        return self._post("/transaction", self._sign(payload))

    def transfer(self, to: str, value_san, nonce: int | None = None) -> dict:
        return self.send(receiver=normalize_address(to), value=value_san, nonce=nonce)

    def deposit_stake(self, amount_san, nonce: int | None = None) -> dict:
        from blockchain.economics import san_to_units

        return self.send(
            nonce=nonce,
            validator={"command": "deposit", "amount": san_to_units(amount_san)},
        )

    def undelegate(self, nonce: int | None = None) -> dict:
        return self.send(nonce=nonce, validator={"command": "undelegate"})

    def withdraw_stake(self, nonce: int | None = None) -> dict:
        return self.send(nonce=nonce, validator={"command": "withdraw"})

    def deploy_contract(
        self,
        contract_id: str,
        source: str,
        gas_limit: int = 2_000_000,
        gas_price: int | None = None,
        nonce: int | None = None,
    ) -> dict:
        return self.send(
            nonce=nonce,
            gas_limit=gas_limit,
            gas_price=self.base_fee() if gas_price is None else gas_price,
            contract_code={
                "command": "deploy",
                "contract_id": contract_id,
                "pena_code": source,
            },
        )

    def call_contract(
        self,
        contract_id: str,
        function_name: str,
        params: list | None = None,
        gas_limit: int = 1_000_000,
        gas_price: int | None = None,
        nonce: int | None = None,
    ) -> dict:
        return self.send(
            nonce=nonce,
            gas_limit=gas_limit,
            gas_price=self.base_fee() if gas_price is None else gas_price,
            contract_code={
                "command": "run",
                "contract_id": contract_id,
                "function_name": function_name,
                "params": params or [],
            },
        )

    # ------------------------------------------------------------------ #
    # Governance
    # ------------------------------------------------------------------ #

    def governance_approval(
        self, name: str, value: int, nonce: int | None = None
    ) -> dict:
        """Sign an approval for a parameter change (bound to sender+nonce)."""
        identity = self._require_identity()
        if nonce is None:
            nonce = self.nonce()
        message = canonical.dumps_bytes(
            {
                "chain_id": self.chain_id,
                "command": "set_param",
                "name": name,
                "value": int(value),
                "tx_sender": identity.public_key_hex,
                "tx_nonce": int(nonce),
            }
        )
        signature = identity.sign_hex(message)
        if signature is None:
            raise SanClientError("This identity has no private key to sign with")
        return {"public_key": self.identity.public_key_hex, "signature": signature}

    def governance_set_param(
        self,
        name: str,
        value: int,
        approvals: list[dict],
        nonce: int | None = None,
    ) -> dict:
        return self.send(
            nonce=nonce,
            governance={
                "command": "set_param",
                "name": name,
                "value": int(value),
                "approvals": approvals,
            },
        )
