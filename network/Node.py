import asyncio
import copy
import hashlib
import json
import logging
import random
import socket
import time

import grpc
import requests

from blockchain.address import (
    AddressError,
    address_from_public_key,
    normalize_address,
    try_address_from_public_key,
)
from blockchain.Block import SCHEMA_VERSION, Block
from blockchain.Blockchain import Blockchain, governance_value_ok
from blockchain.economics import (
    FINALITY_DENOMINATOR,
    FINALITY_NUMERATOR,
    INITIAL_BASE_FEE,
    next_base_fee,
    san_to_units,
    units_to_san,
)
from blockchain.identity import NodeIdentity
from blockchain.merkle import (
    account_key,
    merkle_proof,
    state_entry_proof,
)
from blockchain.merkle import state_root as compute_state_root
from blockchain.persistence import ChainStore, transaction_id
from blockchain.Transaction import Transaction
from network import transport
from network.config import (
    NodeConfig,
    complete_peer,
    peer_label,
    same_peer,
)
from network.transport import PeerStream
from SANVM.pena_parser import PenaParser
from SANVM.Storage import Storage
from SANVM.VM import SANVirtualMachine, VMError
from utils import canonical
from utils.parser import Parser

logger = logging.getLogger(__name__)

# P2P protocol version; a handshake with a different version is rejected.
PROTOCOL_VERSION = 2
# Reject blocks dated more than this far into the future. Kept small so
# timestamp-justified proposer rounds cannot be inflated arbitrarily.
BLOCK_FUTURE_DRIFT = 120
# A block extending the live tip may not be backdated more than this far;
# historical replay (sync/restart) is exempt because old blocks are legitimately
# in the past. This is what stops a subsidy proposer from minting a burst with
# sequential historical timestamps.
BLOCK_PAST_DRIFT = 120
# How far ahead of the tip a finality vote for an uncommitted block may be
# stored (audit fix: unbounded future-vote storage) and how many staged
# voters a single block hash may collect before the block arrives.
VOTE_LOOKAHEAD = 64
MAX_STAGED_VOTERS_PER_HASH = 128
# A legitimate vote is a public key plus a signature (~8 KB hex); anything
# larger is junk and must not be retained (audit fix: oversized vote spam).
VOTE_MAX_BYTES = 16_384
# Handshake timestamps older/newer than this are rejected.
HELLO_TTL = 60.0

# Fields that are not covered by a vote signature.
VOTE_META_FIELDS = ("signature", "public_key")
# Fields that are not covered by a peer-record signature.
PEER_META_FIELDS = ("signature",)
# Fields that are not covered by a handshake signature.
HELLO_META_FIELDS = ("signature",)


