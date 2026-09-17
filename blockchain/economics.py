"""SAN money model and deterministic fee calculation.

All amounts are kept as integer base units (like wei) so that balances can
never drift because of floating point rounding. ``SAN_BASE`` is 10^8, i.e.
1 SAN = 100_000_000 units.

The fee rate depends only on data every node can see (the transaction count of
the current chain tip), never on a node's local mempool size, so all nodes
compute the exact same fee for the same block.
"""

from __future__ import annotations

from decimal import Decimal, InvalidOperation

SAN_DECIMALS = 8
SAN_BASE = 10 ** SAN_DECIMALS

MIN_FEE_PER_BYTE = SAN_BASE // 100  # 0.01 SAN per byte
MAX_FEE_PER_BYTE = SAN_BASE // 10   # 0.1 SAN per byte
CONGESTION_TX_PER_STEP = 100
CONGESTION_MAX_STEPS = 10

# Gas: computation is priced separately from serialized size.
MIN_GAS_PRICE = 1                     # base units per gas unit
DEFAULT_BLOCK_GAS_LIMIT = 30_000_000  # gas units per block

# Stake: a validator must bond at least this much to be in the active set.
MIN_VALIDATOR_STAKE_UNITS = 1000 * SAN_BASE
# Unbonding delay before the stake can be withdrawn (blocks).
DEFAULT_UNBONDING_PERIOD = 100
# Slashing: share of the stake burned on proven equivocation (basis points).
DEFAULT_SLASH_BPS = 5000

# Finality: a block is finalized with 2/3 of the active stake voting for it.
FINALITY_NUMERATOR = 2
FINALITY_DENOMINATOR = 3

# Fee market (EIP-1559 style): a per-gas base fee is burned and adjusts with
# how full blocks are; the rest of the gas price is the validator's tip.
INITIAL_BASE_FEE = 1
BASE_FEE_MAX_CHANGE_DENOMINATOR = 8   # at most +/-12.5% per block
BASE_FEE_TARGET_DIVISOR = 2           # target = block gas limit / 2


def next_base_fee(current_base_fee: int, gas_used: int, gas_limit: int) -> int:
    """Deterministic base-fee adjustment for the next block."""
    current = max(int(current_base_fee), INITIAL_BASE_FEE)
    target = max(int(gas_limit) // BASE_FEE_TARGET_DIVISOR, 1)
    used = max(int(gas_used), 0)

    if used == target:
        return current

    change = current * abs(used - target) // target // BASE_FEE_MAX_CHANGE_DENOMINATOR
    change = max(change, 1)
    if used > target:
        return current + change
    return max(current - change, INITIAL_BASE_FEE)


def san_to_units(amount) -> int:
    """Convert a SAN amount (int/float/str/Decimal) into integer base units."""
    if isinstance(amount, bool) or amount is None:
        raise ValueError(f"Invalid SAN amount: {amount!r}")

    try:
        value = Decimal(str(amount))
    except (InvalidOperation, ValueError) as exc:
        raise ValueError(f"Invalid SAN amount: {amount!r}") from exc

    if not value.is_finite():
        raise ValueError(f"Invalid SAN amount: {amount!r}")

    scaled = value * SAN_BASE
    units = int(scaled)
    if scaled != units:
        raise ValueError(f"Amount has more than {SAN_DECIMALS} decimals: {amount!r}")
    return units


def units_to_san(units: int) -> Decimal:
    return Decimal(units) / SAN_BASE


def fee_rate_for_transaction_count(transaction_count: int) -> int:
    """Fee per byte (in base units) for a block whose parent has N transactions."""
    count = max(int(transaction_count), 0)
    steps = min(count // CONGESTION_TX_PER_STEP, CONGESTION_MAX_STEPS)
    return min(MIN_FEE_PER_BYTE * (1 + steps), MAX_FEE_PER_BYTE)
