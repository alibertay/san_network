"""Node configuration and peer address helpers.

Every port can be overridden through environment variables so that several
nodes can run on the same machine and so tests can use ephemeral ports::

    SAN_HOST, SAN_API_PORT, SAN_P2P_PORT, SAN_PEER_PORT, SAN_CONTROLLER_PORT,
    SAN_BOOTSTRAP, SAN_CONTROLLER_COUNT, SAN_BLOCK_THRESHOLD_FEE,
    SAN_PEER_CHECK_INTERVAL
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, field

from blockchain.address import (
    AddressError,
    address_from_public_key,
    is_valid_address,
    normalize_address,
)
from blockchain.economics import (
    DEFAULT_SLASH_BPS,
    DEFAULT_UNBONDING_PERIOD,
    MIN_VALIDATOR_STAKE_UNITS,
    san_to_units,
)

logger = logging.getLogger(__name__)

DEFAULT_CHAIN_ID = "san-devnet-1"


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None or not raw.strip():
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


def _normalize_allocation_key(raw: str) -> str:
    """Genesis keys may be addresses or raw public keys (derived to addresses)."""
    if is_valid_address(raw):
        return normalize_address(raw)
    try:
        return address_from_public_key(raw)
    except AddressError as exc:
        raise ValueError(f"invalid address/public key {raw!r}") from exc


def _parse_genesis_allocations(raw: str | None) -> dict[str, int]:
    """Parse ``SAN_GENESIS_ALLOCATION="address:amount,address:amount"``.

    Keys accept either an ``0x`` address or a hex public key (which is hashed
    into its address). Amounts are SAN values converted to integer base units.
    """
    allocations: dict[str, int] = {}
    if not raw:
        return allocations
    for entry in raw.split(","):
        entry = entry.strip()
        if not entry:
            continue
        address, _, amount = entry.partition(":")
        address = address.strip()
        if not address or not amount.strip():
            logger.warning("Ignoring malformed genesis allocation %r", entry)
            continue
        try:
            allocations[_normalize_allocation_key(address)] = san_to_units(amount.strip())
        except ValueError as exc:
            logger.warning("Ignoring genesis allocation %r: %s", entry, exc)
    return allocations


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or not raw.strip():
        return default
    try:
        return int(raw)
    except ValueError:
        logger.warning("Invalid %s=%r, using default %s", name, raw, default)
        return default


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or not raw.strip():
        return default
    try:
        return float(raw)
    except ValueError:
        logger.warning("Invalid %s=%r, using default %s", name, raw, default)
        return default


def _env_str(name: str, default: str | None = None) -> str | None:
    raw = os.getenv(name)
    if raw is None or not raw.strip():
        return default
    return raw.strip()


def _env_san_units(name: str, default_san: float = 0.0) -> int:
    """Read a SAN amount from the environment and convert it to base units."""
    raw = os.getenv(name)
    if raw is None or not raw.strip():
        return san_to_units(default_san)
    try:
        return san_to_units(raw.strip())
    except ValueError as exc:
        logger.warning("Invalid %s=%r: %s", name, raw, exc)
        return san_to_units(default_san)


@dataclass(frozen=True)
class NodeConfig:
    host: str = "0.0.0.0"
    api_port: int = 8000          # FastAPI REST port (/sync, /transaction, ...)
    # Chain identity: signatures are bound to this id, so testnet/mainnet
    # transactions can never be replayed across chains.
    chain_id: str = DEFAULT_CHAIN_ID
    p2p_port: int = 8765          # incoming block broadcast listener
    peer_port: int = 8770         # peer discovery / gossip / PING-PONG
    controller_port: int = 8769   # controller vote listener
    bootstrap: str | None = None  # e.g. "127.0.0.1:6161"
    advertise_host: str | None = None  # host announced to peers (defaults to local IP)
    controller_count: int = 10
    block_threshold_fee: float = 500.0
    block_gas_limit: int = 30_000_000
    # Validator staking and finality
    min_validator_stake: int = MIN_VALIDATOR_STAKE_UNITS
    unbonding_period: int = DEFAULT_UNBONDING_PERIOD
    slash_bps: int = DEFAULT_SLASH_BPS
    peer_check_interval: float = 30.0
    ws_timeout: float = 3.0
    genesis_allocations: dict[str, int] = field(default_factory=dict)
    require_block_signature: bool = True
    # Block rewards: a fixed subsidy minted per block (0 = fee-only chain) and
    # an optional destination address so a node can pay rewards to a wallet.
    block_reward: float = 0.0           # SAN per block (genesis parameter)
    min_block_interval_ms: int = 0      # consensus minimum spacing (ms)
    reward_address: str = ""            # where tips + subsidy are credited
    # Audit fix: every non-genesis block must commit to the post-state, so a
    # proposer cannot skip the state root and dodge state verification.
    require_state_root: bool = True
    # Identity
    key_file: str | None = None
    # Network hardening
    max_peers: int = 64
    ws_max_size: int = 1 << 20          # 1 MiB per WebSocket message
    peer_rate_limit: int = 60           # peer messages per window
    peer_rate_window: float = 10.0
    peer_record_ttl: float = 300.0      # seconds a peer update stays fresh
    peer_miss_threshold: int = 2        # failed health checks before eviction
    # Controller selection
    controller_min_stake: int = 0       # base units required to be a controller
    epoch_length: int = 100             # blocks per controller epoch
    # Proposer fallback: when the expected proposer is offline, the next one
    # takes over round by round (no more stalled heights).
    proposer_timeout: float = 6.0
    max_proposer_rounds: int = 16
    # Fork handling
    max_orphans: int = 64               # buffered fork blocks
    max_reorg_depth: int = 64           # how far back a reorg may go
    block_gossip: bool = True           # re-broadcast accepted blocks
    # TLS (optional; when unset the node runs plain ws/http in development)
    tls_cert: str | None = None
    tls_key: str | None = None
    tls_ca: str | None = None
    # Persistence (namespaced key-value store); unset keeps everything in memory
    db_path: str | None = None
    db_backend: str = "lmdb"           # lmdb | memory
    sync_batch_size: int = 128          # blocks per /sync page
    sync_max_blocks: int = 50_000       # blocks per sync session
    # Snapshots and pruning (archive nodes keep everything)
    snapshot_interval: int = 1000        # blocks between state snapshots
    prune_keep: int = 0                  # >0 prunes DB blocks older than this
    # Public RPC hardening
    rpc_rate_limit: int = 120           # requests per window per client IP
    rpc_rate_window: float = 10.0
    rpc_max_body: int = 1 << 20         # max request body bytes

    @classmethod
    def from_env(cls) -> "NodeConfig":
        return cls(
            host=_env_str("SAN_HOST", "0.0.0.0") or "0.0.0.0",
            chain_id=_env_str("SAN_CHAIN_ID", DEFAULT_CHAIN_ID) or DEFAULT_CHAIN_ID,
            api_port=_env_int("SAN_API_PORT", 8000),
            p2p_port=_env_int("SAN_P2P_PORT", 8765),
            peer_port=_env_int("SAN_PEER_PORT", 8770),
            controller_port=_env_int("SAN_CONTROLLER_PORT", 8769),
            bootstrap=_env_str("SAN_BOOTSTRAP"),
            advertise_host=_env_str("SAN_ADVERTISE_HOST"),
            controller_count=_env_int("SAN_CONTROLLER_COUNT", 10),
            block_threshold_fee=_env_float("SAN_BLOCK_THRESHOLD_FEE", 500.0),
            block_gas_limit=_env_int("SAN_BLOCK_GAS_LIMIT", 30_000_000),
            min_validator_stake=_env_san_units("SAN_MIN_VALIDATOR_STAKE", 1000.0),
            unbonding_period=_env_int("SAN_UNBONDING_PERIOD", DEFAULT_UNBONDING_PERIOD),
            slash_bps=_env_int("SAN_SLASH_BPS", DEFAULT_SLASH_BPS),
            peer_check_interval=_env_float("SAN_PEER_CHECK_INTERVAL", 30.0),
            ws_timeout=_env_float("SAN_WS_TIMEOUT", 3.0),
            genesis_allocations=_parse_genesis_allocations(_env_str("SAN_GENESIS_ALLOCATION")),
            require_block_signature=_env_bool("SAN_REQUIRE_BLOCK_SIGNATURE", True),
            block_reward=_env_float("SAN_BLOCK_REWARD", 0.0),
            min_block_interval_ms=_env_int("SAN_MIN_BLOCK_INTERVAL_MS", 0),
            reward_address=_env_str("SAN_REWARD_ADDRESS") or "",
            require_state_root=_env_bool("SAN_REQUIRE_STATE_ROOT", True),
            key_file=_env_str("SAN_KEY_FILE"),
            max_peers=_env_int("SAN_MAX_PEERS", 64),
            ws_max_size=_env_int("SAN_WS_MAX_SIZE", 1 << 20),
            peer_rate_limit=_env_int("SAN_PEER_RATE_LIMIT", 60),
            peer_rate_window=_env_float("SAN_PEER_RATE_WINDOW", 10.0),
            peer_record_ttl=_env_float("SAN_PEER_TTL", 300.0),
            peer_miss_threshold=_env_int("SAN_PEER_MISS_THRESHOLD", 2),
            controller_min_stake=_env_san_units("SAN_CONTROLLER_MIN_STAKE", 0.0),
            epoch_length=_env_int("SAN_EPOCH_LENGTH", 100),
            proposer_timeout=_env_float("SAN_PROPOSER_TIMEOUT", 6.0),
            max_proposer_rounds=_env_int("SAN_MAX_PROPOSER_ROUNDS", 16),
            max_orphans=_env_int("SAN_MAX_ORPHANS", 64),
            max_reorg_depth=_env_int("SAN_MAX_REORG_DEPTH", 64),
            block_gossip=_env_bool("SAN_BLOCK_GOSSIP", True),
            tls_cert=_env_str("SAN_TLS_CERT"),
            tls_key=_env_str("SAN_TLS_KEY"),
            tls_ca=_env_str("SAN_TLS_CA"),
            db_path=_env_str("SAN_DB_PATH"),
            db_backend=_env_str("SAN_DB_BACKEND", "lmdb") or "lmdb",
            sync_batch_size=_env_int("SAN_SYNC_BATCH", 128),
            sync_max_blocks=_env_int("SAN_SYNC_MAX_BLOCKS", 50_000),
            snapshot_interval=_env_int("SAN_SNAPSHOT_INTERVAL", 1000),
            prune_keep=_env_int("SAN_PRUNE_KEEP", 0),
            rpc_rate_limit=_env_int("SAN_RPC_RATE_LIMIT", 120),
            rpc_rate_window=_env_float("SAN_RPC_RATE_WINDOW", 10.0),
            rpc_max_body=_env_int("SAN_RPC_MAX_BODY", 1 << 20),
        )

    @property
    def tls_enabled(self) -> bool:
        return bool(self.tls_cert and self.tls_key)

    def __post_init__(self):
        """Normalize genesis keys (address or public key) to addresses.

        This runs for every construction path (env, tests, embedders), so the
        ledger is always keyed by account address.
        """
        normalized: dict[str, int] = {}
        for key, units in self.genesis_allocations.items():
            try:
                normalized[_normalize_allocation_key(key)] = int(units)
            except (TypeError, ValueError) as exc:
                logger.warning("Ignoring genesis allocation %r: %s", key, exc)
        if normalized != self.genesis_allocations:
            object.__setattr__(self, "genesis_allocations", normalized)


def normalize_peer(peer, default_api_port: int = 8000) -> dict | None:
    """Coerce a peer entry into a record dict.

    Accepts the legacy ``"host:api_port"`` / ``"host"`` strings and the full
    record dict ``{"host", "api_port", "p2p_port", "peer_port",
    "controller_port"}``.
    """
    if isinstance(peer, str):
        host, _, port = peer.partition(":")
        host = host.strip()
        if not host:
            return None
        record: dict = {"host": host}
        if port.strip():
            try:
                record["api_port"] = int(port)
            except ValueError:
                logger.warning("Invalid peer port in %r", peer)
                return None
        else:
            record["api_port"] = default_api_port
        return record

    if isinstance(peer, dict):
        host = str(peer.get("host") or "").strip()
        if not host:
            return None
        record = dict(peer)
        record["host"] = host
        for key in ("api_port", "p2p_port", "peer_port", "controller_port"):
            if key in record and record[key] is not None:
                try:
                    record[key] = int(record[key])
                except (TypeError, ValueError):
                    logger.warning("Invalid %s in peer %r", key, peer)
                    return None
        return record

    return None


def complete_peer(peer, config: NodeConfig) -> dict | None:
    """Normalize a peer and fill in the local port defaults for missing ports."""
    record = normalize_peer(peer, default_api_port=config.api_port)
    if record is None:
        return None
    record.setdefault("p2p_port", config.p2p_port)
    record.setdefault("peer_port", config.peer_port)
    record.setdefault("controller_port", config.controller_port)
    return record


def same_peer(a: dict | None, b: dict | None) -> bool:
    if not a or not b:
        return False
    return a.get("host") == b.get("host") and a.get("api_port") == b.get("api_port")


def peer_api_url(peer: dict, path: str, scheme: str = "http") -> str:
    return f"{scheme}://{peer['host']}:{peer['api_port']}{path}"


def peer_ws_url(peer: dict, port_key: str) -> str:
    scheme = "wss" if peer.get("tls") else "ws"
    return f"{scheme}://{peer['host']}:{peer[port_key]}"


def peer_label(peer: dict | None) -> str:
    if not peer:
        return "<none>"
    return f"{peer.get('host')}:{peer.get('peer_port', '?')}"
