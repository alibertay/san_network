# Gossip and Peer-to-Peer Networking

SAN Network nodes talk to each other over **gRPC**. One service
(`network/proto/p2p.proto`) is bound to three ports (peer, p2p, controller);
inside the `Session` stream every message is a JSON object with a `type`
field, so the port number only hints at intent and the message type decides
how a session is handled. The REST API is for users and clients only.

This document is based on the Go node (`internal/netnode/node_peers.go`,
`node_sync.go`, `transport.go`, `config.go`, `node.go`,
`internal/api/server.go`) and the Python reference (`network/Node.py`,
`network/transport.py`). Go/Python differences are collected in
[Status / limitations](#status--limitations).

Contents:

1. [Transport and RPCs](#1-transport-and-rpcs)
2. [Peer records and discovery](#2-peer-records-and-discovery)
3. [Peer health](#3-peer-health)
4. [Peer and controller selection](#4-peer-and-controller-selection)
5. [Session message types](#5-session-message-types)
6. [Propagation](#6-propagation)
7. [Chain sync](#7-chain-sync)
8. [Hardening](#8-hardening)
9. [Constants](#9-constants)
10. [Status / limitations](#status--limitations)

---

## 1. Transport and RPCs

`network/proto/p2p.proto` defines the service:

| RPC | Kind | Purpose |
|-----|------|---------|
| `Session` | bidirectional stream of `Envelope` | handshake, gossip, block/vote exchange, controller quorum, PING/PONG |
| `Sync` | unary (`SyncRequest{from_index, limit}` -> `SyncResponse{payload}`) | one page of blocks plus a snapshot on the first page |
| `Bootstrap` | unary (`Empty` -> `Envelope`) | the seed's peer list **plus the seed's own signed self record** (same as REST `GET /bootstrap`) |
| `Status` | unary (`Empty` -> `Envelope`) | chain id, genesis fingerprint, height, finality |

Implementation:

* Server: `p2pServer` in `internal/netnode/transport.go:126`; the same server
  is bound to `peer_port`, `p2p_port` and `controller_port`
  (`startServers`, `internal/netnode/node.go:360`). Python does the same in
  `network/transport.py:204`.
* Client: `OpenSession` (`transport.go:337`) opens a `Session`, sends a
  signed `HELLO`, waits for the peer's `HELLO_ACK`, then returns a
  `PeerStream` with `Send`/`Recv`/`Close`.
* `PeerStream` is a bounded JSON-message session. Both directions use a
  256-slot queue (`SessionQueueSize`); `Send` blocks when the queue is full
  (backpressure) and `Recv` returns `PeerClosed` when the session ends.
* Most gossip is sent over **one-shot sessions**: `sendToPeer`
  (`node_peers.go:488`) opens a session, sends exactly one message and closes
  it. The Go sender flushes queued messages before tearing the connection
  down (`transport.go:374`).

The REST API complements the P2P layer:

* `GET /bootstrap` returns known peers **plus this node's own signed peer
  record** (`handleBootstrap`, `internal/api/server.go:399`); the gRPC
  `Bootstrap` RPC returns the same list
  (`p2pServer.Bootstrap`, `internal/netnode/transport.go:268`), so a joining
  node learns the seed's advertised address (needed for cache-only restarts).
* `POST /join` reads `SAN_BOOTSTRAP`, fetches peers through the REST
  endpoint (`DiscoverPeers`) and registers (`handleJoin`,
  `internal/api/server.go:409`).
* `POST /faucet` is an optional, node-local endpoint (`SAN_FAUCET=1`): it
  builds a normal transfer signed by the node identity and submits it through
  the ordinary mempool path (`internal/api/faucet.go`), so funded transactions
  are gossiped like any user transaction.

---

## 2. Peer records and discovery

### 2.1 Signed self record

`SelfPeerRecord` (`internal/netnode/node_peers.go:25`;
`Node.self_peer_record`, `network/Node.py:625`) builds:

| Field | Value |
|-------|-------|
| `chain_id` | this node's chain |
| `host` | `SAN_ADVERTISE_HOST`, else the locally detected IP |
| `api_port`, `p2p_port`, `peer_port`, `controller_port` | configured ports |
| `timestamp` | current time (seconds, float) |
| `tls` | whether server TLS is enabled |
| `public_key`, `signature` | added when the node has a key |

The signature covers the canonical JSON of the record **without**
`signature` only (`peerRecordPayload`, `peerMetaFields`).

### 2.2 Verification

`VerifyPeerRecord` (`node_peers.go:55`):

1. `chain_id` must match.
2. A record without `public_key`/`signature` is rejected when
   `SAN_REQUIRE_BLOCK_SIGNATURE=true` (default), otherwise accepted.
3. The timestamp must be fresh: `|now - timestamp| <= peer_record_ttl`
   (`SAN_PEER_TTL`, default 300 s).
4. The signature must verify against the signed payload.

`seenRecently` (`node_peers.go:89`) deduplicates announcements by identity
(host/ports/key, ignoring `timestamp` and `signature`) for one TTL, so
periodic re-announcements do not trigger re-gossip.

### 2.3 Registration and merging

* `RegisterToNetwork` sends `PEER_UPDATE {peer: self}` to the current
  outgoing peer on the peer port (`node_peers.go:114`).
* `addPeer` (`node_peers.go:155`) completes missing port fields from the
  local config (`CompletePeer`), rejects self (localhost host + own API port)
  and invalid records, deduplicates by `host + api_port`, and enforces
  `SAN_MAX_PEERS` (default 64).
* The node answers a peer's `GET_PEERS` with its own list (`PEERS`); when a
  **new** record is added it also calls `refreshPeerSelection()` and
  announces that record to every other peer (`gossipPeers`,
  `node_peers.go:450`).
* The node also **asks**: `requestPeers` (`node_peers.go:547`) sends
  `GET_PEERS`, waits up to `2 * SAN_WS_TIMEOUT` for a `PEERS` reply, and
  merges every record through the same `addPeer` path used by `PEER_UPDATE`
  (chain binding, freshness, signature, self/dedup/`SAN_MAX_PEERS`). Self
  entries already in the table are pruned and the selection is refreshed. A
  reply is truncated to `2 * SAN_MAX_PEERS` records and draws no reply, so a
  hostile list cannot force unbounded signature verification or a gossip
  storm.

### 2.4 Discovery

On startup `bootstrap` (`node_peers.go:509`) runs for every entry of the
comma-separated `SAN_BOOTSTRAP` list (plus the `SAN_DNS_SEEDS` entries that
were resolved into the address manager):

1. Try the gRPC `Bootstrap` RPC (`RemoteBootstrap`, 5 s timeout).
2. If it returns nothing, fall back to the REST `GET /bootstrap`
   (`DiscoverPeers`, `node.go:1261`, 5 s timeout).
3. Merge the records (`AddPeers`), and if any peer was added, register with
   `RegisterToNetwork`.
4. Ask the selected peer for its own list over the session stream
   (`requestPeers`) so a partial seed list converges.

`POST /join` performs the same REST discovery on demand. The health loop
(section 3) and a successful `Synchronize` (section 7.2) also call
`requestPeers`, so discovery converges over the session stream even when the
REST `/bootstrap` endpoint is unreachable.

### 2.5 Local peer registry (auto-discovery)

Explicit bootstrap is optional: nodes running on the same machine find each
other through a small file-backed registry (`internal/netnode/discovery.go`).
`cmd/sanup` enables this with `SAN_DISCOVERY=1`; `sannode` can opt in with the
same variable.

- **File**: `SAN_PEER_REGISTRY` when set, otherwise `~/.san/peers.json`
  (Windows: `%USERPROFILE%\.san\peers.json`).
- **Entries**: the node's own signed `SelfPeerRecord` (host, ports, chain id,
  timestamp, public key, signature) plus a `heartbeat` timestamp. Writes take
  a `<file>.lock` and replace the file atomically, so concurrent starts cannot
  lose entries; stale locks (10 s) are reclaimed.
- **Freshness**: entries whose heartbeat is older than `SAN_DISCOVERY_TTL`
  (default 600 s) are ignored and dropped on the next write. Records still
  have to pass `VerifyPeerRecord` (chain binding, signature, `SAN_PEER_TTL`).
- **Loop**: `startDiscovery` registers this node and merges fresh entries
  before the initial sync; the discovery loop then re-publishes its heartbeat
  every `SAN_DISCOVERY_INTERVAL` seconds (default 5), merges newly discovered
  records, and calls `requestPeers` plus one bounded background `Synchronize`
  when it added peers.
- **Fallback probing**: every 30 s the loop also probes the default local REST
  ports 8000-8010 (`GET /bootstrap`) and peer ports 8770-8780 (gRPC
  `Bootstrap`), so nodes started without the registry are still found
  (`SAN_DISCOVERY_PROBE=false` disables this).
- **Cleanup**: `Node.Stop` removes the node's own entry; `sanup --stop`
  removes it even after a forced kill, and otherwise the TTL expires it.

Nothing in the registry is trusted: every record is verified like any other
peer announcement, so a hostile local file can at worst advertise peers that
fail verification or the health checks.

### 2.6 Wide-area discovery (DNS seeds, addrman, outbound)

The registry above only works on one machine. For a public devnet the node
uses a Bitcoin-style address manager plus explicit seeds
(`internal/netnode/addrman.go`, `dnsseed.go`, `outbound.go`,
`discovery.go`). It activates when `SAN_DNS_SEEDS`, `SAN_BOOTSTRAP` or
`SAN_DISCOVERY=1` is set, or when a persisted address cache exists.

**Sources.**

* `SAN_DNS_SEEDS` is a comma-separated list of seed hostnames (or
  `host:port`). They are resolved with `net.DefaultResolver.LookupHost` at
  startup and every 5 minutes; every A/AAAA record becomes a candidate on the
  peer port (`SAN_PEER_PORT`).
* `SAN_BOOTSTRAP` is a comma-separated list of `host:port` seeds. Every entry
  is fetched at startup (gRPC `Bootstrap`, REST `/bootstrap` fallback) and
  also stored in the address manager; a `127.0.0.1:peer_port` entry is the
  cross-machine-style test case.
* `PEER_UPDATE`, `PEERS` replies and inbound sessions feed the manager:
  gossiped records must pass `VerifyPeerRecord` (source `gossip`), registry
  records use source `registry`, and the observed IP of an inbound session is
  stored low-trust (source `inbound`, never gossiped).
* The manager never stores this node itself and deduplicates by
  `host:peer_port`.

**Addrman.** A bounded (`SAN_MAX_ADDR_ENTRIES`, default 1024) map of entries
with `record`, `source`, `first_seen`, `last_seen`, `last_tried`, `failures`
and a `tried` flag. When full, untried and least-recently-seen entries are
evicted first. Entries idle for 30 days are dropped on load. The set is
persisted atomically (0600) to `SAN_PEERS_CACHE`, default
`<SAN_DB_PATH dir>/peers-cache.json` (or `~/.san/peers-cache.json`), every
60 seconds while dirty and on a graceful `Stop()`. Persisted records are not
re-validated against `SAN_PEER_TTL` (signatures age out); a cached address can
only become a peer after a fresh signed handshake, so a tampered cache can at
worst waste a dial.

**Outbound slots.** `outboundLoop` (every `SAN_DISCOVERY_INTERVAL`, capped at
5 s) keeps up to `SAN_OUTBOUND_PEERS` (default 8) outbound candidates
connected. It selects eligible addresses, dials them with the TLS-aware
transport, asks for their peer list (gRPC `Bootstrap`; a plain
`OpenSession` + `GET_PEERS` + `PEER_UPDATE` fallback), then calls
`requestPeers` and one bounded `requestSync`. Failures set an exponential
backoff (`5 s * 2^failures`, capped at 15 min); after 5 consecutive failures
the address is evicted from the manager and from `PEERS`.

**Address gossip.** The existing `GET_PEERS`/`PEERS` and `PEER_UPDATE`
messages are the gossip layer: `handlePeersMessage` merges every verified
record, stores it in the manager and refreshes the selection; the health loop
periodically re-announces this node's signed record. No new wire message was
added, so Python nodes remain compatible.

**DNS/DoS considerations.** DNS seeds are a centralization point: an operator
running their own resolver can censor or eclipse a node, so `SAN_BOOTSTRAP`
(and the cached address book) is always kept as a fallback, the seed hostnames
come from the operator's own configuration, and a seed cannot replace
signature verification. The manager bounds memory, truncates `PEERS` replies
to `2 * SAN_MAX_PEERS`, rate-limits sessions and applies the reconnect backoff
above, so a hostile seed can only advertise unreachable or unverifiable
addresses. The local registry is only a fallback when no wide-area sources are
configured; the loopback port probe (8000-8010 / 8770-8780) runs only in that
same case and never leaves `127.0.0.1`.

**Eclipse resistance.** A node keeps its cached address book across restarts
and prefers a tried/new mix, so a single seed cannot replace the whole peer
set; outbound slots are filled from the manager, not only from gossip. There
is no per-IP subnet bucketing or feeler connection yet (see
[Status / limitations](#status--limitations)).

---

## 3. Peer health

The peer health loop (`peerHealthLoop`, `node_peers.go:334`) runs every
`SAN_PEER_CHECK_INTERVAL` seconds (default 30) and does all of the
following:

1. `CheckDeadPeers` (below) and `refreshPeerSelection`.
2. If peers exist: `RegisterToNetwork` (self-healing re-announcement) and
   `requestPeers` (pull the outgoing peer's list with `GET_PEERS` -> `PEERS`,
   merge and refresh the selection).
3. `maybeProduceFromPool` (retry block production; e.g. a controller was
   briefly down).
4. `flushPendingVotes` and `rebroadcastOwnVotes`.
5. Fetch up to 8 missing orphan parents with `GET_BLOCK`.
6. Pull the mempool (`RequestMempool`) when the local pool is empty, or when
   this node is the expected proposer for the next height.

### 3.1 PING/PONG and eviction

`PingNode` (`node_peers.go:425`) opens a one-shot peer session, sends
`PING`, waits up to 3 seconds for `PONG`, and returns true only when the
handshake completed and the peer answered.

`CheckDeadPeers` (`node_peers.go:392`):

* a successful ping clears `peerFailures[host:api_port]`;
* a failed ping increments it; below `SAN_PEER_MISS_THRESHOLD` (default 2)
  the peer stays with a logged miss;
* at the threshold the peer is removed (`removePeer`, which refreshes the
  selection) and a `DEAD_PEER` claim is gossiped to the outgoing peer
  (`gossipDeadPeer`).

Received `DEAD_PEER` messages are **ignored** ("unsigned eviction claims are
ignored: peers are only dropped by our own health checks",
`node_peers.go:767`). `SAN_MAX_PEERS` bounds how many records the node keeps.

---

## 4. Peer and controller selection

`refreshPeerSelection` (`node_peers.go:281`) is called on start, after peer
add/remove, after health checks and after bootstrap:

```
pool        = copy(PEERS); shuffle(pool)         # per-node randomness
incoming    = pool[0]
outgoing    = pool[1] if len(pool) > 1 else pool[0]
controllers = selectControllers()
```

* `incoming` is the fallback sync peer used when no peer answers `Status`.
  Other request paths prefer the outgoing peer and then walk the peer list.
* `outgoing` receives `DEAD_PEER` claims, self registration, and is tried
  first for `GET_BLOCK` and `GET_TXS`.
* `controllers` is the set the node asks to approve locally produced blocks.

Controller selection (`selectControllers`, `node_peers.go:233`):

1. `epoch = tip_index / max(SAN_EPOCH_LENGTH, 1)` (default epoch length 100).
2. A peer is eligible when its record has a `public_key`; if
   `SAN_CONTROLLER_MIN_STAKE > 0`, the balance of the address derived from
   that key must be `>= controller_min_stake` (liquid balance, not stake).
3. Score each eligible peer with `sha256("{epoch}:{public_key}")` and rank
   ascending (the selection is deterministic for an epoch and a peer set).
4. Keep the first `SAN_CONTROLLER_COUNT` peers (default 10).

The controller service itself answers `BLOCK_VOTE_REQUEST` with a signed
`BLOCK_VOTE_RESPONSE` after running full block verification
(`serveBlockVote`, `node_sync.go:567`). The selection is only used on the
requesting side; any peer that is asked verifies and answers.

Because selection uses the local peer table, different nodes can have
different controller sets. See the consensus document's threat model.

---

## 5. Session message types

Every session starts with a handshake; after that the dispatcher routes by
`type`. Messages handled before the generic peer handler
(`dispatchPeerMessage`, `node_peers.go:717`): `GET_BLOCK`, `BLOCK`,
`BLOCK_VOTE_REQUEST`. Everything else goes to `handlePeerMessage`
(`node_peers.go:738`). Unknown types are logged and ignored.

| Type | Direction | Port | Purpose and flow |
|------|-----------|------|------------------|
| `HELLO` | initiator -> responder | any | Signed handshake: `type`, `protocol=2`, `chain_id`, `public_key`, `timestamp`, `signature`. Must be the first message. |
| `HELLO_ACK` | responder -> initiator | any | Same shape; the initiator verifies it before using the session. |
| `PING` | either | peer | Liveness probe. |
| `PONG` | either | peer | Answer to `PING`. |
| `PEER_UPDATE` | either | peer | One signed peer record. Verified, deduplicated, added, then re-gossiped to all other peers. |
| `DEAD_PEER` | either | peer | Claim that a peer is dead. **Ignored** on receipt; only local health checks evict. |
| `GET_PEERS` | either | peer | Ask for the peer list; the responder replies `PEERS` with its stored records (`node.Peers()`), exactly like Python (`node_peers.go:773`). |
| `PEERS` | either | peer | Peer list reply. Go verifies and merges every record (`handlePeersMessage`, `node_peers.go:594`), prunes self entries, refreshes the selection and truncates the list to `2 * SAN_MAX_PEERS`; the Python reference only answers `GET_PEERS` (it has no `PEERS` client path). |
| `TX` | either | peer | One mempool transaction payload; re-validated and pooled, then gossiped if new. |
| `GET_TXS` | either | peer | Ask for up to 64 mempool transactions. |
| `TXS` | either | peer | Answer to `GET_TXS` (at most 64 payloads). |
| `GET_BLOCK` | either | p2p | Ask for a block body by hash (orphan parent fetch). |
| `BLOCK` | either | p2p | A full block body. Used both for gossip and as the `GET_BLOCK` answer. |
| `BLOCK_NOT_FOUND` | responder -> requester | p2p | The requested hash is unknown; the requester treats it as a failed fetch. |
| `BLOCK_VOTE_REQUEST` | producer -> controller | controller | Full block dict; the controller runs `VerifyBlock` and answers. |
| `BLOCK_VOTE_RESPONSE` | controller -> producer | controller | `{chain_id, approved, block_hash, public_key, signature}`. The producer checks type, approval, chain id, block hash, advertised key and signature. |
| `FINALITY_VOTE` | either | peer | A stake-weighted finality vote (see the consensus doc). Verified, staged or tallied, then re-gossiped. |

Handshake:

```
initiator                                             responder
    |   gRPC Session(stream Envelope)                    |
    |------ HELLO {protocol:2, chain_id, --------------->|
    |        public_key, timestamp, signature}           |
    |                                                    | acceptHandshake:
    |                                                    |  type == HELLO &&
    |                                                    |  VerifyHello(data)
    |<----- HELLO_ACK {same shape} ----------------------|
    | VerifyHello(data)                                  |
    |                                                    |
    |------ PING ---------------------------------------->| handlePeerMessage
    |<----- PONG -----------------------------------------|
    |------ PEER_UPDATE {signed record} ----------------->| verify -> dedup -> add
    |------ TX {payload} -------------------------------->| ingest -> gossip
    |------ GET_TXS ------------------------------------->|
    |<----- TXS {txs:[...]} ------------------------------|
    |------ GET_PEERS ----------------------------------->| handlePeerMessage
    |<----- PEERS {peers:[...]} --------------------------|
    |------ GET_BLOCK {block_hash} ---------------------->| dispatch -> serveBlockRequest
    |<----- BLOCK {block} / BLOCK_NOT_FOUND --------------|
    |------ BLOCK_VOTE_REQUEST {block} ------------------>| dispatch -> serveBlockVote
    |<----- BLOCK_VOTE_RESPONSE {approved, signature} ----|
    |------ FINALITY_VOTE {vote} ------------------------>| handleFinalityVote
    |  ...                                                |
```

`VerifyHello` (`node.go:442`) enforces `type in {HELLO, HELLO_ACK}`,
`protocol == 2`, matching `chain_id`, `|now - timestamp| <= HELLO_TTL`
(60 s), and a valid signature over the record without `signature`.

Note that **staking and governance messages have no dedicated gossip type**:
`deposit`/`undelegate`/`withdraw`/`evidence`/`set_param` are ordinary
transactions and travel as `TX`/`TXS`. Transaction commit results are only
returned by the REST API (`POST /transaction`); they are not gossiped.

---

## 6. Propagation

### 6.1 Block gossip

`gossipBlock` (`node_sync.go:651`; `Node.gossip_block`,
`network/Node.py:2517`):

* deduplicates by block hash (`seenBlockGossip`, reset to the latest hash
  when it exceeds 4096 entries);
* sends `BLOCK` to **every** known peer on `p2p_port` using one-shot
  sessions;
* is called by `sendToControllers` as soon as the controller quorum is
  reached (before the local commit), after a block is committed locally, and
  after accepting an incoming gossip block when `SAN_BLOCK_GOSSIP=true`
  (default, deduplication makes repeated calls no-ops). Sync and `GET_BLOCK`
  fetch paths commit blocks without re-gossiping them.

`handleIncomingBlock` (`node_sync.go:593`) also applies a sync-miss
heuristic: if a block that extends the tip fails local verification twice in
a row, the node calls `Synchronize` instead of stalling (using a fresh
background context because one-shot gossip sessions are canceled when the
sender closes them).

### 6.2 Transaction gossip

`gossipTransaction` (`node_consensus.go:973`):

* deduplicates by tx id (`seenTxGossip`, reset above 8192 entries);
* sends `TX {tx: payload}` to every peer on `peer_port`;
* is called after `SubmitTransaction` and after a new transaction is ingested
  from `TX`/`TXS`.

Mempool pull: `RequestMempool` (`node_consensus.go:922`) uses the outgoing
peer, `GET_TXS`, expects `TXS`, and ingests at most 64 payloads (each
re-validated through `IngestTransaction`, which also re-gossips accepted
transactions).

### 6.3 Vote gossip

`broadcastVote` (`node_consensus.go:223`) deduplicates by
`height:block_hash:public_key` in `seenVotes` (reset when it exceeds 8192
entries) and sends `FINALITY_VOTE` to every peer on `peer_port`. Every vote
that is staged or tallied is re-broadcast, which converges the vote set
without a coordinator. `rebroadcastOwnVotes` (`node_consensus.go:165`)
re-sends this node's own votes for heights in
`max(finalized - 8, tip - 64, 0) < height <= tip` on every health-check
cycle, so a single dropped message cannot stall finality.

---

## 7. Chain sync

### 7.1 Status and longest-chain selection

`PeerStatus` (`internal/netnode/node_peers.go:847`;
`Node.peer_status`, `network/Node.py:3036`) returns:

| Field | Meaning |
|-------|---------|
| `version` | block/chain schema version (5) |
| `chain_id` | chain identity |
| `genesis_allocation` | SHA-256 fingerprint of the canonical genesis allocations (config, not the genesis block hash) |
| `height` | tip index |
| `finalized_height` | finality checkpoint |
| `tip_hash` | tip block hash |

`selectSyncPeer` (`internal/netnode/node_sync.go:74`;
`Node._select_sync_peer`, `network/Node.py:3047`):

1. Take at most 8 known peers.
2. Ask each for `Status` (5 s timeout).
3. Skip a peer whose `chain_id` differs, or whose non-empty
   `genesis_allocation` fingerprint differs from ours.
4. Pick the highest reported `height`.
5. If no status is available, fall back to the incoming peer, else the first
   known peer.

### 7.2 Synchronize

`Synchronize` (`internal/netnode/node_sync.go:119`;
`Node.synchronize`, `network/Node.py:3262`):

```
fromIndex = local tip + 1
batch     = max(SAN_SYNC_BATCH, 1)                  # default 128
maxBlocks = max(SAN_SYNC_MAX_BLOCKS, batch)         # default 50000
while applied < maxBlocks:
    page = remoteSync(peer, fromIndex, batch)       # unary Sync RPC, 10 s timeout
    verify page.chain_id / page.genesis_allocation / page.version (<= 5)
    for each block in page.blocks:
        historical = !(local tip is fresh && block.index == local tip + 1)
        if !verifyBlock(block, historical): stop
        if !commitBlock(block, historical): stop
        applied++; fromIndex = block.index + 1
    if stop or !page.has_more: break
```

* The server (`GetSyncPayload`, `node_sync.go:23`) caps a page at 512
  blocks, fetches one extra block internally to decide `has_more`, and
  includes `next_from_index`.
* Only the first page (`from_index <= 0`) carries `state` and `storage`
  snapshots; **the client never adopts them** - state is derived by
  replaying the verified blocks. A peer can withhold blocks but cannot
  inject state.
* `historical=true` relaxes only the live wall-clock lower bound
  (`BLOCK_PAST_DRIFT`); every other consensus rule still applies. A block
  that extends a fresh local tip is validated with live rules.
* A version newer than the local schema stops the sync; verification or
  commit failure stops the batch.
* After applying blocks: `connectOrphans`, `lastSeenBlockIndex = tip`,
  `flushPendingVotes`, then `requestPeers` (a peer with a longer chain is a
  good source of fresh peer records).

### 7.3 Block fetch fallback

For a missing parent (or any requested hash), `requestBlock`
(`node_sync.go:279`) opens a `GET_BLOCK` session on `p2p_port`, tries the
outgoing peer first and then each known peer, with in-flight deduplication
(`requestedBlocks`, reset above 512 entries). `fetchBlock`
(`node_sync.go:451`) classifies the reply (`classifyBlockReply`,
`node_sync.go:429`):

* `BLOCK`: parsed and passed through `processIncomingBlock`;
* `BLOCK_NOT_FOUND`: the peer is marked as missing that hash
  (`markPeerMissingBlock`, `BlockMissTTL` = 120 s) and moved to the back of
  the candidate list for that hash (`blockFetchPeers`), but it is still tried
  when every peer is marked; a successful fetch or any incoming gossip of the
  hash clears the marks (`clearBlockMisses`);
* anything else: a plain failed fetch.

If **every** candidate answers `BLOCK_NOT_FOUND`, one chain sync is
attempted (at most once per `SAN_PEER_CHECK_INTERVAL`) because the block may
live on a longer branch we have not seen. In all failure cases the dedup
entry is removed so a later cycle can retry. Python's `_request_block` asks a
single peer and treats `BLOCK_NOT_FOUND` as a generic failure, so the Go
retry policy is a strict extension that changes nothing on the wire.

---

## 8. Hardening

| Mechanism | Implementation |
|-----------|----------------|
| Handshake first | `acceptHandshake` (`node_peers.go:688`) rejects any connection whose first message is not a valid `HELLO`; handshake timeout is `2 * SAN_WS_TIMEOUT`. |
| Session queue bounds | inbound/outbound queues of `SessionQueueSize = 256`; a flooding peer blocks on the queue instead of growing memory. |
| Concurrent session cap | `MaxConcurrentSessions = 256`; over the cap gRPC returns `ResourceExhausted`. |
| Concurrent sync cap | `MaxConcurrentSyncs = 8` (`transport.go:241`). |
| Per-connection rate limit | at most `SAN_PEER_RATE_LIMIT` inbound messages (default 60) per `SAN_PEER_RATE_WINDOW` (default 10 s); the session is closed on excess. |
| Message size cap | `grpc.MaxRecvMsgSize`/`MaxSendMsgSize` set to `SAN_WS_MAX_SIZE` (default 1 MiB) on server and client (`BuildServer`, `dialPeer`). |
| Controller vote verification | approval responses are checked for chain id, block hash, advertised public key and signature before counting (`requestBlockVote`, `node_sync.go:509`). |
| Peer record freshness | ±`SAN_PEER_TTL` (default 300 s) and signature verification; unsigned records rejected by default. |
| Dead-peer claims | `DEAD_PEER` is not trusted; eviction requires the local ping threshold. |
| Orphan/request bounds | `SAN_MAX_ORPHANS` buffered fork blocks, `SAN_MAX_REORG_DEPTH` reorg bound, 512 in-flight block requests (reset when exceeded). |
| TLS | server certificate from `SAN_TLS_CERT`/`SAN_TLS_KEY`; clients verify with `SAN_TLS_CA` or the system trust store; with TLS the peer host is pinned via `grpc.WithAuthority(host)` for self-signed certificates (`dialPeer`, `TransportClientCredentials`). `sanup cert --ca-only` creates one shared devnet CA and `sanup cert --ca-dir` signs per-node certificates from it (the CA key never leaves the CA machine); see [public-devnet.md](public-devnet.md#2-shared-devnet-ca). |
| Outbound backpressure | `PeerStream.Close` waits at most 500 ms for the sender, then closes the gRPC connection to unblock a sender stuck in flow control; `CloseSend` is only called once the sender stopped (calling it concurrently with `Send` races inside gRPC). Regression test: `internal/netnode/transport_leak_test.go` (F15 fixed). |
| Vote bounds | `VoteMaxBytes`, `VoteLookahead` and staged-voter caps (see the consensus document). |

---

## 9. Constants

| Constant | Value | Where |
|----------|-------|-------|
| `ProtocolVersion` | 2 | `internal/netnode/node.go:35` |
| `SessionQueueSize` | 256 | `internal/netnode/transport.go:26` |
| `MaxConcurrentSessions` | 256 | `internal/netnode/transport.go:27` |
| `MaxConcurrentSyncs` | 8 | `internal/netnode/transport.go:28` |
| `HelloTTL` (handshake freshness) | 60 s | `internal/netnode/node.go:52` |
| `SAN_PEER_TTL` (`PeerRecordTTL`) | 300 s | `internal/netnode/config.go:94` |
| `SAN_PEER_CHECK_INTERVAL` | 30 s | `internal/netnode/config.go:84` |
| `SAN_PEER_MISS_THRESHOLD` | 2 | `internal/netnode/config.go:95` |
| `SAN_MAX_PEERS` | 64 | `internal/netnode/config.go:90` |
| `SAN_PEER_RATE_LIMIT` / `SAN_PEER_RATE_WINDOW` | 60 / 10 s | `internal/netnode/config.go:92` |
| `SAN_WS_MAX_SIZE` | 1,048,576 bytes (1 MiB) | `internal/netnode/config.go:91` |
| `SAN_SYNC_BATCH` (`SyncBatchSize`) | 128 | `internal/netnode/config.go:103` |
| `SAN_SYNC_MAX_BLOCKS` | 50,000 | `internal/netnode/config.go:104` |
| Sync page hard cap | 512 blocks | `internal/netnode/node_sync.go:28` |
| `SAN_EPOCH_LENGTH` | 100 | `internal/netnode/config.go:96` |
| `SAN_CONTROLLER_COUNT` | 10 | `internal/netnode/config.go:78` |
| `SAN_CONTROLLER_MIN_STAKE` | 0 | `internal/netnode/config.go:232` |
| Orphan parent retries per health cycle | 8 | `internal/netnode/node_peers.go:380` |
| In-flight block request bound | 512 | `internal/netnode/node_sync.go:288` |
| Mempool pull page | 64 transactions | `internal/netnode/node_peers.go:798` |
| Ping response timeout | 3 s | `internal/netnode/node_peers.go:435` |
| Bootstrap (`SAN_BOOTSTRAP`) timeout | 5 s (gRPC and REST) | `internal/netnode/node_peers.go:515` |
| `GET_PEERS` reply timeout | `2 * SAN_WS_TIMEOUT` (6 s) | `internal/netnode/node_peers.go:572` |
| `PEERS` reply cap | `2 * SAN_MAX_PEERS` (128 records) | `internal/netnode/node_peers.go:595` |
| `BlockMissTTL` (BLOCK_NOT_FOUND memory) | 120 s | `internal/netnode/node.go:45` |
| Missing-block sync throttle | one per `SAN_PEER_CHECK_INTERVAL` | `internal/netnode/node_sync.go:322` |
| `SAN_DISCOVERY_INTERVAL` (registry loop) | 5 s | `internal/netnode/discovery.go` |
| `SAN_DISCOVERY_TTL` (registry freshness) | 600 s | `internal/netnode/discovery.go` |
| Local fallback probe interval | 30 s (ports 8000-8010 / 8770-8780) | `internal/netnode/discovery.go` |
| `SAN_DNS_SEEDS` refresh interval | 5 min | `internal/netnode/discovery.go` |
| `SAN_MAX_ADDR_ENTRIES` (addrman cap) | 1024 | `internal/netnode/config.go` |
| `SAN_OUTBOUND_PEERS` (outbound slots) | 8 | `internal/netnode/config.go` |
| `SAN_PEERS_CACHE` (addrman file) | `<db dir>/peers-cache.json` | `internal/netnode/config.go` |
| Addrman flush / stale TTL | 60 s / 30 days | `internal/netnode/outbound.go`, `internal/netnode/addrman.go` |
| Reconnect backoff | `5 s * 2^failures`, max 15 min, evict at 5 | `internal/netnode/addrman.go` |

---

## Status / limitations

* **Same protocol, two implementations.** Go (`internal/netnode`) and Python
  (`network/Node.py`) share the JSON message vocabulary, chain-id binding and
  handshake rules; the Go tests include a two-node P2P test
  (`internal/netnode/node_p2p_test.go`) and a live PEERS/GET_BLOCK exchange
  over gRPC (`internal/netnode/node_peers_wiring_test.go`). There is no
  cross-language live network test, so message-level compatibility has not
  been exercised Go-to-Python in this repository.
* **`GET_PEERS`/`PEERS` client path is Go-only.** Go answers `GET_PEERS` with
  its raw stored record list (exactly like Python) and now also asks:
  bootstrap, the health loop and a successful sync call `requestPeers`, which
  merges the `PEERS` reply through the same verification/`SAN_MAX_PEERS` path
  as `PEER_UPDATE`, prunes self entries and refreshes the selection. The
  frozen Python reference has no client path for `PEERS` (its
  `_handle_peer_message` treats the type as unknown), so a Go node can pull
  peer lists from Python nodes but not vice versa until Python is updated.
* **`BLOCK_NOT_FOUND` retry policy is Go-only.** Both servers produce
  `BLOCK_NOT_FOUND`; Python's `_request_block` treats it as a generic failed
  fetch and asks only its one selected peer. Go classifies the reply,
  deprioritises that peer for the hash for `BlockMissTTL` (120 s), keeps
  walking the peer list, and triggers one throttled chain sync when every
  peer reports the hash unknown. The wire format is unchanged, so the
  difference is local retry policy only.
* **No standalone SYNC status message.** Synchronization uses the unary
  `Sync` RPC plus the unary `Status` RPC; there is no `SYNC`/`status`
  message type inside `Session`.
* **Staking/commit gossip is transaction gossip.** There is no
  `TX_COMMITTED` or stake-specific message; staking commands travel as
  ordinary `TX`/`TXS` payloads, and commit acknowledgements exist only in the
  REST response (`pooled`/`committed`/`rejected`).
* **Shuffle differences.** `refreshPeerSelection` uses each language's
  random shuffle, so incoming/outgoing peer choices are not comparable
  between Go and Python (they are node-local choices and must not agree).
* **Go round/queue details.** Go's `PeerStream` uses Go channels and flushes
  queued messages before closing; Python uses asyncio queues with a `_CLOSED`
  sentinel (`network/transport.py:77`). Both bound queues at 256 and cap
  sessions at 256 / syncs at 8.
* **Go `OpenSession` handshake timeout** is `SAN_WS_TIMEOUT` (3 s default)
  for both directions of the handshake; Python allows `ws_timeout * 2` for
  the `HELLO_ACK` receive (`network/transport.py:300`). A peer that is slower
  than 3 s to answer is treated as unreachable by the Go node.
* **TLS trust.** Without `SAN_TLS_CA`, both implementations use the system
  trust store and log a warning; self-signed deployments must configure the
  CA and rely on the pinned authority/host override. The public-devnet CA flow
  (one shared CA, per-node certificates, `ca.key` never distributed) and the
  plaintext phase-1 trade-off are in
  [public-devnet.md](public-devnet.md#2-shared-devnet-ca).
* **Local discovery is Go-only.** The registry/auto-discovery loop
  (section 2.5) has no Python counterpart; a Go node can find Python nodes via
  bootstrap/probing, but Python nodes cannot read the registry. A live
  two-node discovery integration test lives in
  `internal/netnode/discovery_test.go`, and `cmd/sane2e` exercises
  auto-discovery end to end with three nodes.
* **Wide-area discovery is Go-only too** (section 2.6). DNS seeds, the
  address manager, outbound slots and the peer cache have no Python
  counterpart, but no wire message changed, so a Go node can bootstrap from
  and gossip with Python nodes; only Go nodes benefit from the cache/backoff.
* **No subnet bucketing or feeler connections yet.** The addrman is a flat,
  bounded set with a tried/new mix and per-entry backoff; it does not bucket by
  /16 (or IPv6 /32) and does not probe random addresses. This is weaker than
  Bitcoin's eclipse resistance and is a documented residual risk.
* **Graceful stop on Linux/macOS.** `sanup --stop` verifies the recorded pid
  is really the staged `sanup-node` binary (`/proc/<pid>/exe`, with a cmdline
  fallback) before signalling, sends `SIGTERM`, waits up to 15 s for the
  graceful `Stop()` path (peer cache saved, registry entry removed) and only
  then escalates to `SIGKILL`. On Windows there is no SIGTERM: `--stop` uses
  `taskkill /F`, so the child cannot save the cache on exit and the outbound
  loop flushes it every 60 s instead.
