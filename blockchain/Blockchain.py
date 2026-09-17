import statistics

from blockchain.Block import Block
from blockchain.economics import (
    DEFAULT_SLASH_BPS,
    DEFAULT_UNBONDING_PERIOD,
    INITIAL_BASE_FEE,
    MIN_VALIDATOR_STAKE_UNITS,
    fee_rate_for_transaction_count,
)
from blockchain.merkle import state_root as compute_state_root

# Every node must produce the exact same genesis block, otherwise chains can
# never be synced or compared. Fixed timestamp keeps the genesis hash stable.
GENESIS_TIMESTAMP = 0.0
GENESIS_MESSAGE = "TEXT A MESSAGE TO THE HUMANITY"
DEFAULT_CHAIN_ID = "san-devnet-1"
DEFAULT_PROPOSER_TIMEOUT_MS = 6_000

# Parameters that validator-majority governance may change, with (min, max).
GOVERNANCE_PARAMETERS = {
    "min_validator_stake": (0, None),
    "unbonding_period": (0, None),
    "slash_bps": (0, 10_000),
    "block_gas_limit": (100_000, None),
    # Round deadline in milliseconds: part of consensus state so every node
    # validates proposer rounds with the same timeout (audit fix).
    "proposer_timeout_ms": (100, 60_000),
    # Inflationary block subsidy in base units; 0 keeps the chain fee-only.
    "block_reward": (0, None),
    # Minimum time between blocks in milliseconds (audit fix: a reward chain
    # must not allow subsidy bursts; 0 disables the rule for fee-only chains).
    "min_block_interval_ms": (0, 600_000),
}


def governance_value_ok(name: str, value) -> bool:
    if name not in GOVERNANCE_PARAMETERS:
        return False
    if isinstance(value, bool) or not isinstance(value, int):
        return False
    minimum, maximum = GOVERNANCE_PARAMETERS[name]
    if value < minimum:
        return False
    return maximum is None or value <= maximum