class Node:
    """A SAN Network node: chain state, mempool, P2P listeners and consensus.

    The constructor never performs network I/O. Servers and background tasks
    are started by :meth:`start` and torn down by :meth:`stop`, which the
    FastAPI lifespan drives. When ``db_path`` is configured the chain, ledger
    state and contract storage survive restarts.
    """

    def __init__(self, config: NodeConfig | None = None, identity: NodeIdentity | None = None):
        self.config = config or NodeConfig.from_env()
        self.identity = identity or NodeIdentity.load(self.config.key_file)
        self.chain_id = self.config.chain_id

        self.PEERS: list[dict] = []
        self.incoming_node: dict | None = None
        self.outgoing_node: dict | None = None
        self.controller_nodes: list[dict] = []
        self._seen_peer_updates: dict[str, float] = {}

        self.blockchain = Blockchain(
            self.config.genesis_allocations,
            chain_id=self.config.chain_id,
            min_validator_stake=self.config.min_validator_stake,
            unbonding_period=self.config.unbonding_period,
            slash_bps=self.config.slash_bps,
            block_gas_limit=self.config.block_gas_limit,
            proposer_timeout_ms=int(self.config.proposer_timeout * 1000),
            block_reward=san_to_units(self.config.block_reward),
            min_block_interval_ms=int(self.config.min_block_interval_ms),
        )
        self.reward_address = ""
        if self.config.reward_address:
            try:
                self.reward_address = normalize_address(self.config.reward_address)
            except AddressError as exc:
                raise ValueError(
                    f"Invalid SAN_REWARD_ADDRESS {self.config.reward_address!r}: {exc}"
                ) from exc
        self.finalized_height = 0
        self.finalized_hash = self.blockchain.tip.current_block_hash
        self._finality_votes: dict[int, dict[str, dict[str, dict]]] = {}
        # Validator weights frozen when a block is committed, so finality can
        # never be re-weighted by later stake/registry changes.
        self._finality_sets: dict[int, dict[str, int]] = {}
        if (
            self.config.snapshot_interval > 0
            and 0 < self.config.prune_keep < self.config.snapshot_interval
        ):
            logger.warning(
                "SAN_PRUNE_KEEP (%d) is below SAN_SNAPSHOT_INTERVAL (%d); pruning "
                "will be limited to the newest snapshot height",
                self.config.prune_keep,
                self.config.snapshot_interval,
            )
        # Heights committed but not yet voted on (any commit path), so a vote
        # can never be skipped when block gossip, sync and orphan connection
        # interleave. Drained by _flush_pending_votes.
        self._pending_votes: set[int] = set()
        self._equivocation_evidence: list[dict] = []
        self._seen_votes: set[str] = set()
        # Receipts of recent blocks (bounded) and operational counters.
        self._receipts: dict[int, list[dict]] = {}
        self.metrics: dict[str, int] = {
            "blocks_committed": 0,
            "transactions_committed": 0,
            "votes_received": 0,
            "votes_seen": 0,
            "vote_messages_received": 0,
            "votes_dropped_invalid": 0,
            "votes_dropped_height": 0,
            "votes_dropped_voter": 0,
            "votes_dropped_duplicate": 0,
            "votes_dropped_equivocation": 0,
            "votes_broadcast": 0,
            "votes_unbroadcast": 0,
            "reorgs": 0,
            "slashing_events": 0,
            "governance_changes": 0,
        }
        self.last_seen_block_index = self.blockchain.tip.index
        self._chain_hashes: set[str] = {
            block.current_block_hash for block in self.blockchain.chain
        }
        self._orphans: dict[str, Block] = {}
        self._seen_block_gossip: set[str] = set()
        self._peer_failures: dict[str, int] = {}
        self._requested_blocks: set[str] = set()
        self._sync_misses = 0
        # Proposer fallback state: {height, round, started} (see _current_round).
        self._proposer_state: dict | None = None
        # State at the first block of the (possibly pruned) chain window.
        self._anchor_state: dict | None = None

        self.storage = Storage()
        self.transaction_pool: list[Transaction] = []
        self._pool_tx_ids: set[str] = set()
        self._seen_tx_gossip: set[str] = set()

        self.store = (
            ChainStore(self.config.db_path, backend=self.config.db_backend)
            if self.config.db_path
            else None
        )

        self._tasks: list[asyncio.Task] = []
        self._servers: list = []
        self._running = False
        self._lock = asyncio.Lock()

        if self.store is not None:
            if self.store.is_empty():
                self.store.append_block(
                    self.blockchain.tip,
                    receipts=[],
                    state=self.blockchain.state_snapshot(),
                    storage=self.storage.to_dict(),
                    head=0,
                )
                self.store.set_meta(
                    "genesis_allocation", self._genesis_allocation_fingerprint()
                )
            else:
                self._load_persisted_state(self.store)

    # ------------------------------------------------------------------ #
    # Lifecycle
    # ------------------------------------------------------------------ #

    async def start(self):
        if self._running:
            return
        self._running = True
        await self._start_servers()
        await self._bootstrap()
        self._refresh_peer_selection()
        self._tasks.append(asyncio.create_task(self._peer_health_loop()))
        self._tasks.append(asyncio.create_task(self._block_production_loop()))

        if self.PEERS:
            await self.synchronize()
            if not self.transaction_pool:
                await self.request_mempool()

        logger.info(
            "Node started (api=%s p2p=%s peer=%s controller=%s peers=%d tls=%s db=%s)",
            self.config.api_port,
            self.config.p2p_port,
            self.config.peer_port,
            self.config.controller_port,
            len(self.PEERS),
            self.config.tls_enabled,
            bool(self.store),
        )

    async def stop(self):
        self._running = False

        tasks, self._tasks = self._tasks, []
        for task in tasks:
            task.cancel()
        for task in tasks:
            try:
                await task
            except asyncio.CancelledError:
                pass

        servers, self._servers = self._servers, []
        for server in servers:
            try:
                await server.stop(0.5)
            except Exception:  # noqa: BLE001 - shutdown must not raise
                pass

        if self.store is not None:
            self.store.close()

    async def _start_servers(self):
        """Start the gRPC P2P server.

        One service is bound to the peer, p2p and controller ports so existing
        peer records keep working; every port speaks the same protocol and the
        message type decides how a session is handled.
        """
        server = transport.build_server(self)
        await server.start()
        self._servers.append(server)

    # ------------------------------------------------------------------ #
    # TLS
    # ------------------------------------------------------------------ #

    def transport_server_credentials(self):
        if not self.config.tls_enabled:
            logger.warning(
                "TLS is disabled: P2P traffic is plaintext. Set SAN_TLS_CERT/"
                "SAN_TLS_KEY (and SAN_TLS_CA on peers) for production."
            )
            return None
        with open(self.config.tls_cert, "rb") as handle:
            certificate = handle.read()
        with open(self.config.tls_key, "rb") as handle:
            private_key = handle.read()
        return grpc.ssl_server_credentials([(private_key, certificate)])

    def transport_client_credentials(self, peer: dict):
        """Client credentials: the CA bundle verifies the server certificate.

        Without ``SAN_TLS_CA`` the system trust store is used, so self-signed
        certificates only work when their CA is configured explicitly.
        """
        if not self.config.tls_ca:
            logger.warning(
                "SAN_TLS_CA is not configured; the system trust store is used "
                "for peer certificates"
            )
            return grpc.ssl_channel_credentials()
        with open(self.config.tls_ca, "rb") as handle:
            ca = handle.read()
        return grpc.ssl_channel_credentials(root_certificates=ca)

    # ------------------------------------------------------------------ #
    # Protocol handshake (chain_id + protocol version + signed HELLO)
    # ------------------------------------------------------------------ #

    def _hello_payload(self, message_type: str) -> dict:
        record = {
            "type": message_type,
            "protocol": PROTOCOL_VERSION,
            "chain_id": self.chain_id,
            "public_key": self.get_public_key(),
            "timestamp": time.time(),
        }
        signature = self.identity.sign_hex(canonical.dumps_bytes(record))
        if signature:
            record["signature"] = signature
        return record

    def _verify_hello(self, data: dict) -> bool:
        if not isinstance(data, dict):
            return False
        if data.get("type") not in ("HELLO", "HELLO_ACK"):
            return False
        if data.get("protocol") != PROTOCOL_VERSION:
            logger.warning("Handshake rejected: protocol %r", data.get("protocol"))
            return False
        if data.get("chain_id") != self.chain_id:
            logger.warning(
                "Handshake rejected: chain_id %r does not match %r",
                data.get("chain_id"),
                self.chain_id,
            )
            return False
        raw_timestamp = data.get("timestamp")
        if not isinstance(raw_timestamp, (int, float)) or isinstance(raw_timestamp, bool):
            return False
        age = time.time() - float(raw_timestamp)
        if abs(age) > HELLO_TTL:
            logger.warning("Handshake rejected: stale timestamp")
            return False

        public_key = data.get("public_key")
        signature = data.get("signature")
        if not public_key or not signature:
            return False
        payload = {
            key: value for key, value in data.items() if key not in HELLO_META_FIELDS
        }
        return NodeIdentity.verify(canonical.dumps_bytes(payload), signature, public_key)

    async def _accept_handshake(self, websocket) -> bool:
        """First message on every inbound connection must be a valid HELLO."""
        try:
            raw = await asyncio.wait_for(
                websocket.recv(), timeout=self.config.ws_timeout * 2
            )
            data = json.loads(raw)
        except Exception as exc:  # noqa: BLE001
            logger.debug("Handshake failed: %s", exc)
            return False

        if data.get("type") != "HELLO" or not self._verify_hello(data):
            logger.warning("Rejected connection: invalid handshake")
            try:
                await websocket.close(code=1008, reason="handshake failed")
            except Exception:  # noqa: BLE001
                pass
            return False

        await websocket.send(json.dumps(self._hello_payload("HELLO_ACK")))
        return True

    async def _open_peer(self, peer: dict, port_key: str) -> PeerStream:
        """Open a gRPC session to a peer and complete the handshake."""
        return await transport.open_session(
            self, peer, port_key, self.config.ws_timeout
        )

    async def _bootstrap(self):
        if not self.config.bootstrap:
            return
        peers: list = []
        try:
            peers = await transport.remote_bootstrap(self, self.config.bootstrap)
        except Exception as exc:  # noqa: BLE001 - fall back to the REST endpoint
            logger.debug("gRPC bootstrap failed (%s); trying the REST endpoint", exc)
        if not peers:
            peers = await asyncio.to_thread(self.discover_peers, self.config.bootstrap)
        self.add_peers(peers)
        if not self.PEERS:
            logger.warning("Bootstrap %s returned no usable peers", self.config.bootstrap)
            return
        await self.register_to_network()

    # ------------------------------------------------------------------ #
    # Persistence
    # ------------------------------------------------------------------ #

    def _genesis_allocation_fingerprint(self) -> str:
        payload = canonical.dumps_bytes(self.config.genesis_allocations)
        return hashlib.sha256(payload).hexdigest()

    def _genesis_anchor_state(self) -> dict:
        """State that the first block of an unpruned chain starts from."""
        return {
            "balances": dict(self.config.genesis_allocations),
            "nonces": {},
            "validators": {},
            "total_slashed": 0,
            "total_burned": 0,
            "parameters": {
                "min_validator_stake": self.config.min_validator_stake,
                "unbonding_period": self.config.unbonding_period,
                "slash_bps": self.config.slash_bps,
                "block_gas_limit": self.config.block_gas_limit,
                "proposer_timeout_ms": int(self.config.proposer_timeout * 1000),
                "block_reward": san_to_units(self.config.block_reward),
                "min_block_interval_ms": int(self.config.min_block_interval_ms),
            },
            "base_fee": INITIAL_BASE_FEE,
            "storage": {},
        }

    def block_at(self, height: int) -> Block | None:
        """Block at an absolute height, honouring a pruned chain window."""
        if not self.blockchain.chain:
            return None
        offset = height - self.blockchain.chain[0].index
        if 0 <= offset < len(self.blockchain.chain):
            return self.blockchain.chain[offset]
        return None

    def block_hash_at(self, height: int) -> str | None:
        block = self.block_at(height)
        return block.current_block_hash if block is not None else None

    def _verify_window(self, chain: list) -> None:
        for previous, current in zip(chain, chain[1:]):
            if (
                current.index != previous.index + 1
                or current.previous_block_hash != previous.current_block_hash
                or current.current_block_hash != current.calculate_hash()
            ):
                raise RuntimeError(
                    f"Persisted chain is corrupt at block {current.index}; refusing to start"
                )

    def _load_persisted_state(self, store: ChainStore) -> None:
        chain = store.load_chain()
        if not chain:
            return

        stored_finalized_height = store.get_meta("finalized_height")
        stored_finalized_hash = store.get_meta("finalized_hash")

        stored_fingerprint = store.get_meta("genesis_allocation")
        local_fingerprint = self._genesis_allocation_fingerprint()
        if stored_fingerprint and stored_fingerprint != local_fingerprint:
            raise RuntimeError(
                "Persisted chain was created with a different genesis allocation "
                "(SAN_GENESIS_ALLOCATION); refusing to start"
            )

        if chain[0].index == 0 and (len(chain) == 1 or chain[1].index == 1):
            # Full chain: verify against this node's genesis and replay state.
            expected_genesis = self.blockchain.tip.current_block_hash
            if chain[0].current_block_hash != expected_genesis:
                raise RuntimeError(
                    "Persisted chain genesis does not match this node's genesis "
                    "(different SAN_GENESIS_ALLOCATION?); refusing to start"
                )
            self._verify_window(chain)
            self.blockchain.chain = chain
            state = store.load_state()
            if state:
                self.blockchain.load_state(state)
            storage = store.load_storage()
            if storage:
                self.storage.load_from_dict(storage)
            self._anchor_state = self._genesis_anchor_state()
        else:
            # Pruned chain: anchor the window at the latest snapshot.
            snapshot = None
            for candidate in store.list_snapshots():
                if store.block_hash_at(candidate["height"]) == candidate["hash"]:
                    snapshot = candidate
                    break
            if snapshot is None:
                raise RuntimeError(
                    "Persisted chain is pruned but has no matching snapshot; refusing to start"
                )
            window = [block for block in chain if block.index >= snapshot["height"]]
            if not window or window[0].index != snapshot["height"]:
                raise RuntimeError(
                    "Persisted snapshot does not match the stored block window; refusing to start"
                )
            self._verify_window(window)
            header = window[0]
            if header.state_root is not None:
                recomputed = compute_state_root(
                    snapshot["state"]["balances"],
                    snapshot["state"]["nonces"],
                    snapshot["state"]["validators"],
                    snapshot["state"]["total_slashed"],
                    snapshot["storage"],
                    snapshot["state"].get("parameters"),
                    snapshot["state"].get("base_fee", INITIAL_BASE_FEE),
                    snapshot["state"].get("total_burned", 0),
                )
                if recomputed != header.state_root:
                    raise RuntimeError(
                        "Persisted snapshot state root does not match its header; refusing to start"
                    )
            self._anchor_state = {
                **snapshot["state"],
                "storage": snapshot["storage"],
            }
            if not self._replay_chain(window, historical=True):
                raise RuntimeError(
                    "Failed to replay the stored block window; refusing to start"
                )
            logger.info(
                "Loaded a pruned window starting at height %d (snapshot anchor)",
                window[0].index,
            )

        self.last_seen_block_index = chain[-1].index
        self._chain_hashes = {block.current_block_hash for block in self.blockchain.chain}

        self._require_loaded_state_roots(chain)
        self._load_finality_state(store)
        if stored_finalized_height is not None and stored_finalized_hash:
            height = int(stored_finalized_height)
            block = self.block_at(height)
            if block is not None and block.current_block_hash == stored_finalized_hash:
                # Audit fix: finality only ever moves forward. A crash between
                # the finality blob write and the metadata write must not allow
                # a lower checkpoint to reopen finalized history.
                if height > self.finalized_height:
                    self.finalized_height = height
                    self.finalized_hash = stored_finalized_hash
            else:
                logger.warning(
                    "Persisted finality checkpoint does not match the chain; ignoring"
                )
        # The loaded state must match the chain's committed state root.
        tip_block = self.block_at(self.blockchain.tip.index)
        if tip_block is not None and tip_block.state_root is not None:
            if self.current_state_root() != tip_block.state_root:
                raise RuntimeError(
                    "Persisted state does not match the chain state root; refusing to start"
                )

        logger.info(
            "Loaded %d block(s) from %s (window starts at %d)",
            len(self.blockchain.chain),
            self.config.db_path,
            self.blockchain.chain[0].index,
        )

    # ------------------------------------------------------------------ #
    # Peer discovery / gossip
    # ------------------------------------------------------------------ #

    async def ping_node(self, peer: dict) -> bool:
        """Return True only when the peer completes a handshake and answers PING."""
        websocket = None
        try:
            websocket = await self._open_peer(peer, "peer_port")
            await websocket.send(json.dumps({"type": "PING"}))
            response = await asyncio.wait_for(websocket.recv(), timeout=3)
            data = json.loads(response)
            return data.get("type") == "PONG"
        except Exception as exc:  # noqa: BLE001 - unreachable peer is a valid outcome
            logger.debug("Ping to %s failed: %s", peer_label(peer), exc)
            return False
        finally:
            if websocket is not None:
                try:
                    await websocket.close()
                except Exception:  # noqa: BLE001
                    pass

    @staticmethod
    def get_local_ip() -> str:
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.connect(("8.8.8.8", 80))
            local_ip = sock.getsockname()[0]
            sock.close()
            return local_ip
        except Exception as exc:  # noqa: BLE001
            logger.debug("Could not determine local IP: %s", exc)
            try:
                return socket.gethostbyname(socket.gethostname())
            except Exception:  # noqa: BLE001
                return "127.0.0.1"

    @staticmethod
    def discover_peers(bootstrap_node: str) -> list:
        """Fetch the peer list from the bootstrap node (best effort)."""
        base_url = bootstrap_node if "://" in bootstrap_node else f"http://{bootstrap_node}"
        try:
            response = requests.get(f"{base_url.rstrip('/')}/bootstrap", timeout=5)
            response.raise_for_status()
            payload = response.json()
            return payload.get("peers") or []
        except Exception as exc:  # noqa: BLE001
            logger.warning("Could not fetch peers from %s: %s", bootstrap_node, exc)
            return []

    # ------------------------------------------------------------------ #
    # Peer records (signed, time bounded)
    # ------------------------------------------------------------------ #

    @staticmethod
    def _peer_record_payload(record: dict) -> bytes:
        payload = {
            key: value for key, value in record.items() if key not in PEER_META_FIELDS
        }
        return canonical.dumps_bytes(payload)

    def self_peer_record(self) -> dict:
        record = {
            "chain_id": self.chain_id,
            "host": self.config.advertise_host or self.get_local_ip(),
            "api_port": self.config.api_port,
            "p2p_port": self.config.p2p_port,
            "peer_port": self.config.peer_port,
            "controller_port": self.config.controller_port,
            "timestamp": time.time(),
            "tls": self.config.tls_enabled,
        }
        if self.identity.public_key_hex:
            record["public_key"] = self.identity.public_key_hex
            signature = self.identity.sign_hex(self._peer_record_payload(record))
            if signature:
                record["signature"] = signature
        return record

    def _verify_peer_record(self, record: dict) -> bool:
        if record.get("chain_id") != self.chain_id:
            logger.warning(
                "Rejected peer record for chain %r", record.get("chain_id")
            )
            return False

        public_key = record.get("public_key")
        signature = record.get("signature")

        if not public_key or not signature:
            if self.config.require_block_signature:
                logger.warning(
                    "Rejected unsigned peer record from %s", record.get("host")
                )
                return False
            return True

        timestamp = record.get("timestamp")
        if timestamp is not None:
            try:
                age = time.time() - float(timestamp)
            except (TypeError, ValueError):
                return False
            if age > self.config.peer_record_ttl or age < -self.config.peer_record_ttl:
                logger.warning(
                    "Rejected stale peer record from %s (age %.0fs)",
                    record.get("host"),
                    age,
                )
                return False

        return NodeIdentity.verify(
            self._peer_record_payload(record), signature, public_key
        )

    def _seen_recently(self, record: dict) -> bool:
        """Deduplicate peer updates by identity (ports + key), ignoring the
        timestamp so periodic re-announcements do not trigger re-gossip."""
        identity = {
            key: value
            for key, value in record.items()
            if key not in (*PEER_META_FIELDS, "timestamp")
        }
        digest = hashlib.sha256(canonical.dumps_bytes(identity)).hexdigest()
        now = time.time()
        cutoff = now - self.config.peer_record_ttl
        self._seen_peer_updates = {
            key: seen for key, seen in self._seen_peer_updates.items() if seen >= cutoff
        }
        if digest in self._seen_peer_updates:
            return True
        self._seen_peer_updates[digest] = now
        return False

    async def register_to_network(self) -> bool:
        if self.outgoing_node is None:
            logger.warning("No outgoing node configured; cannot register")
            return False
        message = json.dumps({"type": "PEER_UPDATE", "peer": self.self_peer_record()})
        return await self._send_to_peer(self.outgoing_node, "peer_port", message)

    def _is_self(self, peer: dict) -> bool:
        local_hosts = {"127.0.0.1", "localhost", "0.0.0.0", self.get_local_ip()}
        return (
            peer.get("host") in local_hosts
            and peer.get("api_port") == self.config.api_port
        )

    def _merge_peers(self, peers) -> int:
        added = 0
        for raw in peers or []:
            added += self._add_peer(raw)
        return added

    def _add_peer(self, raw) -> int:
        record = complete_peer(raw, self.config)
        if record is None or self._is_self(record):
            return 0
        if not self._verify_peer_record(record):
            return 0
        if any(same_peer(record, known) for known in self.PEERS):
            return 0
        if len(self.PEERS) >= self.config.max_peers:
            logger.warning(
                "Peer limit reached (%d); ignoring %s",
                self.config.max_peers,
                peer_label(record),
            )
            return 0
        self.PEERS.append(record)
        return 1

    def _remove_peer(self, peer: dict | None) -> None:
        if peer is None:
            return
        self.PEERS = [known for known in self.PEERS if not same_peer(known, peer)]
        self._refresh_peer_selection()

    def add_peers(self, peers) -> int:
        """Merge discovered peers and refresh the incoming/outgoing selection."""
        added = self._merge_peers(peers)
        self._refresh_peer_selection()
        return added

    # ------------------------------------------------------------------ #
    # Controller selection (deterministic per epoch)
    # ------------------------------------------------------------------ #

    def _controller_eligible(self, peer: dict) -> bool:
        public_key = peer.get("public_key")
        if not public_key:
            return False
        if self.config.controller_min_stake > 0:
            address = try_address_from_public_key(public_key)
            if address is None:
                return False
            return (
                self.blockchain.SAN.get(address, 0)
                >= self.config.controller_min_stake
            )
        return True

    def _select_controllers(self) -> list[dict]:
        eligible = [peer for peer in self.PEERS if self._controller_eligible(peer)]
        if not eligible:
            return []

        epoch = self.blockchain.tip.index // max(self.config.epoch_length, 1)

        def score(peer: dict) -> bytes:
            return hashlib.sha256(
                f"{epoch}:{peer['public_key']}".encode("utf-8")
            ).digest()

        ranked = sorted(eligible, key=score)
        return ranked[: min(self.config.controller_count, len(ranked))]

    def _refresh_peer_selection(self) -> None:
        pool = list(self.PEERS)
        random.shuffle(pool)
        self.incoming_node = pool[0] if pool else None
        self.outgoing_node = pool[1] if len(pool) > 1 else (pool[0] if pool else None)
        self.controller_nodes = self._select_controllers()

    async def _block_production_loop(self) -> None:
        """Try to produce a block frequently (tx-driven or subsidy-driven)."""
        while self._running:
            interval = max(min(self.blockchain.proposer_timeout, 2.0), 0.5)
            await asyncio.sleep(interval)
            try:
                await self._maybe_produce_from_pool()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001
                logger.error("Block production loop failed: %s", exc)

    async def _peer_health_loop(self):
        while self._running:
            await asyncio.sleep(self.config.peer_check_interval)
            try:
                await self.check_dead_peers()
                if self.PEERS:
                    # Re-announce ourselves so a transient eviction self-heals.
                    await self.register_to_network()
                # Retry block production (e.g. a controller was briefly down).
                await self._maybe_produce_from_pool()
                # Cast any missing finality votes (idempotent).
                await self._flush_pending_votes()
                # Re-broadcast our own still-unfinalized votes so a dropped
                # gossip message cannot stall finality.
                await self._rebroadcast_own_votes()
                # Retry fetching parents of orphan blocks.
                missing_parents = {
                    block.previous_block_hash
                    for block in self._orphans.values()
                    if block.previous_block_hash not in self._chain_hashes
                    and block.previous_block_hash not in self._orphans
                }
                for parent_hash in list(missing_parents)[:8]:
                    await self._request_block(parent_hash)
                # Keep the mempool convergent (Bitcoin-style getdata): pull
                # when the pool is empty, or when we are the next proposer and
                # a one-shot gossip may have been missed.
                height = self.blockchain.tip.index + 1
                if self.PEERS and (
                    not self.transaction_pool
                    or self._is_expected_proposer(height, self._current_round(height))
                ):
                    await self.request_mempool()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001
                logger.error("Peer health check failed: %s", exc)

    async def check_dead_peers(self):
        """Ping every known peer and drop the ones that keep not answering.

        Pinging all peers (not just the active in/out pair) keeps the
        controller set healthy: a stopped node is evicted before its missing
        vote can block consensus. A single miss is tolerated, and peers
        re-announce themselves periodically, so transient failures self-heal.
        """
        for peer in list(self.PEERS):
            key = f"{peer.get('host')}:{peer.get('api_port')}"
            if await self.ping_node(peer):
                self._peer_failures.pop(key, None)
                continue

            failures = self._peer_failures.get(key, 0) + 1
            self._peer_failures[key] = failures
            if failures < self.config.peer_miss_threshold:
                logger.info(
                    "Peer %s missed a health check (%d/%d)",
                    peer_label(peer),
                    failures,
                    self.config.peer_miss_threshold,
                )
                continue

            logger.info("Peer %s is unresponsive; removing", peer_label(peer))
            self._peer_failures.pop(key, None)
            self._remove_peer(peer)
            await self.gossip_dead_peer(peer)

        self._refresh_peer_selection()

    async def gossip_peers(self, new_peer: dict) -> bool:
        """Announce a newly learned peer to every known peer.

        Broadcasting (instead of forwarding to a single outgoing node) makes
        discovery converge to a full mesh; deduplication on the receiver keeps
        the exchange loop-free.
        """
        if not self.PEERS:
            logger.debug("No peers; gossip skipped")
            return False
        message = json.dumps({"type": "PEER_UPDATE", "peer": new_peer})
        delivered = False
        for peer in list(self.PEERS):
            if same_peer(peer, new_peer):
                continue
            delivered = await self._send_to_peer(peer, "peer_port", message) or delivered
        return delivered

    async def gossip_dead_peer(self, dead_peer: dict) -> bool:
        if self.outgoing_node is None:
            return False
        message = json.dumps({"type": "DEAD_PEER", "peer": dead_peer})
        return await self._send_to_peer(self.outgoing_node, "peer_port", message)

    async def _send_to_peer(self, peer: dict | None, port_key: str, message: str) -> bool:
        """One-shot gRPC session: handshake, one message, close."""
        if peer is None:
            return False
        stream = None
        try:
            stream = await self._open_peer(peer, port_key)
            await stream.send(message)
            return True
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "Could not send message to %s (%s): %s", peer_label(peer), port_key, exc
            )
            return False
        finally:
            if stream is not None:
                try:
                    await stream.close()
                except Exception:  # noqa: BLE001
                    pass

    # ------------------------------------------------------------------ #
    # gRPC session handlers
    # ------------------------------------------------------------------ #

    async def _peer_session(self, stream: PeerStream) -> None:
        """Serve one inbound gRPC session: handshake, then message dispatch."""
        try:
            await self._serve_session(stream)
        finally:
            await stream.close()

    async def _serve_session(self, stream: PeerStream) -> None:
        if not await self._accept_handshake(stream):
            return
        message_count = 0
        window_start = time.monotonic()
        try:
            async for raw in stream:
                now = time.monotonic()
                if now - window_start > self.config.peer_rate_window:
                    window_start = now
                    message_count = 0
                message_count += 1
                if message_count > self.config.peer_rate_limit:
                    logger.warning("Peer exceeded the message rate limit; closing")
                    await stream.close()
                    return

                try:
                    await self._dispatch_peer_message(stream, raw)
                except Exception as exc:  # noqa: BLE001 - never drop the server
                    logger.warning("Peer message failed: %s", exc)
        except Exception as exc:  # noqa: BLE001
            logger.debug("Peer connection closed: %s", exc)

    async def _dispatch_peer_message(self, stream: PeerStream, raw: str) -> None:
        """Route a message to the handler for its type.

        One gRPC service serves every P2P port, so block, vote and gossip
        messages are distinguished by type instead of by listening socket.
        """
        data = json.loads(raw)
        message_type = data.get("type")

        if message_type == "GET_BLOCK":
            await self._serve_block_request(stream, data.get("block_hash"))
            return

        if message_type == "BLOCK":
            await self._handle_incoming_block(stream, data)
            return

        if message_type == "BLOCK_VOTE_REQUEST":
            await self._serve_block_vote(stream, data)
            return

        await self._handle_peer_message(stream, raw)

    async def _handle_peer_message(self, stream: PeerStream, raw) -> None:
        data = json.loads(raw)
        message_type = data.get("type")

        if message_type == "PING":
            await stream.send(json.dumps({"type": "PONG"}))
            return

        if message_type == "PEER_UPDATE":
            new_peer = complete_peer(data.get("peer"), self.config)
            if new_peer is None or self._is_self(new_peer):
                return
            if not self._verify_peer_record(new_peer):
                return
            if self._seen_recently(new_peer):
                return
            if self._add_peer(new_peer):
                self._refresh_peer_selection()
                logger.info("New peer learned: %s", peer_label(new_peer))
                await self.gossip_peers(new_peer)
            return

        if message_type == "DEAD_PEER":
            # Audit finding: anyone could evict any peer with an unsigned
            # claim. Peers are only dropped by our own health checks now.
            logger.debug("Ignoring remote DEAD_PEER claim: %r", data.get("peer"))
            return

        if message_type == "GET_PEERS":
            await stream.send(json.dumps({"type": "PEERS", "peers": self.PEERS}))
            return

        if message_type == "TX":
            await self._handle_incoming_tx(data.get("tx"))
            return

        if message_type == "GET_TXS":
            txs = [tx.to_dict() for tx in self.transaction_pool[:64]]
            await stream.send(json.dumps({"type": "TXS", "txs": txs}))
            return

        if message_type == "TXS":
            for payload in (data.get("txs") or [])[:64]:
                await self._ingest_transaction(payload)
            return

        if message_type == "FINALITY_VOTE":
            self.metrics["vote_messages_received"] += 1
            await self._handle_finality_vote(data.get("vote"))
            return

        logger.debug("Unknown peer message type: %r", message_type)

    async def _handle_incoming_block(self, stream: PeerStream, data: dict) -> None:
        try:
            block = Block.from_dict(data["block"])
        except Exception as exc:  # noqa: BLE001
            logger.warning("Received malformed block: %s", exc)
            return

        async with self._lock:
            accepted = self._process_incoming_block(block)

        if accepted:
            self._sync_misses = 0
        elif (
            block.index == self.blockchain.tip.index + 1
            and block.previous_block_hash == self.blockchain.tip.current_block_hash
        ):
            # A tip-adjacent block we could not verify (e.g. the local
            # round clock lags behind): catch up instead of stalling.
            self._sync_misses += 1
            if self._sync_misses >= 2:
                self._sync_misses = 0
                await self.synchronize()

        if accepted and self.config.block_gossip:
            await self.gossip_block(block)
        if accepted:
            await self._maybe_vote(block)
            await self._flush_pending_votes()

        # If this block is still an orphan, ask a peer for its parent.
        if (
            accepted
            and block.previous_block_hash not in self._chain_hashes
            and block.previous_block_hash not in self._orphans
        ):
            await self._request_block(block.previous_block_hash)

    async def _serve_block_vote(self, stream: PeerStream, data: dict) -> None:
        """Answer a controller quorum vote request over the session."""
        approved = False
        block_hash = None
        try:
            block_data = data.get("block")
            if block_data:
                block = Block.from_dict(block_data)
                block_hash = block.current_block_hash
                approved = self.verify_block(block)
        except Exception as exc:  # noqa: BLE001
            logger.warning("Block vote failed: %s", exc)

        response = {
            "type": "BLOCK_VOTE_RESPONSE",
            "chain_id": self.chain_id,
            "approved": approved,
            "block_hash": block_hash,
        }
        signature = self._sign_vote(response)
        if signature:
            response["signature"] = signature
            response["public_key"] = self.get_public_key()

        await stream.send(json.dumps(response))

    # ------------------------------------------------------------------ #
    # Block verification and application
    # ------------------------------------------------------------------ #

    def verify_block(self, block: Block, *, historical: bool = False) -> bool:
        """Full block validation: chain binding, time, continuity, hash,
        state commitment, signature, proposer round and transactions.

        ``historical`` skips the wall-clock lower bound only (sync/restart
        replay of old blocks); every consensus rule still applies.
        """
        if block.chain_id != self.chain_id:
            logger.warning(
                "Block %s belongs to chain %r, not %r",
                block.index,
                block.chain_id,
                self.chain_id,
            )
            return False

        tip = self.blockchain.tip
        if block.index != tip.index + 1:
            return False
        if block.previous_block_hash != tip.current_block_hash:
            return False

        # Median-time-past: a block must be newer than the median of the last
        # 11 blocks and not more than two hours into the future.
        median = self.blockchain.median_time_past()
        if block.timestamp <= median:
            logger.warning(
                "Block %s: timestamp %.0f is not after median %.0f",
                block.index,
                block.timestamp,
                median,
            )
            return False
        if block.timestamp > time.time() + BLOCK_FUTURE_DRIFT:
            logger.warning("Block %s: timestamp is too far in the future", block.index)
            return False
        if not historical and block.timestamp < time.time() - BLOCK_PAST_DRIFT:
            logger.warning(
                "Block %s: timestamp %.0f is backdated beyond the live window",
                block.index,
                block.timestamp,
            )
            return False
        min_interval = self.blockchain.min_block_interval
        if min_interval > 0 and block.timestamp < tip.timestamp + min_interval:
            logger.warning(
                "Block %s: sooner than the minimum block interval (%.1fs)",
                block.index,
                min_interval,
            )
            return False

        if block.reward_address:
            try:
                normalize_address(block.reward_address)
            except AddressError:
                logger.warning(
                    "Block %s: invalid reward address %r",
                    block.index,
                    block.reward_address,
                )
                return False

        if self.config.require_state_root and block.index > 0 and block.state_root is None:
            # Audit fix: a proposer could otherwise omit the state commitment.
            logger.warning("Block %s: missing state root", block.index)
            return False

        if block.round > 0:
            # Audit fix: round claims must be justified in consensus terms, not
            # by a local clock, so every node (including sync/replay) agrees.
            required = tip.timestamp + block.round * self.blockchain.proposer_timeout * 0.8
            if block.timestamp < required:
                logger.warning(
                    "Block %s: round %s is not justified by its timestamp",
                    block.index,
                    block.round,
                )
                return False

        if block.current_block_hash != block.calculate_hash():
            logger.warning("Block %s: hash mismatch", block.index)
            return False
        if not self._verify_block_signature(block):
            logger.warning("Block %s: invalid validator signature", block.index)
            return False

        # With an active validator set, only the deterministic proposer may
        # produce a block at this height.
        active = self.blockchain.active_validators()
        if active:
            cap = max(int(self.config.max_proposer_rounds), len(active), 1)
            if block.round < 0 or block.round >= cap:
                logger.warning("Block %s: implausible proposer round %s", block.index, block.round)
                return False
            expected = self.expected_proposer(block.index, active, block.round)
            actual = try_address_from_public_key(block.validator) if block.validator else None
            if expected != actual:
                logger.warning(
                    "Block %s (round %s): proposer %s is not the expected %s",
                    block.index,
                    block.round,
                    actual,
                    expected,
                )
                return False


        return self._simulate_block(block, apply=False) is not None

    def expected_proposer(
        self, height: int, active: dict[str, int] | None = None, round: int = 0
    ) -> str | None:
        """Deterministic proposer for a height and round.

        Every node derives the same base proposer from the chain id and the
        height; each subsequent round rotates to the next validator, so an
        offline proposer no longer stalls the chain (Tendermint-style rounds).
        """
        if active is None:
            active = self.blockchain.active_validators()
        if not active:
            return None
        ranked = sorted(active)
        seed = hashlib.sha256(f"{self.chain_id}:{height}".encode("utf-8")).digest()
        base = int.from_bytes(seed[:8], "big") % len(ranked)
        return ranked[(base + int(round)) % len(ranked)]

    def _current_round(self, height: int) -> int:
        """Round number for a height; advances as proposer timeouts elapse."""
        now = time.monotonic()
        state = self._proposer_state
        if state is None or state["height"] != height:
            self._proposer_state = {"height": height, "round": 0, "started": now}
            return 0

        active = self.blockchain.active_validators()
        cap = max(int(self.config.max_proposer_rounds), len(active), 1)
        elapsed = now - state["started"]
        advance = int(elapsed // max(self.blockchain.proposer_timeout, 0.1))
        round_ = min(advance, cap - 1)
        if round_ > state["round"]:
            state["round"] = round_
            logger.info("Height %d: proposer round advanced to %d", height, round_)
        return state["round"]

    def _simulate_block(self, block: Block, *, apply: bool) -> dict | None:
        """Validate a block and optionally apply its effects.

        Everything runs on copies of the ledger state, so a rejected block can
        never leave partial state behind. With ``apply=True`` the simulation
        result is adopted atomically (balances, nonces, validators, storage,
        fee market and governance parameters).
        """
        rate = self.blockchain.next_fee_rate()
        balances = dict(self.blockchain.SAN)
        nonces = dict(self.blockchain.nonces)
        storage = Storage.from_dict(self.storage.to_dict())
        validators = copy.deepcopy(self.blockchain.validators)
        total_slashed = self.blockchain.total_slashed
        total_burned = self.blockchain.total_burned
        parameters = dict(self.blockchain.parameters)
        base_fee = int(self.blockchain.base_fee)
        block_gas_limit = int(parameters.get("block_gas_limit", 0))
        vm = SANVirtualMachine(storage, verbose=False)

        total_gas = 0
        total_gas_used = 0
        total_tips = 0
        receipts: list[dict] = []

        for tx_index, tx in enumerate(block.transactions):
            if not isinstance(tx, dict):
                return None
            if Transaction.chain_id_of(tx) != self.chain_id:
                logger.warning("Block %s: transaction from another chain", block.index)
                return None
            if not Transaction.verify_transaction(tx):
                logger.warning("Block %s: invalid transaction signature", block.index)
                return None
            gas_error = Transaction.validate_gas_fields(tx)
            if gas_error:
                logger.warning("Block %s: %s", block.index, gas_error)
                return None
            validator_error = Transaction.validate_validator_command(tx)
            if validator_error:
                logger.warning("Block %s: %s", block.index, validator_error)
                return None
            governance_error = Transaction.validate_governance_command(tx)
            if governance_error:
                logger.warning("Block %s: %s", block.index, governance_error)
                return None
            if tx.get("fee") != Transaction.expected_fee(tx, rate):
                logger.warning("Block %s: invalid transaction fee", block.index)
                return None

            sender = tx.get("sender")
            if not isinstance(sender, str) or not sender:
                return None
            try:
                sender_address = address_from_public_key(sender)
            except AddressError:
                logger.warning("Block %s: invalid sender public key", block.index)
                return None

            nonce = tx.get("nonce")
            if not isinstance(nonce, int) or isinstance(nonce, bool) or nonce < 0:
                return None
            if nonce != nonces.get(sender_address, 0):
                logger.warning(
                    "Block %s: invalid nonce for %s", block.index, sender_address
                )
                return None

            gas_limit = Transaction.gas_limit_of(tx)
            total_gas += gas_limit
            if total_gas > block_gas_limit:
                logger.warning("Block %s: block gas limit exceeded", block.index)
                return None

            gas_price = Transaction.gas_price_of(tx)
            if Transaction.has_execution(tx) and gas_price < base_fee:
                logger.warning(
                    "Block %s: gas price %d is below the base fee %d",
                    block.index,
                    gas_price,
                    base_fee,
                )
                return None

            fee = int(tx["fee"])
            value_units = 0
            if "value" in tx:
                try:
                    value_units = san_to_units(tx["value"])
                except ValueError:
                    logger.warning("Block %s: invalid transaction value", block.index)
                    return None
                if value_units <= 0:
                    logger.warning("Block %s: non-positive transaction value", block.index)
                    return None
                try:
                    receiver = normalize_address(tx.get("receiver"))
                except AddressError:
                    logger.warning("Block %s: invalid receiver address", block.index)
                    return None
                balances[receiver] = balances.get(receiver, 0) + value_units

            sender_balance = balances.get(sender_address, 0)
            if sender_balance < value_units + fee:
                logger.warning(
                    "Block %s: insufficient balance for %s", block.index, sender_address
                )
                return None
            balances[sender_address] = sender_balance - value_units - fee
            nonces[sender_address] = nonce + 1

            execution: dict = {"gas_used": 0, "status": "success", "error": None, "logs": []}
            if Transaction.has_execution(tx):
                execution = self._run_tx_execution(vm, storage, tx)
            gas_used = int(execution["gas_used"])
            total_gas_used += gas_used

            # Unused gas is refunded; the base fee is burned and only the tip
            # goes to the validator.
            refund = (gas_limit - gas_used) * gas_price
            balances[sender_address] += refund
            burned = gas_used * base_fee
            total_burned += burned
            total_tips += fee - refund - burned

            receipts.append(
                {
                    "tx_index": tx_index,
                    "tx_id": hashlib.sha256(Transaction.serialize_message(tx)).hexdigest(),
                    "sender": sender_address,
                    "status": execution["status"],
                    "gas_used": gas_used,
                    "gas_limit": gas_limit,
                    "failure": execution.get("error"),
                    "logs": execution.get("logs", []),
                }
            )

            command = Transaction.validator_command_of(tx)
            if command is not None:
                slashed = self._apply_validator_command(
                    tx, command, sender_address, block.index, balances, validators
                )
                if slashed is None:
                    logger.warning("Block %s: invalid validator command", block.index)
                    return None
                total_slashed += slashed

            governance = Transaction.governance_command_of(tx)
            if governance is not None:
                if not self._apply_governance_command(tx, governance, validators, parameters):
                    logger.warning("Block %s: invalid governance command", block.index)
                    return None

        # Base fee for the next block (EIP-1559-style adjustment).
        next_fee = next_base_fee(base_fee, total_gas_used, block_gas_limit)

        if block.validator:
            # The reward destination is declared in the signed block header,
            # so every node credits the same address (the proposer's own
            # address when the header leaves it empty).
            reward_address = (
                block.reward_address
                or self._validator_reward_address(block.validator)
            )
            subsidy = int(parameters.get("block_reward", 0))
            balances[reward_address] = (
                balances.get(reward_address, 0) + total_tips + subsidy
            )

        storage_snapshot = storage.to_dict()

        # The header commits to the resulting ledger state; every node can
        # recompute it and reject a block whose state root does not match.
        if block.state_root is not None:
            computed_root = compute_state_root(
                balances,
                nonces,
                validators,
                total_slashed,
                storage_snapshot,
                parameters,
                next_fee,
                total_burned,
            )
            if computed_root != block.state_root:
                logger.warning(
                    "Block %s: state root mismatch (announced %s, computed %s)",
                    block.index,
                    block.state_root,
                    computed_root,
                )
                return None

        if apply:
            self.blockchain.SAN = balances
            self.blockchain.nonces = nonces
            self.storage.load_from_dict(storage_snapshot)
            self.blockchain.validators.clear()
            self.blockchain.validators.update(validators)
            self.blockchain.total_slashed = total_slashed
            self.blockchain.total_burned = total_burned
            self.blockchain.parameters = parameters
            self.blockchain.base_fee = next_fee

        return {
            "balances": balances,
            "nonces": nonces,
            "validators": validators,
            "total_slashed": total_slashed,
            "total_burned": total_burned,
            "parameters": parameters,
            "base_fee": next_fee,
            "storage": storage_snapshot,
            "receipts": receipts,
            "gas_used": total_gas_used,
        }

    # ------------------------------------------------------------------ #
    # Validators, staking and slashing
    # ------------------------------------------------------------------ #

    def _apply_validator_command(
        self,
        tx: dict,
        command: dict,
        sender_address: str,
        block_index: int,
        balances: dict,
        validators: dict,
    ) -> int | None:
        """Apply a staking command. Returns burned units, or None to reject."""
        name = command.get("command")
        info = validators.get(sender_address)

        if name == "deposit":
            amount = int(command["amount"])
            if balances.get(sender_address, 0) < amount:
                logger.warning("Validator deposit exceeds the available balance")
                return None
            balances[sender_address] -= amount
            stake = int(info.get("stake", 0)) if info else 0
            validators[sender_address] = {
                "public_key": tx["sender"],
                "stake": stake + amount,
                "joined_height": int(info.get("joined_height", block_index)) if info else block_index,
                "release_height": None,
            }
            return 0

        if name == "undelegate":
            if info is None or info.get("release_height") is not None:
                return None
            updated = dict(info)
            updated["release_height"] = block_index + self.blockchain.unbonding_period
            validators[sender_address] = updated
            return 0

        if name == "withdraw":
            release = info.get("release_height") if info else None
            if info is None or release is None or block_index < int(release):
                return None
            balances[sender_address] = balances.get(sender_address, 0) + int(
                info.get("stake", 0)
            )
            del validators[sender_address]
            return 0

        if name == "evidence":
            vote_a = command["vote_a"]
            vote_b = command["vote_b"]
            if not self._verify_equivocation(vote_a, vote_b):
                logger.warning("Invalid equivocation evidence")
                return None
            offender = try_address_from_public_key(vote_a.get("public_key"))
            offender_info = validators.get(offender) if offender else None
            if offender is None or offender_info is None:
                return None
            stake = int(offender_info.get("stake", 0))
            burned = stake * self.blockchain.slash_bps // 10_000
            remaining = stake - burned
            balances[offender] = balances.get(offender, 0) + remaining
            del validators[offender]
            self.metrics["slashing_events"] += 1
            logger.warning(
                "Slashed %s: %d units burned, %d returned",
                offender,
                burned,
                remaining,
            )
            return burned

        return None

    def _vote_payload(self, vote: dict) -> bytes:
        payload = {key: value for key, value in vote.items() if key != "signature"}
        return canonical.dumps_bytes(payload)

    def _verify_equivocation(self, vote_a: dict, vote_b: dict) -> bool:
        """Two signed votes by one validator for different hashes at one height."""
        try:
            public_key = vote_a["public_key"]
            signature_a = vote_a["signature"]
            signature_b = vote_b["signature"]
        except (KeyError, TypeError):
            return False

        if vote_b.get("public_key") != public_key:
            return False
        for vote in (vote_a, vote_b):
            if vote.get("chain_id") != self.chain_id:
                return False
            if not isinstance(vote.get("height"), int):
                return False
        if vote_a.get("height") != vote_b.get("height"):
            return False
        if not vote_a.get("block_hash") or not vote_b.get("block_hash"):
            return False
        if vote_a.get("block_hash") == vote_b.get("block_hash"):
            return False

        if not NodeIdentity.verify(self._vote_payload(vote_a), signature_a, public_key):
            return False
        if not NodeIdentity.verify(self._vote_payload(vote_b), signature_b, public_key):
            return False
        return True

    # ------------------------------------------------------------------ #
    # Finality (2/3 of the active stake)
    # ------------------------------------------------------------------ #

    async def _maybe_vote(self, block: Block) -> None:
        """If this node is an active validator, vote for the new block.

        The vote is processed locally (so a single-validator network still
        finalizes) and gossiped to peers.
        """
        public_key = self.get_public_key()
        address = try_address_from_public_key(public_key) if public_key else None
        if address is None:
            return
        if address not in self.blockchain.active_validators():
            logger.debug("Not voting for block %s: not an active validator", block.index)
            return

        vote = {
            "chain_id": self.chain_id,
            "public_key": public_key,
            "height": block.index,
            "block_hash": block.current_block_hash,
            "timestamp": time.time(),
        }
        vote["signature"] = self.identity.sign_hex(self._vote_payload(vote))
        await self._handle_finality_vote(vote)

    async def _flush_pending_votes(self) -> None:
        """Vote for every committed height we have not voted on yet."""
        while self._pending_votes:
            height = min(self._pending_votes)
            if height > self.blockchain.tip.index:
                break
            self._pending_votes.discard(height)
            block = self.block_at(height)
            if block is None:
                continue
            await self._maybe_vote(block)

    async def _rebroadcast_own_votes(self) -> None:
        """Re-send our own votes for heights that are not finalized yet."""
        if not self.PEERS:
            return
        address = try_address_from_public_key(self.get_public_key())
        if address is None:
            return
        # Keep re-broadcasting for a trailing window even after *we* finalized
        # a height: other nodes may not have received our vote yet. The window
        # is bounded on both ends.
        window_start = max(self.finalized_height - 8, self.blockchain.tip.index - 64, 0)
        for height in sorted(self._finality_votes):
            if height <= window_start or height > self.blockchain.tip.index:
                continue
            for block_hash, voters in self._finality_votes[height].items():
                vote = voters.get(address)
                if vote is None:
                    continue
                message = json.dumps({"type": "FINALITY_VOTE", "vote": vote})
                for peer in list(self.PEERS):
                    await self._send_to_peer(peer, "peer_port", message)

    async def _broadcast_vote(self, vote: dict) -> None:
        key = f"{vote.get('height')}:{vote.get('block_hash')}:{vote.get('public_key')}"
        if key in self._seen_votes:
            return
        self._seen_votes.add(key)
        if len(self._seen_votes) > 8192:
            self._seen_votes = {key}

        if not self.PEERS:
            self.metrics["votes_unbroadcast"] += 1
            return
        message = json.dumps({"type": "FINALITY_VOTE", "vote": vote})
        delivered = False
        for peer in list(self.PEERS):
            delivered = await self._send_to_peer(peer, "peer_port", message) or delivered
        if delivered:
            self.metrics["votes_broadcast"] += 1
        else:
            self.metrics["votes_unbroadcast"] += 1

    async def _handle_finality_vote(self, vote) -> None:
        """Verify, store and gossip a finality vote; finalize at 2/3 stake."""
        if not isinstance(vote, dict):
            self.metrics["votes_dropped_invalid"] += 1
            return
        try:
            public_key = vote["public_key"]
            signature = vote["signature"]
            height = vote["height"]
            block_hash = vote["block_hash"]
        except (KeyError, TypeError):
            self.metrics["votes_dropped_invalid"] += 1
            return

        if vote.get("chain_id") != self.chain_id:
            self.metrics["votes_dropped_invalid"] += 1
            return
        if len(canonical.dumps_bytes(vote)) > VOTE_MAX_BYTES:
            self.metrics["votes_dropped_invalid"] += 1
            return
        if not isinstance(height, int) or height < 0 or not isinstance(block_hash, str):
            self.metrics["votes_dropped_invalid"] += 1
            return
        if not block_hash or not NodeIdentity.verify(self._vote_payload(vote), signature, public_key):
            self.metrics["votes_dropped_invalid"] += 1
            return

        weights = self._finality_sets.get(height)
        address = try_address_from_public_key(public_key)
        self.metrics["votes_seen"] += 1
        if weights is None:
            tip_index = self.blockchain.tip.index
            if height <= tip_index or height > tip_index + VOTE_LOOKAHEAD:
                # Unknown height at or below the tip, or implausibly far ahead
                # (audit fix: future votes must be bounded).
                self.metrics["votes_dropped_height"] += 1
                logger.debug(
                    "Finality vote for height %s ignored (no weight snapshot)", height
                )
                return
            # The block has not reached us yet: keep the vote so the tally can
            # complete as soon as it is committed (gossip may overtake blocks).
            # Votes are always keyed by address, never by raw public key, and
            # conflicting votes still produce equivocation evidence.
            if address is None:
                return
            height_votes = self._finality_votes.get(height, {})
            for other_hash, voters in height_votes.items():
                if other_hash != block_hash and address in voters:
                    self.metrics["votes_dropped_equivocation"] += 1
                    self._record_evidence(address, height, voters[address], vote)
                    return
            staged = height_votes.get(block_hash)
            if staged is None:
                # New hash: check the caps before inserting anything, so no
                # empty marker entries can accumulate.
                if (
                    len(height_votes) >= 8
                    or self._staged_hash_count() >= VOTE_LOOKAHEAD * 4
                ):
                    self.metrics["votes_dropped_height"] += 1
                    return
                staged = {}
                self._finality_votes.setdefault(height, {})[block_hash] = staged
            if address in staged or len(staged) >= MAX_STAGED_VOTERS_PER_HASH:
                self.metrics["votes_dropped_height"] += 1
                return
            staged[address] = vote
            await self._broadcast_vote(vote)
            return
        if address is None or address not in weights:
            self.metrics["votes_dropped_voter"] += 1
            return

        height_votes = self._finality_votes.get(height, {})
        for other_hash, voters in height_votes.items():
            if other_hash != block_hash and address in voters:
                self.metrics["votes_dropped_equivocation"] += 1
                self._record_evidence(address, height, voters[address], vote)
                return

        staged = height_votes.get(block_hash)
        if staged is None:
            if len(height_votes) >= 8 or self._staged_hash_count() >= VOTE_LOOKAHEAD * 4:
                self.metrics["votes_dropped_height"] += 1
                return
            staged = {}
            self._finality_votes.setdefault(height, {})[block_hash] = staged
        if address in staged:
            self.metrics["votes_dropped_duplicate"] += 1
            return
        if len(staged) >= MAX_STAGED_VOTERS_PER_HASH:
            self.metrics["votes_dropped_height"] += 1
            return
        staged[address] = vote
        self.metrics["votes_received"] += 1

        self._tally_finality(height, block_hash)
        await self._broadcast_vote(vote)

    def _staged_hash_count(self) -> int:
        """Total staged block hashes across heights (cheap: at most 64*8)."""
        return sum(len(hashes) for hashes in self._finality_votes.values())

    def _record_evidence(self, address: str, height: int, vote_a: dict, vote_b: dict) -> None:
        evidence = {"offender": address, "height": height, "vote_a": vote_a, "vote_b": vote_b}
        if evidence in self._equivocation_evidence:
            return
        self._equivocation_evidence.append(evidence)
        if len(self._equivocation_evidence) > 256:
            self._equivocation_evidence.pop(0)
        logger.warning("Equivocation detected: %s voted twice at height %d", address, height)

    def _tally_finality(self, height: int, block_hash: str) -> None:
        if height <= self.finalized_height:
            return
        # Weight votes with the set frozen at commit time (audit finding: the
        # mutable registry used to let a shrunken set finalize old blocks).
        active = self._finality_sets.get(height)
        if not active:
            return

        total_stake = sum(active.values())
        voted_stake = sum(
            int(active[address])
            for address in self._finality_votes.get(height, {}).get(block_hash, {})
            if address in active
        )
        if voted_stake * FINALITY_DENOMINATOR < total_stake * FINALITY_NUMERATOR:
            return

        block = self.block_at(height)
        if block is None or block.current_block_hash != block_hash:
            return

        self.finalized_height = height
        self.finalized_hash = block_hash
        self._finality_votes = {
            vote_height: votes
            for vote_height, votes in self._finality_votes.items()
            if vote_height >= height
        }
        if self.store is not None:
            self._persist_finality()
            if self.config.prune_keep > 0:
                cutoff = height - self.config.prune_keep
                # Audit fix: never prune our restart anchor (the newest
                # snapshot), even when snapshot_interval > prune_keep.
                latest = self.store.load_latest_snapshot()
                if latest is None:
                    # No restart anchor exists yet: pruning now would make the
                    # store unloadable (audit finding).
                    cutoff = 0
                elif cutoff > latest["height"]:
                    cutoff = latest["height"]
                if cutoff > 1:
                    pruned = self.store.prune_blocks_below(cutoff)
                    if pruned:
                        logger.info(
                            "Pruned %d block(s) below the finalized checkpoint", pruned
                        )
        logger.info(
            "Block %d finalized with %d/%d voting stake", height, voted_stake, total_stake
        )

    def attestation_state(self) -> dict:
        active = self.blockchain.active_validators()
        return {
            "finalized_height": self.finalized_height,
            "finalized_hash": self.finalized_hash,
            "validators": [
                {
                    "address": address,
                    "stake_units": stake,
                    "stake": str(units_to_san(stake)),
                }
                for address, stake in sorted(active.items())
            ],
            "total_stake_units": sum(active.values()),
            "min_stake_units": self.blockchain.min_validator_stake,
            "unbonding_period": self.blockchain.unbonding_period,
            "slash_bps": self.blockchain.slash_bps,
            "base_fee": self.blockchain.base_fee,
            "total_burned": self.blockchain.total_burned,
            "parameters": dict(self.blockchain.parameters),
        }

    def pending_finality_heights(self) -> list[int]:
        return sorted(self._finality_votes)[-10:]

    def equivocation_evidence(self) -> list[dict]:
        return list(self._equivocation_evidence)

    def _run_tx_execution(self, vm: SANVirtualMachine, storage: Storage, tx: dict) -> dict:
        """Execute a transaction payload and return a receipt-like result.

        Failed executions leave no state behind but still burn the gas escrow,
        so the charged fee always matches the deterministic fee formula.
        """
        gas_limit = Transaction.gas_limit_of(tx)
        snapshot = storage.to_dict()
        try:
            if "bytecode" in tx:
                bytecode = Parser.parse_instruction_list(tx["bytecode"])
                vm.run(bytecode, gas_limit=gas_limit)
                return {
                    "gas_used": vm.gas_used,
                    "status": "success",
                    "error": None,
                    "logs": list(vm.logs),
                }

            contract_code = tx.get("contract_code") or {}
            command = contract_code.get("command")
            if command == "deploy":
                if "pena_code" in contract_code:
                    bytecode = PenaParser().parse(contract_code["pena_code"])
                    vm.deploy_contract(
                        contract_code["contract_id"], bytecode, gas_limit=gas_limit
                    )
                else:
                    vm.deploy_contract(
                        contract_code["contract_id"],
                        contract_code["bytecode"],
                        gas_limit=gas_limit,
                    )
                return {
                    "gas_used": vm.contract_manager.last_gas_used,
                    "status": "success",
                    "error": None,
                    "logs": list(vm.contract_manager.last_logs),
                }

            if command == "run":
                vm.call_contract_function(
                    contract_code["contract_id"],
                    contract_code["function_name"],
                    contract_code.get("params", []),
                    gas_limit=gas_limit,
                )
                return {
                    "gas_used": vm.contract_manager.last_gas_used,
                    "status": "success",
                    "error": None,
                    "logs": list(vm.contract_manager.last_logs),
                }

            raise VMError(f"Unknown contract command: {command!r}")
        except Exception as exc:  # noqa: BLE001 - audit fix: every deterministic
            # execution failure burns the gas escrow instead of poisoning the
            # mempool with a transaction that crashes block production.
            logger.warning("Transaction execution failed (%s); gas escrow burned", exc)
            storage.load_from_dict(snapshot)
            return {
                "gas_used": gas_limit,
                "status": "failed",
                "error": str(exc),
                "logs": [],
            }

    # ------------------------------------------------------------------ #
    # Governance (validator-majority parameter changes)
    # ------------------------------------------------------------------ #

    def _apply_governance_command(
        self, tx: dict, command: dict, validators: dict, parameters: dict
    ) -> bool:
        """Apply a parameter change approved by 2/3 of the active stake."""
        name = command.get("name")
        value = command.get("value")
        if isinstance(value, bool) or not isinstance(value, int):
            logger.warning("Invalid parameter value: %r", value)
            return False
        if not isinstance(name, str) or not governance_value_ok(name, value):
            logger.warning("Invalid parameter change: %s=%r", name, value)
            return False

        min_stake = int(parameters.get("min_validator_stake", 0))
        active = {
            address: int(info.get("stake", 0))
            for address, info in validators.items()
            if info.get("release_height") is None
            and int(info.get("stake", 0)) >= min_stake
        }
        if not active:
            logger.warning("Governance rejected: no active validators")
            return False

        message = canonical.dumps_bytes(
            {
                "chain_id": self.chain_id,
                "command": "set_param",
                "name": name,
                "value": value,
                "tx_sender": tx.get("sender"),
                "tx_nonce": tx.get("nonce"),
            }
        )
        approved_stake = 0
        seen: set[str] = set()
        for approval in command.get("approvals") or []:
            if not isinstance(approval, dict):
                return False
            public_key = approval.get("public_key")
            signature = approval.get("signature")
            if not public_key or not signature:
                return False
            address = try_address_from_public_key(public_key)
            if address is None or address in seen or address not in active:
                return False
            if not NodeIdentity.verify(message, signature, public_key):
                return False
            seen.add(address)
            approved_stake += active[address]

        total_stake = sum(active.values())
        if approved_stake * FINALITY_DENOMINATOR < total_stake * FINALITY_NUMERATOR:
            logger.warning(
                "Governance rejected: %d/%d stake approved", approved_stake, total_stake
            )
            return False

        parameters[name] = int(value)
        self.metrics["governance_changes"] += 1
        logger.info("Parameter %s set to %s by validator majority", name, value)
        return True

    @staticmethod
    def _validator_reward_address(validator: str) -> str:
        """Validator rewards go to the address derived from its public key.

        Falls back to the raw value for unsigned development blocks.
        """
        derived = try_address_from_public_key(validator)
        return derived if derived is not None else validator

    def _commit_block(self, block: Block, *, historical: bool = False) -> bool:
        """Validate and apply a block atomically, execution included."""
        if not self.verify_block(block, historical=historical):
            # Audit fix: a locally produced block must pass the same full
            # validation as a received one (signature, state root, round...).
            logger.warning("Block %s rejected by full verification", block.index)
            return False

        # Freeze the voting weights for this height before the block mutates
        # the validator registry.
        self._finality_sets[block.index] = dict(
            self.blockchain.active_validators_for(block.index)
        )
        if len(self._finality_sets) > 4096:
            self._finality_sets = {
                height: weights
                for height, weights in self._finality_sets.items()
                if height > block.index - 4096
            }

        outcome = self._simulate_block(block, apply=True)
        if outcome is None:
            logger.warning("Block %s rejected during execution", block.index)
            return False

        self._receipts[block.index] = outcome["receipts"]
        if len(self._receipts) > 128:
            self._receipts = {
                index: receipts
                for index, receipts in self._receipts.items()
                if index > block.index - 128
            }
        self.metrics["blocks_committed"] += 1
        self.metrics["transactions_committed"] += len(block.transactions)

        self.blockchain.chain.append(block)
        self.last_seen_block_index = block.index
        self._chain_hashes.add(block.current_block_hash)
        if self.blockchain.active_validators():
            self._pending_votes.add(block.index)
        if self._proposer_state and self._proposer_state["height"] <= block.index:
            self._proposer_state = None

        # Votes that arrived before the block itself can now be tallied; votes
        # from addresses outside the frozen set are pruned first.
        frozen = self._finality_sets.get(block.index, {})
        for pending_hash in list(self._finality_votes.get(block.index, {})):
            voters = self._finality_votes[block.index][pending_hash]
            for address in list(voters):
                if address not in frozen:
                    voters.pop(address, None)
            self._tally_finality(block.index, pending_hash)

        self._persist_block(block, outcome["receipts"])
        self._persist_finality()

        if (
            self.store is not None
            and self.config.snapshot_interval > 0
            and block.index > 0
            and block.index % self.config.snapshot_interval == 0
        ):
            try:
                self.store.save_snapshot(
                    block.index,
                    block.current_block_hash,
                    self.blockchain.state_snapshot(),
                    self.storage.to_dict(),
                )
                logger.info("State snapshot saved at height %d", block.index)
            except Exception as exc:  # noqa: BLE001
                logger.error("Snapshot failed: %s", exc)

        self._prune_pool()
        return True

    def _persist_finality(self) -> None:
        """Persist the finality checkpoint and recent voting bookkeeping.

        Audit fix: frozen voting sets and in-flight finality votes were only
        kept in memory, so a restart could finalize a block with different
        weights (or lose an almost-complete quorum).
        """
        if self.store is None:
            return
        keep_from = self.blockchain.tip.index - 256
        state = {
            "finalized_height": self.finalized_height,
            "finalized_hash": self.finalized_hash,
            "sets": {
                str(height): weights
                for height, weights in self._finality_sets.items()
                if height >= keep_from
            },
            "votes": {
                str(height): votes
                for height, votes in self._finality_votes.items()
                if height >= keep_from
            },
        }
        try:
            self.store.set_meta("finality_state", json.dumps(state))
            self.store.set_meta("finalized_height", str(self.finalized_height))
            if self.finalized_hash:
                self.store.set_meta("finalized_hash", self.finalized_hash)
        except Exception as exc:  # noqa: BLE001 - never break consensus on IO
            logger.error("Finality persistence failed: %s", exc)

    def _require_loaded_state_roots(self, chain: list[Block]) -> None:
        """Refuse a persisted window that skips state commitments (audit fix)."""
        if not self.config.require_state_root:
            return
        for block in chain:
            if block.index > 0 and block.state_root is None:
                raise RuntimeError(
                    f"Persisted block {block.index} has no state root; refusing to start"
                )

    def _load_finality_state(self, store: ChainStore) -> bool:
        """Restore frozen voting sets and pending votes; returns True if used."""
        raw = store.get_meta("finality_state")
        if not raw:
            return False
        try:
            state = json.loads(raw)
            if not isinstance(state, dict):
                raise TypeError(f"expected an object, got {type(state).__name__}")
            self._finality_sets = {
                int(height): dict(weights)
                for height, weights in (state.get("sets") or {}).items()
            }
            self._finality_votes = {
                int(height): votes for height, votes in (state.get("votes") or {}).items()
            }
            # Checkpoint fallback: honor the values stored beside the sets so a
            # missing meta key cannot silently unfinalize history.
            height = state.get("finalized_height")
            block_hash = state.get("finalized_hash")
            if (
                isinstance(height, int)
                and height > self.finalized_height
                and isinstance(block_hash, str)
                and block_hash
            ):
                block = self.block_at(height)
                if block is not None and block.current_block_hash == block_hash:
                    self.finalized_height = height
                    self.finalized_hash = block_hash
        except (ValueError, TypeError, AttributeError) as exc:
            logger.warning("Ignoring unreadable persisted finality state: %s", exc)
            return False
        return True

    def _persist_block(self, block: Block, receipts: list) -> None:
        """Atomic block commit: header, body, indexes, receipts and state.

        A crash can never leave a half-written block behind, and the ledger
        state that goes with a block is committed in the same batch.
        """
        if self.store is None:
            return
        try:
            self.store.append_block(
                block,
                receipts=receipts,
                state=self.blockchain.state_snapshot(),
                storage=self.storage.to_dict(),
                head=block.index,
            )
            self.store.set_meta(
                "genesis_allocation", self._genesis_allocation_fingerprint()
            )
        except Exception as exc:  # noqa: BLE001 - never break consensus on IO
            logger.error("Persistence failed for block %s: %s", block.index, exc)

    # ------------------------------------------------------------------ #
    # Fork handling (longest valid chain wins)
    # ------------------------------------------------------------------ #

    def _verify_fork_block(self, block: Block) -> bool:
        """Cheap checks for a block whose branch state is not known yet."""
        if block.current_block_hash != block.calculate_hash():
            return False
        if not self._verify_block_signature(block):
            return False
        for tx in block.transactions:
            if not isinstance(tx, dict) or not Transaction.verify_transaction(tx):
                return False
        return True

    def _buffer_orphan(self, block: Block) -> None:
        self._orphans[block.current_block_hash] = block
        while len(self._orphans) > self.config.max_orphans:
            oldest = min(self._orphans.values(), key=lambda candidate: candidate.index)
            self._orphans.pop(oldest.current_block_hash, None)
            if oldest.current_block_hash == block.current_block_hash:
                break

    def _connect_orphans(self) -> int:
        """Extend the chain with buffered blocks whose parent is the tip."""
        connected = 0
        progressed = True
        while progressed:
            progressed = False
            for block_hash, block in list(self._orphans.items()):
                if block.previous_block_hash != self.blockchain.tip.current_block_hash:
                    continue
                self._orphans.pop(block_hash, None)
                if not self.verify_block(block) or not self._commit_block(block):
                    logger.warning("Orphan block %s failed to connect", block.index)
                    continue
                connected += 1
                progressed = True
        return connected

    def _process_incoming_block(self, block: Block) -> bool:
        """Accept an extension, or buffer a competing branch for reorg."""
        if block.current_block_hash in self._chain_hashes:
            return False

        tip = self.blockchain.tip
        if block.previous_block_hash == tip.current_block_hash:
            if not self.verify_block(block):
                logger.warning("Block %s failed verification; ignored", block.index)
                return False
            self._commit_block(block)
            self._connect_orphans()
            return True

        if (
            block.previous_block_hash in self._chain_hashes
            or block.previous_block_hash in self._orphans
        ):
            if block.index <= tip.index - self.config.max_reorg_depth:
                logger.warning(
                    "Ignoring fork block %s beyond the reorg depth", block.index
                )
                return False
            if not self._verify_fork_block(block):
                logger.warning("Fork block %s failed signature checks", block.index)
                return False
            self._buffer_orphan(block)
            self._connect_orphans()
            if self._orphans:
                self._try_reorg()
            return True

        # Unknown parent: keep it for a bounded window so out-of-order gossip
        # can still assemble a branch once the missing ancestor arrives.
        if block.index > tip.index + self.config.max_orphans:
            logger.debug("Ignoring far-ahead block %s", block.index)
            return False
        if block.index <= tip.index - self.config.max_reorg_depth:
            logger.debug("Ignoring old orphan block %s", block.index)
            return False
        if not self._verify_fork_block(block):
            logger.warning("Orphan block %s failed signature checks", block.index)
            return False
        self._buffer_orphan(block)
        self._connect_orphans()
        if self._orphans:
            self._try_reorg()
        return True

    def _best_orphan_chain(self) -> list[Block] | None:
        """Build the longest candidate chain that connects to a known ancestor."""
        if not self._orphans:
            return None

        chain = self.blockchain.chain
        chain_index = {block.current_block_hash: i for i, block in enumerate(chain)}
        best: list[Block] | None = None

        for orphan in self._orphans.values():
            branch: list[Block] = [orphan]
            cursor = orphan
            seen = {orphan.current_block_hash}
            while True:
                parent_hash = cursor.previous_block_hash
                if parent_hash in chain_index:
                    fork_index = chain_index[parent_hash]
                    candidate = chain[: fork_index + 1] + list(reversed(branch))
                    if best is None or len(candidate) > len(best):
                        best = candidate
                    break

                parent_block = self._orphans.get(parent_hash)
                if parent_block is None or parent_block.current_block_hash in seen:
                    break
                seen.add(parent_block.current_block_hash)
                branch.append(parent_block)
                cursor = parent_block

        return best

    def _capture_state(self) -> dict:
        """In-memory snapshot used to roll back a failed reorg replay."""
        return {
            "chain": list(self.blockchain.chain),
            "balances": dict(self.blockchain.SAN),
            "nonces": dict(self.blockchain.nonces),
            "validators": copy.deepcopy(self.blockchain.validators),
            "total_slashed": self.blockchain.total_slashed,
            "total_burned": self.blockchain.total_burned,
            "parameters": dict(self.blockchain.parameters),
            "base_fee": self.blockchain.base_fee,
            "storage": self.storage.to_dict(),
            "chain_hashes": set(self._chain_hashes),
            "finality_sets": dict(self._finality_sets),
            "finality_votes": copy.deepcopy(self._finality_votes),
            "receipts": dict(self._receipts),
        }

    def _restore_state(self, snapshot: dict) -> None:
        self.blockchain.chain = list(snapshot["chain"])
        self.blockchain.SAN = dict(snapshot["balances"])
        self.blockchain.nonces = dict(snapshot["nonces"])
        self.blockchain.validators = copy.deepcopy(snapshot["validators"])
        self.blockchain.total_slashed = snapshot["total_slashed"]
        self.blockchain.total_burned = snapshot["total_burned"]
        self.blockchain.parameters = dict(snapshot["parameters"])
        self.blockchain.base_fee = snapshot["base_fee"]
        self.storage.load_from_dict(snapshot["storage"])
        self._chain_hashes = set(snapshot["chain_hashes"])
        self._finality_sets = dict(snapshot["finality_sets"])
        self._finality_votes = copy.deepcopy(snapshot["finality_votes"])
        self._receipts = dict(snapshot["receipts"])
        self.last_seen_block_index = self.blockchain.tip.index

    def _replay_chain(self, chain: list[Block], *, historical: bool = False) -> bool:
        """Rebuild ledger state from a candidate chain, in memory only.

        Audit fix: the candidate is applied entirely in memory and then written
        to the database in one atomic ``replace_chain`` batch, so a crash can
        never leave a half-replayed reorg on disk.

        ``historical`` is only True for replaying our own persisted window on
        startup. A live fork candidate is validated with live rules (its own
        timestamps never classify it as history); only blocks at or below the
        finalized checkpoint count as trusted history.
        """
        anchor = self._anchor_state or self._genesis_anchor_state()
        self.blockchain.chain = [chain[0]]
        self.blockchain.SAN = dict(anchor["balances"])
        self.blockchain.nonces = dict(anchor["nonces"])
        self.blockchain.validators = {
            address: dict(info) for address, info in anchor["validators"].items()
        }
        self.blockchain.total_slashed = int(anchor["total_slashed"])
        self.blockchain.total_burned = int(anchor["total_burned"])
        self.blockchain.parameters = dict(anchor["parameters"])
        self.blockchain.base_fee = int(anchor["base_fee"])
        self.storage.load_from_dict(anchor.get("storage") or {})
        self._chain_hashes = {chain[0].current_block_hash}

        for block in chain[1:]:
            self._finality_sets[block.index] = dict(
                self.blockchain.active_validators_for(block.index)
            )
            # History is decided by the trusted finalized checkpoint, never
            # by the block's own timestamp.
            block_historical = historical or block.index <= self.finalized_height
            if not self.verify_block(block, historical=block_historical):
                return False
            outcome = self._simulate_block(block, apply=True)
            if outcome is None:
                return False
            self.blockchain.chain.append(block)
            self._chain_hashes.add(block.current_block_hash)
            self._receipts[block.index] = outcome["receipts"]

        self.last_seen_block_index = self.blockchain.tip.index
        if self.store is not None:
            self.store.replace_chain(
                self.blockchain.chain,
                state=self.blockchain.state_snapshot(),
                storage=self.storage.to_dict(),
                receipts_by_index=dict(self._receipts),
            )
            self.store.delete_mismatched_snapshots(self.block_hash_at)
        return True

    def _requeue_transactions(self, old_chain: list[Block], new_chain: list[Block]) -> None:
        """Put transactions from reorged-out blocks back into the mempool."""
        new_hashes = {block.current_block_hash for block in new_chain}
        rate = self.blockchain.next_fee_rate()

        for block in old_chain:
            if block.current_block_hash in new_hashes:
                continue
            for tx_dict in block.transactions:
                if not isinstance(tx_dict, dict):
                    continue
                try:
                    transaction = Transaction(tx_dict, rate)
                    self._validate_transaction_for_mempool(transaction)
                except (TypeError, ValueError):
                    continue
                if transaction.tx_id in self._pool_tx_ids:
                    continue
                self.transaction_pool.append(transaction)
                self._pool_tx_ids.add(transaction.tx_id)

        self._prune_pool()

    def _try_reorg(self) -> bool:
        candidate = self._best_orphan_chain()
        if candidate is None or len(candidate) <= len(self.blockchain.chain):
            return False

        # Finalized history can never be rewritten.
        if self.finalized_height > 0:
            if len(candidate) <= self.finalized_height:
                logger.warning("Rejected reorg shorter than finalized history")
                return False
            if (
                candidate[self.finalized_height].current_block_hash
                != self.finalized_hash
            ):
                logger.warning("Rejected reorg that would rewrite finalized history")
                return False

        old_chain = list(self.blockchain.chain)
        snapshot = self._capture_state()

        if not self._replay_chain(candidate):
            logger.error("Reorg candidate failed during replay; restoring old chain")
            self._restore_state(snapshot)
            return False

        self.metrics["reorgs"] += 1
        self._requeue_transactions(old_chain, candidate)
        if self.store is not None:
            self.store.delete_mismatched_snapshots(self.block_hash_at)
        self._persist_finality()
        self._orphans = {
            block_hash: block
            for block_hash, block in self._orphans.items()
            if block_hash not in self._chain_hashes
        }
        logger.warning(
            "Reorg: switched to a %d-block chain (tip index %d, hash %s)",
            len(candidate),
            self.blockchain.tip.index,
            self.blockchain.tip.current_block_hash,
        )
        return True

    # ------------------------------------------------------------------ #
    # Consensus
    # ------------------------------------------------------------------ #

    async def send_to_controllers(self, block: Block) -> bool:
        controllers = list(self.controller_nodes)
        if not controllers:
            logger.warning("No controller nodes; accepting block %s locally", block.index)
            return True

        approvals = 0
        for controller in controllers:
            if await self._request_block_vote(controller, block):
                approvals += 1

        ratio = approvals / len(controllers)
        logger.info(
            "Block %s controller approval: %d/%d (%.2f)",
            block.index,
            approvals,
            len(controllers),
            ratio,
        )
        if ratio < 0.66:
            return False

        await self.broadcast_block(block)
        return True

    async def _request_block_vote(self, controller: dict, block: Block) -> bool:
        message = json.dumps({"type": "BLOCK_VOTE_REQUEST", "block": block.to_dict()})
        websocket = None
        try:
            websocket = await self._open_peer(controller, "controller_port")
            await websocket.send(message)
            response = await asyncio.wait_for(
                websocket.recv(), timeout=self.config.ws_timeout * 2
            )
            data = json.loads(response)
        except Exception as exc:  # noqa: BLE001 - a missing vote is not an approval
            logger.warning("Controller %s did not vote: %s", peer_label(controller), exc)
            return False
        finally:
            if websocket is not None:
                try:
                    await websocket.close()
                except Exception:  # noqa: BLE001
                    pass

        if data.get("type") != "BLOCK_VOTE_RESPONSE" or data.get("approved") is not True:
            return False
        if data.get("chain_id") != self.chain_id:
            logger.warning("Controller %s voted for another chain", peer_label(controller))
            return False
        if data.get("block_hash") != block.current_block_hash:
            logger.warning("Controller %s voted for a different block", peer_label(controller))
            return False

        signature = data.get("signature")
        public_key = data.get("public_key")
        if not signature or not public_key:
            logger.warning("Controller %s sent an unsigned vote", peer_label(controller))
            return False

        expected_public_key = controller.get("public_key")
        if expected_public_key and expected_public_key != public_key:
            logger.warning(
                "Controller %s vote key does not match its advertised key",
                peer_label(controller),
            )
            return False

        payload = {
            key: value for key, value in data.items() if key not in VOTE_META_FIELDS
        }
        signed_bytes = canonical.dumps_bytes(payload)
        return NodeIdentity.verify(signed_bytes, signature, public_key)

    async def broadcast_block(self, block: Block) -> None:
        """Broadcast a locally produced block to every known peer."""
        await self.gossip_block(block)

    async def gossip_block(self, block: Block) -> None:
        """Gossip a block once; deduplication keeps the network loop-free."""
        block_hash = block.current_block_hash
        if block_hash in self._seen_block_gossip:
            return
        self._seen_block_gossip.add(block_hash)
        if len(self._seen_block_gossip) > 4096:
            self._seen_block_gossip = {block_hash}

        if not self.PEERS:
            logger.info("No peers; block %s not broadcast", block.index)
            return

        message = json.dumps({"type": "BLOCK", "block": block.to_dict()})
        for peer in list(self.PEERS):
            await self._send_to_peer(peer, "p2p_port", message)

    async def gossip_transaction(self, transaction: Transaction) -> None:
        """Announce a transaction to every peer (mempool gossip)."""
        tx_id = transaction.tx_id
        if tx_id in self._seen_tx_gossip:
            return
        self._seen_tx_gossip.add(tx_id)
        if len(self._seen_tx_gossip) > 8192:
            self._seen_tx_gossip = {tx_id}

        if not self.PEERS:
            return
        message = json.dumps({"type": "TX", "tx": transaction.to_dict()})
        for peer in list(self.PEERS):
            await self._send_to_peer(peer, "peer_port", message)

    async def _ingest_transaction(self, payload) -> bool:
        """Validate, pool and gossip a transaction; True when it was new.

        Used for both pushed (`TX`) and pulled (`TXS`) transactions, so a
        restarted node can refill its mempool from a peer.
        """
        if not isinstance(payload, dict):
            return False
        async with self._lock:
            try:
                transaction = Transaction(payload, self.blockchain.next_fee_rate())
                self._validate_transaction_for_mempool(transaction)
            except (TypeError, ValueError) as exc:
                logger.debug("Rejected transaction: %s", exc)
                return False

            if transaction.tx_id in self._pool_tx_ids or transaction.tx_id in self._seen_tx_gossip:
                return False

            self.transaction_pool.append(transaction)
            self._pool_tx_ids.add(transaction.tx_id)
            logger.info("Transaction added to the mempool: %s", transaction.tx_id)

        await self._maybe_produce_from_pool()
        await self.gossip_transaction(transaction)
        return True

    async def _handle_incoming_tx(self, payload) -> None:
        await self._ingest_transaction(payload)

    async def request_mempool(self, peer: dict | None = None) -> int:
        """Ask a peer for its mempool (used after restarts/joins)."""
        peer = peer or self.outgoing_node or (self.PEERS[0] if self.PEERS else None)
        if peer is None:
            return 0

        websocket = None
        try:
            websocket = await self._open_peer(peer, "peer_port")
            await websocket.send(json.dumps({"type": "GET_TXS"}))
            raw = await asyncio.wait_for(
                websocket.recv(), timeout=self.config.ws_timeout * 2
            )
            data = json.loads(raw)
        except Exception as exc:  # noqa: BLE001
            logger.debug("Mempool request to %s failed: %s", peer_label(peer), exc)
            return 0
        finally:
            if websocket is not None:
                try:
                    await websocket.close()
                except Exception:  # noqa: BLE001
                    pass

        if data.get("type") != "TXS":
            return 0
        accepted = 0
        for payload in (data.get("txs") or [])[:64]:
            if await self._ingest_transaction(payload):
                accepted += 1
        if accepted:
            logger.info("Pulled %d transaction(s) from %s", accepted, peer_label(peer))
        return accepted

    def _find_block_by_hash(self, block_hash: str) -> Block | None:
        if block_hash in self._orphans:
            return self._orphans[block_hash]
        if self.store is not None:
            block = self.store.block_by_hash(block_hash)
            if block is not None:
                return block
        if block_hash in self._chain_hashes:
            for block in self.blockchain.chain:
                if block.current_block_hash == block_hash:
                    return block
        return None

    async def _serve_block_request(self, websocket, block_hash) -> None:
        """Answer a GET_BLOCK with the block body when we have it."""
        if not isinstance(block_hash, str) or not block_hash:
            return
        block = self._find_block_by_hash(block_hash)
        if block is None:
            await websocket.send(
                json.dumps({"type": "BLOCK_NOT_FOUND", "block_hash": block_hash})
            )
            return
        await websocket.send(json.dumps({"type": "BLOCK", "block": block.to_dict()}))

    async def _request_block(self, block_hash: str) -> bool:
        """Fetch one missing block (e.g. an orphan's parent) from a peer."""
        if not block_hash or block_hash in self._requested_blocks:
            return False
        if len(self._requested_blocks) > 512:
            self._requested_blocks.clear()
        self._requested_blocks.add(block_hash)

        peer = self.outgoing_node or (self.PEERS[0] if self.PEERS else None)
        if peer is None:
            return False

        websocket = None
        try:
            websocket = await self._open_peer(peer, "p2p_port")
            await websocket.send(json.dumps({"type": "GET_BLOCK", "block_hash": block_hash}))
            raw = await asyncio.wait_for(
                websocket.recv(), timeout=self.config.ws_timeout * 2
            )
            data = json.loads(raw)
            if data.get("type") != "BLOCK":
                return False
            block = Block.from_dict(data["block"])
        except Exception as exc:  # noqa: BLE001
            logger.debug("Block request for %s failed: %s", block_hash[:12], exc)
            return False
        finally:
            if websocket is not None:
                try:
                    await websocket.close()
                except Exception:  # noqa: BLE001
                    pass

        async with self._lock:
            accepted = self._process_incoming_block(block)
        if accepted:
            await self._flush_pending_votes()
        return accepted

    # ------------------------------------------------------------------ #
    # Transactions and block production
    # ------------------------------------------------------------------ #

    async def submit_transaction(self, payload: dict) -> dict:
        """Validate, pool and (when fees allow) commit a transaction."""
        async with self._lock:
            try:
                transaction = Transaction(payload, self.blockchain.next_fee_rate())
            except (TypeError, ValueError) as exc:
                raise ValueError(f"Invalid transaction: {exc}") from exc

            result = await self._admit_transaction(transaction)

        # Announce new transactions to the mempool of every peer.
        await self.gossip_transaction(transaction)
        return result

    def _is_expected_proposer(
        self, height: int | None = None, round_: int | None = None
    ) -> bool:
        """True when this node may propose the block at ``height``/``round_``."""
        active = self.blockchain.active_validators()
        if not active:
            return True  # bootstrap: any node may propose
        if height is None:
            height = self.blockchain.tip.index + 1
        if round_ is None:
            round_ = self._current_round(height)
        public_key = self.get_public_key()
        address = try_address_from_public_key(public_key) if public_key else None
        return address is not None and self.expected_proposer(height, active, round_) == address

    async def _maybe_produce_from_pool(self) -> None:
        """The expected proposer produces a block from the mempool if it can."""
        async with self._lock:
            if not self.transaction_pool:
                # Fee-only chains produce blocks on demand; a chain with a
                # block subsidy keeps producing (empty) blocks so validators
                # earn rewards even without traffic.
                if self.blockchain.block_reward <= 0:
                    return
                tip_age = time.time() - self.blockchain.tip.timestamp
                if tip_age < self.blockchain.proposer_timeout:
                    return
            if (
                self.config.require_block_signature
                and self.identity.private_key is None
            ):
                logger.error("Cannot produce blocks: no signing key configured")
                return

            height = self.blockchain.tip.index + 1
            round_ = self._current_round(height)
            if not self._is_expected_proposer(height, round_):
                return

            if self.transaction_pool:
                # The fee threshold gates user transactions; an empty subsidy
                # block (reward chain) has nothing to gate.
                total_fee = sum(tx.fee for tx in self.transaction_pool)
                if total_fee < san_to_units(self.config.block_threshold_fee):
                    return

            block = self._build_block(round_)
            if not await self.send_to_controllers(block):
                return
            if not self._commit_block(block):
                return
            await self._maybe_vote(block)
            self.transaction_pool.clear()
            self._pool_tx_ids.clear()

        await self.gossip_block(block)

    async def _admit_transaction(self, transaction: Transaction) -> dict:
        """Mempool admission and block production; caller holds the lock."""
        self._validate_transaction_for_mempool(transaction)

        self.transaction_pool.append(transaction)
        self._pool_tx_ids.add(transaction.tx_id)

        self._prune_pool()
        total_fee = sum(tx.fee for tx in self.transaction_pool)
        threshold_units = san_to_units(self.config.block_threshold_fee)
        if total_fee < threshold_units:
            return {
                "status": "pooled",
                "tx_id": transaction.tx_id,
                "fee": transaction.fee,
                "pooled_transactions": len(self.transaction_pool),
                "total_fee": total_fee,
                "threshold": threshold_units,
            }

        if not self.transaction_pool:
            return {
                "status": "pooled",
                "tx_id": transaction.tx_id,
                "fee": transaction.fee,
            }

        # Any node accepts transactions, but only the expected proposer builds
        # the block; everyone else pools it and gossips it on.
        if not self._is_expected_proposer():
            return {
                "status": "pooled",
                "tx_id": transaction.tx_id,
                "fee": transaction.fee,
                "pooled_transactions": len(self.transaction_pool),
                "note": "forwarded to the current proposer",
            }

        block = self._build_block()
        if not await self.send_to_controllers(block):
            # Transient: a controller was unreachable. Keep the transaction in
            # the mempool and retry on the next production attempt.
            return {
                "status": "pooled",
                "tx_id": transaction.tx_id,
                "fee": transaction.fee,
                "pooled_transactions": len(self.transaction_pool),
                "note": "controller quorum not reached; retrying",
            }

        if not self._commit_block(block):
            # The freshly submitted transaction failed state validation (for
            # example a withdraw before the unbonding period). Drop it so it
            # cannot poison every subsequent block.
            self.transaction_pool = [
                tx for tx in self.transaction_pool if tx.tx_id != transaction.tx_id
            ]
            self._pool_tx_ids.discard(transaction.tx_id)
            return {
                "status": "rejected",
                "tx_id": transaction.tx_id,
                "fee": transaction.fee,
                "reason": "block failed state validation",
            }

        await self._maybe_vote(block)
        self.transaction_pool.clear()
        self._pool_tx_ids.clear()
        return {
            "status": "committed",
            "tx_id": transaction.tx_id,
            "fee": transaction.fee,
            "block_index": block.index,
            "block_hash": block.current_block_hash,
        }

    def _validate_transaction_for_mempool(self, transaction: Transaction) -> None:
        payload = transaction.to_dict()

        if Transaction.chain_id_of(payload) != self.chain_id:
            raise ValueError(
                f"transaction chain_id does not match this chain ({self.chain_id})"
            )

        if not Transaction.verify_transaction(payload):
            raise ValueError("invalid transaction signature")

        sender = transaction.sender
        if not isinstance(sender, str) or not sender:
            raise ValueError("transaction is missing 'sender'")

        sender_address = transaction.sender_address
        if sender_address is None:
            raise ValueError("transaction 'sender' is not a valid public key")

        nonce = transaction.nonce
        if not isinstance(nonce, int) or isinstance(nonce, bool) or nonce < 0:
            raise ValueError("transaction is missing a valid integer 'nonce'")

        if transaction.tx_id in self._pool_tx_ids:
            raise ValueError("duplicate transaction")

        expected_nonce = self.blockchain.nonces.get(
            sender_address, 0
        ) + self._pending_count_for(sender_address)
        if nonce != expected_nonce:
            raise ValueError(f"invalid nonce: expected {expected_nonce}, got {nonce}")

        value_units = transaction.value_units()
        if "value" in payload:
            if value_units <= 0:
                raise ValueError("'value' must be a positive amount")
            try:
                normalize_address(payload.get("receiver"))
            except AddressError as exc:
                raise ValueError(f"invalid 'receiver' address: {exc}") from exc

        available = self.blockchain.SAN.get(
            sender_address, 0
        ) - self._pending_spend_for(sender_address)
        if available < value_units + transaction.fee:
            raise ValueError("insufficient balance")

        gas_limit = Transaction.gas_limit_of(payload)
        block_gas_limit = self.blockchain.block_gas_limit
        if gas_limit > block_gas_limit:
            raise ValueError(
                f"gas_limit {gas_limit} exceeds the block gas limit "
                f"{block_gas_limit}"
            )

    def _pending_count_for(self, sender_address: str) -> int:
        return sum(
            1 for tx in self.transaction_pool if tx.sender_address == sender_address
        )

    def _pending_spend_for(self, sender_address: str) -> int:
        spent = 0
        for tx in self.transaction_pool:
            if tx.sender_address != sender_address:
                continue
            try:
                spent += tx.value_units()
            except ValueError:
                continue
            spent += tx.fee
        return spent

    def _prune_pool(self) -> None:
        """Drop transactions that no longer fit the current chain state."""
        rate = self.blockchain.next_fee_rate()
        next_nonce: dict[str, int] = {}
        kept: list[Transaction] = []

        for tx in self.transaction_pool:
            sender_address = tx.sender_address
            if sender_address is None:
                continue
            expected = next_nonce.get(
                sender_address, self.blockchain.nonces.get(sender_address, 0)
            )
            if tx.nonce != expected:
                continue
            if tx.fee != Transaction.expected_fee(tx.to_dict(), rate):
                continue
            next_nonce[sender_address] = expected + 1
            kept.append(tx)

        self.transaction_pool = kept
        self._pool_tx_ids = {tx.tx_id for tx in kept}

    def _build_block(self, round_: int = 0) -> Block:
        tip = self.blockchain.tip
        # A non-zero round must be justified by the block timestamp so every
        # verifier reaches the same conclusion.
        minimum_timestamp = tip.timestamp + self.blockchain.min_block_interval
        if round_ > 0:
            minimum_timestamp = max(
                minimum_timestamp,
                tip.timestamp + round_ * self.blockchain.proposer_timeout * 0.8,
            )
        transactions = []
        total_gas = 0
        block_gas_limit = self.blockchain.block_gas_limit
        for tx in self.transaction_pool:
            gas_limit = Transaction.gas_limit_of(tx.to_dict())
            if total_gas + gas_limit > block_gas_limit:
                continue  # leave it in the pool for the next block
            transactions.append(tx.to_dict())
            total_gas += gas_limit

        # Predict the post-state root by simulating the block on copies.
        block_timestamp = max(time.time(), minimum_timestamp)
        reward_address = self.reward_address or self._validator_reward_address(
            self.get_public_key() or "UNSIGNED_VALIDATOR"
        )
        provisional = Block(
            index=tip.index + 1,
            previous_block_hash=tip.current_block_hash,
            validator=self.get_public_key() or "UNSIGNED_VALIDATOR",
            validator_signature=None,
            transactions=transactions,
            timestamp=block_timestamp,
            chain_id=self.chain_id,
            round=round_,
            reward_address=reward_address,
        )
        outcome = self._simulate_block(provisional, apply=False)
        predicted_root = None
        if outcome is not None:
            predicted_root = compute_state_root(
                outcome["balances"],
                outcome["nonces"],
                outcome["validators"],
                outcome["total_slashed"],
                outcome["storage"],
                outcome["parameters"],
                outcome["base_fee"],
                outcome["total_burned"],
            )

        block = Block(
            index=tip.index + 1,
            previous_block_hash=tip.current_block_hash,
            validator=self.get_public_key() or "UNSIGNED_VALIDATOR",
            validator_signature=None,
            transactions=transactions,
            timestamp=block_timestamp,
            chain_id=self.chain_id,
            state_root=predicted_root,
            round=round_,
            reward_address=reward_address,
        )
        block.validator_signature = self._sign_block_hash(block.current_block_hash)
        return block

    # ------------------------------------------------------------------ #
    # Read-only queries
    # ------------------------------------------------------------------ #

    def get_account(self, address: str) -> dict:
        normalized = normalize_address(address)
        units = self.blockchain.SAN.get(normalized, 0)
        return {
            "address": normalized,
            "balance_units": units,
            "balance": str(units_to_san(units)),
            "nonce": self.blockchain.nonces.get(normalized, 0),
        }

    def get_block(self, index: int) -> dict | None:
        block = self.block_at(index)
        return block.to_dict() if block is not None else None

    def list_contracts(self) -> list[str]:
        return sorted(self.storage.contracts.keys())

    def query_contract(self, contract_id: str, function_name: str, params: list):
        """Execute a contract call read-only, on a sandboxed copy of its state."""
        contract = self.storage.contracts.get(contract_id)
        if contract is None:
            raise ValueError(f"Unknown contract: {contract_id}")

        sandbox = Storage()
        sandbox.contracts[contract_id] = copy.deepcopy(contract)
        vm = SANVirtualMachine(sandbox)
        return vm.call_contract_function(contract_id, function_name, params)

    # ------------------------------------------------------------------ #
    # Light client support
    # ------------------------------------------------------------------ #

    def current_state_root(self) -> str:
        return compute_state_root(
            self.blockchain.SAN,
            self.blockchain.nonces,
            self.blockchain.validators,
            self.blockchain.total_slashed,
            self.storage.to_dict(),
            self.blockchain.parameters,
            self.blockchain.base_fee,
            self.blockchain.total_burned,
        )

    def peer_status(self) -> dict:
        """Public chain status (used by peers to find the longest chain)."""
        return {
            "version": SCHEMA_VERSION,
            "chain_id": self.chain_id,
            "genesis_allocation": self._genesis_allocation_fingerprint(),
            "height": self.blockchain.tip.index,
            "finalized_height": self.finalized_height,
            "tip_hash": self.blockchain.tip.current_block_hash,
        }

    async def _select_sync_peer(self) -> dict | None:
        """Pick the compatible peer with the longest chain (longest-chain rule).

        Peers that report a different chain id or genesis fingerprint are
        ignored, so a hostile network cannot lure us into a different chain.
        """
        peers = list(self.PEERS)[:8]
        if not peers:
            return None
        statuses = await asyncio.gather(
            *(transport.remote_status(self, peer) for peer in peers)
        )
        best: tuple[int, dict] | None = None
        for peer, status in zip(peers, statuses):
            if not status:
                continue
            if status.get("chain_id") != self.chain_id:
                continue
            peer_allocation = status.get("genesis_allocation")
            if peer_allocation and peer_allocation != self._genesis_allocation_fingerprint():
                continue
            height = int(status.get("height", -1))
            if best is None or height > best[0]:
                best = (height, peer)
        if best is None:
            # Status unavailable everywhere: keep the old best-effort order.
            return self.incoming_node or self.PEERS[0]
        height, peer = best
        logger.info(
            "Longest compatible peer: %s at height %d (local %d)",
            peer_label(peer),
            height,
            self.blockchain.tip.index,
        )
        return peer

    def get_headers(self, from_index: int = 0, limit: int = 64) -> list[dict]:
        """Headers only (no transactions) for light clients."""
        return [
            block.to_header_dict()
            for block in self.blockchain.blocks_since(from_index, limit=limit)
        ]

    def get_account_proof(self, address: str) -> dict | None:
        normalized = normalize_address(address)
        proof = state_entry_proof(
            self.blockchain.SAN,
            self.blockchain.nonces,
            self.blockchain.validators,
            self.blockchain.total_slashed,
            self.storage.to_dict(),
            account_key(normalized),
            self.blockchain.parameters,
            self.blockchain.base_fee,
            self.blockchain.total_burned,
        )
        if proof is None:
            return None
        proof["address"] = normalized
        return proof

    def get_transaction_proof(self, block_index: int, tx_index: int) -> dict | None:
        block = self.block_at(block_index)
        if block is None:
            return None
        entries = [block._transaction_to_dict(tx) for tx in block.transactions]
        if not 0 <= tx_index < len(entries):
            return None
        return {
            "block_index": block_index,
            "block_hash": block.current_block_hash,
            "tx_root": block.tx_root,
            "tx_index": tx_index,
            "transaction": entries[tx_index],
            "proof": merkle_proof(entries, tx_index),
        }

    def finalized_snapshot(self) -> dict | None:
        """Latest snapshot that is safe (finalized and on the canonical chain)."""
        if self.store is None:
            return None
        for snapshot in self.store.list_snapshots():
            if snapshot["height"] > self.finalized_height:
                continue
            block = self.block_at(snapshot["height"])
            if block is None or block.current_block_hash != snapshot["hash"]:
                # Audit finding: a snapshot from a discarded branch must never
                # be served.
                continue
            break
        else:
            return None
        return {
            "chain_id": self.chain_id,
            "height": snapshot["height"],
            "block_hash": snapshot["hash"],
            "state_root": block.state_root,
            "state": snapshot["state"],
            "storage": snapshot["storage"],
        }

    def get_receipt(self, block_index: int, tx_index: int) -> dict | None:
        """Execution receipt (status, gas, logs) of a recent block."""
        receipts = self._receipts.get(block_index)
        if receipts is None and self.store is not None:
            receipts = self.store.receipts_for_block(block_index)
        if not receipts or not 0 <= tx_index < len(receipts):
            return None
        block = self.block_at(block_index)
        return {
            "block_index": block_index,
            "block_hash": block.current_block_hash if block else None,
            **receipts[tx_index],
        }

    def get_transaction(self, tx_id: str) -> dict | None:
        """Transaction lookup through the persisted tx index."""
        if self.store is not None:
            location = self.store.tx_lookup(tx_id)
            if location is None:
                return None
            block = self.block_at(location["block_index"])
            if block is None:
                return None
            # Audit finding: after a reorg a stale index entry could point at a
            # different block/transaction at the same height.
            if location.get("block_hash") != block.current_block_hash:
                logger.debug("Stale tx index entry for %s", tx_id)
                return None
            tx_index = int(location.get("tx_index") or 0)
            if not 0 <= tx_index < len(block.transactions):
                return None
            tx_dict = block._transaction_to_dict(block.transactions[tx_index])
            if transaction_id(tx_dict) != tx_id:
                logger.debug("Tx index entry does not match the transaction: %s", tx_id)
                return None
            return {
                "tx_id": tx_id,
                "block_index": block.index,
                "block_hash": block.current_block_hash,
                "tx_index": tx_index,
                "transaction": tx_dict,
                "receipt": self.get_receipt(block.index, tx_index),
            }

        # No database: fall back to scanning the in-memory chain.
        for block in reversed(self.blockchain.chain):
            for index, tx in enumerate(block.transactions):
                tx_dict = block._transaction_to_dict(tx)
                if transaction_id(tx_dict) == tx_id:
                    return {
                        "tx_id": tx_id,
                        "block_index": block.index,
                        "block_hash": block.current_block_hash,
                        "tx_index": index,
                        "transaction": tx_dict,
                        "receipt": self.get_receipt(block.index, index),
                    }
        return None

    def metrics_snapshot(self) -> dict:
        """Counters and gauges exported through /metrics."""
        active = self.blockchain.active_validators()
        return {
            **self.metrics,
            "height": self.blockchain.tip.index,
            "finalized_height": self.finalized_height,
            "peers": len(self.PEERS),
            "controllers": len(self.controller_nodes),
            "mempool": len(self.transaction_pool),
            "validators": len(active),
            "total_stake_units": sum(active.values()),
            "total_slashed": self.blockchain.total_slashed,
            "total_burned": self.blockchain.total_burned,
            "base_fee": self.blockchain.base_fee,
            "contracts": len(self.storage.contracts),
            "orphans": len(self._orphans),
        }

    # ------------------------------------------------------------------ #
    # Synchronization
    # ------------------------------------------------------------------ #

    def get_sync_payload(self, from_index: int = 0, limit: int = 128) -> dict:
        """Server side of /sync: one page of blocks plus a state snapshot.

        Paging keeps sync payloads bounded for long chains; clients follow
        ``has_more`` / ``next_from_index`` until they catch up.
        """
        page = min(max(int(limit), 1), 512)
        blocks = self.blockchain.blocks_since(from_index, limit=page + 1)
        payload = {
            "version": SCHEMA_VERSION,
            "chain_id": self.chain_id,
            "genesis_allocation": self._genesis_allocation_fingerprint(),
            "blocks": [],
            "storage": None,
            "state": None,
            "has_more": False,
        }
        if not blocks:
            return payload

        batch = blocks[:page]
        payload["blocks"] = [block.to_dict() for block in batch]
        payload["has_more"] = len(blocks) > page
        if payload["has_more"]:
            payload["next_from_index"] = batch[-1].index + 1
        if int(from_index) <= 0:
            # Only the first page carries the snapshot; later pages are just
            # blocks (the client derives state by replaying them).
            payload["storage"] = self.storage.to_dict()
            payload["state"] = self.blockchain.state_snapshot()
        return payload

    async def synchronize(self) -> bool:
        """Pull missing blocks from a peer and verify them one by one.

        Ledger state, nonces and contract storage are **derived by replaying the
        verified blocks**, never adopted from the peer's snapshot, so a hostile
        peer cannot inject state (it can at most withhold blocks).
        """
        if not self.PEERS:
            return False

        peer = await self._select_sync_peer()
        if peer is None:
            return False
        batch_limit = max(int(self.config.sync_batch_size), 1)
        max_blocks = max(int(self.config.sync_max_blocks), batch_limit)

        applied = 0
        from_index = self.blockchain.tip.index + 1

        while applied < max_blocks:
            try:
                payload = await transport.remote_sync(
                    self, peer, from_index, batch_limit
                )
            except Exception as exc:  # noqa: BLE001
                logger.warning("Sync with %s failed: %s", peer_label(peer), exc)
                return applied > 0

            peer_allocation = payload.get("genesis_allocation")
            if peer_allocation and peer_allocation != self._genesis_allocation_fingerprint():
                logger.warning(
                    "Sync: peer %s uses a different genesis allocation; refusing to sync",
                    peer_label(peer),
                )
                return False

            peer_chain = payload.get("chain_id")
            if peer_chain and peer_chain != self.chain_id:
                logger.warning(
                    "Sync: peer %s is on chain %r while this node is on %r",
                    peer_label(peer),
                    peer_chain,
                    self.chain_id,
                )
                return False

            version = payload.get("version", SCHEMA_VERSION)
            try:
                version = int(version)
            except (TypeError, ValueError):
                logger.warning("Sync: invalid schema version %r from %s", version, peer_label(peer))
                return applied > 0
            if version > SCHEMA_VERSION:
                logger.warning(
                    "Sync: peer %s uses schema %s but this node supports %s",
                    peer_label(peer),
                    version,
                    SCHEMA_VERSION,
                )
                return applied > 0

            batch = payload.get("blocks") or []
            if not batch:
                break

            stop = False
            async with self._lock:
                for raw_block in batch:
                    try:
                        block = Block.from_dict(raw_block)
                    except Exception as exc:  # noqa: BLE001
                        logger.warning("Sync: invalid block from %s: %s", peer_label(peer), exc)
                        stop = True
                        break

                    # A block synced while catching up was produced while we
                    # were offline: it must be verifiable without the live
                    # wall-clock window. When our own tip is fresh, the incoming
                    # block is a live tip extension and must satisfy live rules
                    # (the trusted anchor is our tip's freshness, never the
                    # block's self-reported timestamp).
                    tip_fresh = (
                        self.blockchain.tip.timestamp > time.time() - BLOCK_PAST_DRIFT
                    )
                    historical = not (
                        tip_fresh and block.index == self.blockchain.tip.index + 1
                    )
                    if not self.verify_block(block, historical=historical):
                        logger.warning(
                            "Sync: block %s from %s failed verification; stopping",
                            block.index,
                            peer_label(peer),
                        )
                        stop = True
                        break

                    if not self._commit_block(block, historical=historical):
                        stop = True
                        break
                    applied += 1
                    from_index = block.index + 1

            if stop or not payload.get("has_more"):
                break

        if applied:
            self._connect_orphans()
            await self._flush_pending_votes()
            self.last_seen_block_index = self.blockchain.tip.index
            logger.info("Synced %d block(s) from %s", applied, peer_label(peer))

        return applied > 0

    # ------------------------------------------------------------------ #
    # Identity, signing and votes
    # ------------------------------------------------------------------ #

    def _sign_block_hash(self, block_hash: str):
        signature = self.identity.sign_hex(block_hash.encode("utf-8"))
        if signature is None:
            logger.warning("No private key configured; producing an unsigned block")
        return signature

    def _verify_block_signature(self, block: Block) -> bool:
        if not block.validator_signature:
            return not self.config.require_block_signature
        if not block.validator:
            return False
        return NodeIdentity.verify(
            block.current_block_hash.encode("utf-8"),
            block.validator_signature,
            block.validator,
        )

    def _sign_vote(self, response: dict):
        payload = {
            key: value for key, value in response.items() if key not in VOTE_META_FIELDS
        }
        return self.identity.sign_hex(canonical.dumps_bytes(payload))

    def get_public_key(self):
        """Return the node public key as hex, or None when not configured."""
        return self.identity.public_key_hex