class Blockchain:
    def __init__(
        self,
        genesis_balances=None,
        chain_id=DEFAULT_CHAIN_ID,
        min_validator_stake=MIN_VALIDATOR_STAKE_UNITS,
        unbonding_period=DEFAULT_UNBONDING_PERIOD,
        slash_bps=DEFAULT_SLASH_BPS,
        block_gas_limit=30_000_000,
        proposer_timeout_ms=DEFAULT_PROPOSER_TIMEOUT_MS,
        block_reward=0,
        min_block_interval_ms=0,
    ):
        self.chain_id = chain_id
        self.chain = []
        # Balances are integer base units, keyed by account address.
        self.SAN = {address: int(units) for address, units in (genesis_balances or {}).items()}
        # Immutable copy of the genesis allocation (public chain metadata).
        self.genesis_allocations = dict(self.SAN)
        self.nonces = {}
        # Validators: address -> {"public_key", "stake", "joined_height",
        # "release_height"}
        self.validators: dict[str, dict] = {}
        self.total_slashed = 0
        self.total_burned = 0
        self.base_fee = INITIAL_BASE_FEE
        # Consensus parameters live in the chain state so governance can
        # change them; the config only provides the genesis values.
        self.parameters = {
            "min_validator_stake": int(min_validator_stake),
            "unbonding_period": int(unbonding_period),
            "slash_bps": int(slash_bps),
            "block_gas_limit": int(block_gas_limit),
            "proposer_timeout_ms": int(proposer_timeout_ms),
            "block_reward": int(block_reward),
            "min_block_interval_ms": int(min_block_interval_ms),
        }
        # Immutable snapshot of the genesis parameters (what /genesis serves).
        self.genesis_parameters = dict(self.parameters)
        self.genesis_state_root = compute_state_root(
            self.SAN, {}, {}, 0, {}, self.parameters, self.base_fee, self.total_burned
        )
        self._create_genesis_block()

    @property
    def min_validator_stake(self) -> int:
        return int(self.parameters.get("min_validator_stake", 0))

    @property
    def unbonding_period(self) -> int:
        return int(self.parameters.get("unbonding_period", 0))

    @property
    def slash_bps(self) -> int:
        return int(self.parameters.get("slash_bps", 0))

    @property
    def block_gas_limit(self) -> int:
        return int(self.parameters.get("block_gas_limit", 0))

    @property
    def min_block_interval(self) -> float:
        """Consensus minimum seconds between blocks.

        A subsidy chain always enforces at least one second, so a proposer
        cannot mint a burst of reward blocks; governance may raise it.
        """
        configured = int(self.parameters.get("min_block_interval_ms", 0)) / 1000.0
        if self.block_reward > 0:
            return max(configured, 1.0)
        return configured

    @property
    def block_reward(self) -> int:
        """Per-block subsidy in base units, read from consensus state."""
        return int(self.parameters.get("block_reward", 0))

    @property
    def proposer_timeout(self) -> float:
        """Round deadline in seconds, read from consensus state."""
        return int(self.parameters.get("proposer_timeout_ms", DEFAULT_PROPOSER_TIMEOUT_MS)) / 1000.0

    def active_validators(self) -> dict[str, int]:
        """Staked validators eligible to propose/vote at the current tip."""
        tip_index = self.tip.index
        active: dict[str, int] = {}
        for address, info in self.validators.items():
            if info.get("release_height") is not None:
                continue
            if int(info.get("joined_height", 0)) > tip_index:
                continue
            stake = int(info.get("stake", 0))
            if stake >= self.min_validator_stake:
                active[address] = stake
        return active

    def total_active_stake(self) -> int:
        return sum(self.active_validators().values())

    def active_validators_for(self, height: int) -> dict[str, int]:
        """Active set used to weight votes for the block at ``height``.

        A validator can only vote for blocks produced after it joined, so the
        eligibility cut-off is the parent height. Release timing is ignored:
        votes are tallied within a block or two of the target height.
        """
        cutoff = max(height - 1, 0)
        active: dict[str, int] = {}
        for address, info in self.validators.items():
            if int(info.get("joined_height", 0)) > cutoff:
                continue
            stake = int(info.get("stake", 0))
            if stake >= self.min_validator_stake:
                active[address] = stake
        return active

    def _create_genesis_block(self):
        genesis_block = Block(
            index=0,
            previous_block_hash="0",
            validator="GENESIS_VALIDATOR",
            validator_signature="GENESIS_SIGNATURE",
            transactions=[GENESIS_MESSAGE],
            timestamp=GENESIS_TIMESTAMP,
            chain_id=self.chain_id,
            state_root=self.genesis_state_root,
        )

        self.chain.append(genesis_block)

    def add_block(self, validator, validator_signature, transactions):
        last_block = self.chain[-1]

        new_block = Block(
            index=last_block.index + 1,
            previous_block_hash=last_block.current_block_hash,
            validator=validator,
            validator_signature=validator_signature,
            transactions=transactions,
            chain_id=self.chain_id,
        )

        self.chain.append(new_block)

    @property
    def tip(self):
        return self.chain[-1]

    def blocks_since(self, from_index, limit: int | None = None):
        """Return blocks with ``index >= from_index`` (optionally capped).

        The window is bounded so a single sync request cannot materialize a
        whole long chain (audit fix).
        """
        if from_index < 0:
            from_index = 0
        first = self.chain[0].index if self.chain else 0
        offset = max(from_index - first, 0)
        if limit is None:
            return list(self.chain[offset:])
        end = offset + max(int(limit), 0)
        return list(self.chain[offset:end])

    def next_fee_rate(self) -> int:
        """Fee per byte for the next block, derived from the current tip.

        Using on-chain data makes the fee deterministic for every node.
        """
        return fee_rate_for_transaction_count(len(self.tip.transactions))

    def median_time_past(self, window: int = 11) -> float:
        """Median timestamp of the last ``window`` blocks (genesis included)."""
        timestamps = [block.timestamp for block in self.chain[-window:]]
        if not timestamps:
            return 0.0
        return statistics.median(timestamps)

    def state_snapshot(self) -> dict:
        return {
            "balances": dict(self.SAN),
            "nonces": dict(self.nonces),
            "validators": {address: dict(info) for address, info in self.validators.items()},
            "total_slashed": self.total_slashed,
            "total_burned": self.total_burned,
            "base_fee": self.base_fee,
            "parameters": dict(self.parameters),
        }

    def load_state(self, payload) -> None:
        payload = payload or {}
        self.SAN = {
            address: int(units)
            for address, units in (payload.get("balances") or {}).items()
        }
        self.nonces = {
            address: int(nonce) for address, nonce in (payload.get("nonces") or {}).items()
        }
        self.validators = {
            address: dict(info)
            for address, info in (payload.get("validators") or {}).items()
        }
        self.total_slashed = int(payload.get("total_slashed", 0) or 0)
        self.total_burned = int(payload.get("total_burned", 0) or 0)
        self.base_fee = int(payload.get("base_fee", INITIAL_BASE_FEE) or INITIAL_BASE_FEE)
        self.parameters = {
            **self.parameters,
            **{key: int(value) for key, value in (payload.get("parameters") or {}).items()},
        }
        if "proposer_timeout_ms" in self.parameters:
            self.parameters["proposer_timeout_ms"] = int(self.parameters["proposer_timeout_ms"])
