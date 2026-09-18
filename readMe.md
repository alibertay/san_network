# 🌐 SAN Network

SAN Network is a decentralized blockchain network with a peer-to-peer (P2P)
architecture, post-quantum signatures and smart contracts written in the
high-level **PENA** language.

> ✨ Smart contracts are compiled to SANVM bytecode and executed deterministically
> by every node.

---

## 🔑 Key Features

- **Post-Quantum Security**: blocks, transactions, consensus votes and peer
  records are signed with **ML-DSA-44** (the FIPS 204 standard of
  CRYSTALS-Dilithium2) via `pqcrypto>=1.0`.
- **Chain Binding**: every signature names the chain (`SAN_CHAIN_ID`), so
  transactions, blocks, votes and peer records can never be replayed on
  another network. The P2P handshake also verifies chain id and protocol
  version before any other message.
- **Addresses**: accounts are 20-byte addresses derived from the public key
  (`0x` + `sha3_256(pubkey)[:20]`); the transaction carries the full public
  key so signatures stay verifiable.
- **Deterministic Fee Model**: serialized size is priced per byte and code
  execution is priced per gas unit (`gas_limit * gas_price` escrowed up front,
  unused gas refunded). Both prices derive from chain data, so every node
  computes the same fee.
- **Gas Metering**: every opcode has a fixed cost; contracts that loop or read
  state pay for it, and out-of-gas executions are rolled back while the
  escrow is burned — no free compute.
- **Mempool Gossip**: accepted transactions propagate to peers over the P2P
  layer (not just the submitting node's API), with signature/nonce/balance
  validation on every hop and deduplication by transaction id.
- **Hardened RPC**: per-IP request rate limiting and request body limits are
  built in for public nodes.
- **Integer Money**: balances are integer base units (1 SAN = 10^8 units);
  no floating point drift.
- **Replay Protection**: every transaction carries a `nonce` and a `tx_id`;
  the mempool deduplicates and blocks are applied atomically.
- **Signed Peer Records**: peer announcements are signed, time bounded and
  deduplicated; gossip cannot loop, dead peers are evicted after repeated
  missed health checks, and nodes re-announce themselves so the mesh self-heals.
- **Controller Quorum**: blocks need ≥66% approval from the deterministic,
  per-epoch controller set before they are committed and broadcast.
- **Staking and Validators**: validators bond stake on-chain
  (`deposit`), can leave the active set (`undelegate`) and withdraw after the
  unbonding period (`withdraw`). Evidence of double-voting slashes the
  offender's stake.
- **Deterministic Proposer**: once a validator set is active, only the
  stake-weighted, hash-derived proposer for a height may produce its block.
- **BFT-style Finality**: validators sign `FINALITY_VOTE`s weighted by stake;
  a block with ≥2/3 of the active stake is finalized and finalized history can
  never be reorganized away.
- **Committed State**: every block header carries a `tx_root` (Merkle over the
  transactions) and a `state_root` (Merkle over accounts, validator stakes and
  contract code/storage); nodes recompute both and reject mismatches.
- **Light Clients**: `/headers` serves the header chain, `/proof/account` and
  `/proof/tx` serve Merkle inclusion proofs that verify against those roots.
- **Snapshots and Pruning**: state snapshots are stored at a configurable
  interval (verifiable against the header) and old block bodies can be pruned
  from the database behind the finality checkpoint.
- **Fee Market**: a per-gas base fee is burned and adjusts with block load
  (EIP-1559 style); only the tip goes to the validator.
- **Receipts**: every execution produces a receipt with status, gas used and
  contract logs (`print` in PENA emits events).
- **On-chain Governance**: consensus parameters (min stake, unbonding period,
  slash share, block gas limit) are chain state and change only with 2/3
  validator-approved `set_param` transactions.
- **SDK and CLI**: `sdk/client.py` and `python -m sdk.cli` cover balances,
  transfers, staking, contracts, proofs and governance.
- **Metrics**: dependency-free Prometheus text exposition at `/metrics`.
- **Chain Database**: a namespaced key-value store (LMDB; in-memory backend for
  tests) with column-family style prefixes, indexes for height, transactions
  and receipts, and **atomic per-block commits** — a crash can never leave a
  half-written block behind.
- **Fork Choice**: competing branches are buffered as orphans and the node
  reorgs to the longest fully verified chain (bounded by `SAN_MAX_REORG_DEPTH`),
  returning reorged-out transactions to the mempool.
- **Persistence**: set `SAN_DB_PATH` and the whole chain, ledger state and
  contract storage survive restarts (LMDB file).
- **Optional TLS**: run the P2P listeners and API over `wss`/`https` by
  providing a certificate; plain `ws`/`http` is only for development.
- **Smart Contracts**: build and deploy contracts in PENA (see
  `PENA/PENA_docs.md`).

---

## Documentation

- [docs/chaos-limited-consensus.md](docs/chaos-limited-consensus.md) — the
  consensus mechanism: roles and eligibility, deterministic proposer rounds
  with bounded fallback, block validity, fee market, 2/3 finality, slashing,
  governance, fork choice and determinism guarantees.
- [docs/gossip.md](docs/gossip.md) — the P2P layer: signed peer records,
  health checks, controller/peer selection, every session message type,
  gossip propagation, chain sync and hardening.
- [GO_MIGRATION.md](GO_MIGRATION.md) — the Go/Python package map, parity
  fixtures and the Go build/test workflow.

---

## 🚀 Quick Start

The fastest path is the one-command launcher (see [INSTALL.md](INSTALL.md)):

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# founder: new chain, premine + auto-stake, rewards to your address
python scripts/run_node.py --address 0xYourRewardAddress
```

Manual setup (same result, more knobs):

```bash
# 1. Create a persistent node key
python -m blockchain.identity -o san_key.json

# 2. Fund the node at genesis (SAN units)
PUBLIC_KEY=$(python -c "from blockchain.identity import NodeIdentity; print(NodeIdentity.from_file('san_key.json').public_key_hex)")
export SAN_KEY_FILE=san_key.json
export SAN_DB_PATH=data/node.kv
export SAN_GENESIS_ALLOCATION="$PUBLIC_KEY:1000000"

# 3. Run (single worker!)
python run.py
```

Join an existing network by pointing at a seed's REST endpoint:

```bash
export SAN_BOOTSTRAP=127.0.0.1:8000   # host:api_port of any node
python run.py
```

For multi-node genesis orchestration use `scripts/genesis_bootstrap.py`; for a
joiner that fetches and verifies the seed's genesis automatically use
`scripts/run_node.py --bootstrap ... --expect-genesis-hash ...`.

### Start your Go node

The Go implementation is fully self-contained: one Go command starts the node,
creates the node key on first run, publishes itself in the local peer registry
and keeps the wallet's on-chain stake in sync. No Python is involved at
runtime.

```bash
# start as founder (or auto-join a local node already running)
go run ./cmd/sanup --wallet 0xYourAddress

# same, and reconcile the on-chain stake to exactly 100 SAN
go run ./cmd/sanup --wallet 0xYourAddress --stake 100

go run ./cmd/sanup --status
go run ./cmd/sanup --stop
```

Start one node, then run the same command again with a different `--data-dir`:
the second node finds the first through `~/.san/peers.json` (override with
`SAN_PEER_REGISTRY`) and joins the same chain automatically — no `--bootstrap`
argument needed. Fallback port probing (REST 8000-8010, peer 8770-8780) also
finds nodes started by other means. `--bootstrap host:api_port` still works
and takes priority when provided. For machines that are not on the same host,
use the wide-area path (`--seeds`, `--advertise-host`, `--peer-cache`) in
[Public Devnet on a VPS](#public-devnet-on-a-vps); the registry is a local
fallback only and can be disabled with `--no-registry`.

Defaults: data dir `data/go-node`, API/P2P/peer/controller ports
8000/8765/8770/8769 (auto-incremented when busy), in-memory database,
unbonding 0, min stake 0, block reward 2 SAN and a 10 000 SAN dev genesis
allocation for the wallet. `--seed true|false|auto` controls founder/joiner
mode (auto is the default), `--key-file` overrides the key path, and `--json`
prints `--status` as JSON. Staking is signed by the node key, so `--wallet`
must be the key file address (on mismatch the launcher shows the correct
address).

`go run ./cmd/sane2e` is the test-only 3-node verification (auto-discovery,
transfers, SANRC20, custom contract, 0→100→70 stake); it prints a PASS/FAIL
summary and exits non-zero on failure. It is not required to run a node. See
[GO_MIGRATION.md](GO_MIGRATION.md) for details.

---

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `SAN_CHAIN_ID` | `san-devnet-1` | Chain identity; signatures are bound to it |
| `SAN_HOST` | `0.0.0.0` | Listener bind address |
| `SAN_API_PORT` | `8000` | REST API port |
| `SAN_P2P_PORT` | `8765` | Block broadcast port |
| `SAN_PEER_PORT` | `8770` | Peer discovery / gossip / PING-PONG port |
| `SAN_CONTROLLER_PORT` | `8769` | Controller vote port |
| `SAN_ADVERTISE_HOST` | local IP | Host announced to peers |
| `SAN_BOOTSTRAP` | – | Seed nodes `host:port` (comma separated) |
| `SAN_DNS_SEEDS` | – | DNS seed hostnames / `host:port` (comma separated) |
| `SAN_PEERS_CACHE` | `<db dir>/peers-cache.json` | Persisted address manager |
| `SAN_MAX_ADDR_ENTRIES` | `1024` | Address manager cap |
| `SAN_OUTBOUND_PEERS` | `8` | Outbound connection slots |
| `SAN_API_HOST` | `SAN_HOST` | REST bind host |
| `SAN_API_TOKEN` | – | Require `Authorization: Bearer <token>` on the REST API |
| `SAN_KEY_FILE` | – | Node key file (JSON, 0600) |
| `SAN_DB_PATH` | – | Database file (LMDB); unset = in-memory only |
| `SAN_DB_BACKEND` | `lmdb` | `lmdb` or `memory` |
| `SAN_SYNC_BATCH` | `128` | Blocks per sync page |
| `SAN_SYNC_MAX_BLOCKS` | `50000` | Max blocks per sync session |
| `SAN_GENESIS_ALLOCATION` | – | `address:amount[,address:amount]` (address or public key) |
| `SAN_CONTROLLER_COUNT` | `10` | Controllers asked per block |
| `SAN_CONTROLLER_MIN_STAKE` | `0` | Balance (SAN) required to be a controller |
| `SAN_EPOCH_LENGTH` | `100` | Blocks per controller-selection epoch |
| `SAN_BLOCK_THRESHOLD_FEE` | `500` | Mempool fees that trigger block production (SAN) |
| `SAN_BLOCK_GAS_LIMIT` | `30000000` | Max total gas per block |
| `SAN_BLOCK_REWARD` | `0` | Per-block subsidy in SAN (genesis parameter; devnet uses 2) |
| `SAN_REWARD_ADDRESS` | – | Coinbase address for tips + subsidy (default: the proposer's address) |
| `SAN_MIN_BLOCK_INTERVAL_MS` | `0` | Minimum time between blocks (ms); subsidy chains enforce ≥ 1000 |
| `SAN_PROPOSER_TIMEOUT` | `6.0` | Proposer round timeout (s) |
| `SAN_MAX_PROPOSER_ROUNDS` | `16` | Bounded proposer fallback rounds per height |
| `SAN_MIN_VALIDATOR_STAKE` | `1000` | Minimum stake to be an active validator (SAN) |
| `SAN_UNBONDING_PERIOD` | `100` | Blocks between `undelegate` and `withdraw` |
| `SAN_SLASH_BPS` | `5000` | Share of stake burned on proven equivocation (basis points) |
| `SAN_RPC_RATE_LIMIT` / `SAN_RPC_RATE_WINDOW` | `120` / `10` | Per-IP request limit |
| `SAN_RPC_MAX_BODY` | `1048576` | Max request body (bytes) |
| `SAN_SNAPSHOT_INTERVAL` | `1000` | Blocks between state snapshots (0 disables) |
| `SAN_PRUNE_KEEP` | `0` | Prune DB blocks older than this behind finality (0 keeps all) |
| `SAN_MAX_PEERS` | `64` | Peer limit |
| `SAN_PEER_TTL` | `300` | Peer record freshness window (s) |
| `SAN_PEER_MISS_THRESHOLD` | `2` | Failed health checks before eviction |
| `SAN_MAX_ORPHANS` | `64` | Buffered fork blocks |
| `SAN_MAX_REORG_DEPTH` | `64` | How far back a reorg may go |
| `SAN_BLOCK_GOSSIP` | `true` | Re-broadcast accepted blocks |
| `SAN_PEER_RATE_LIMIT` / `SAN_PEER_RATE_WINDOW` | `60` / `10` | Per-connection message limit |
| `SAN_WS_MAX_SIZE` | `1048576` | Max P2P message size (bytes) |
| `SAN_REQUIRE_BLOCK_SIGNATURE` | `true` | Reject unsigned blocks and peer records |
| `SAN_TLS_CERT` / `SAN_TLS_KEY` / `SAN_TLS_CA` | – | Enable TLS / verify peers |
| `SAN_PEER_CHECK_INTERVAL` | `30` | Peer health loop interval (s) |
| `PRIVATE_KEY` / `PUBLIC_KEY` | – | Hex keys (development; prefer `SAN_KEY_FILE`) |

---

## 🔌 REST API

- `GET /health` → chain id, height, tip hash, peer/controller/mempool counters
- `GET /account/{address}` → balance (`SAN` + base units) and nonce
- `GET /block/{index}` → block payload
- `GET /mempool` → pending transaction ids
- `GET /contracts` → deployed contract ids
- `POST /contract/query` → read-only contract call (`{contract_id, function_name, params}`)
- `GET /validators` → active validator set with stake weights
- `GET /finality` → finalized height/hash and pending vote heights
- `GET /evidence` → collected equivocation evidence
- `GET /headers?from_index=&limit=` → header chain (no transactions)
- `GET /proof/account/{address}` → account Merkle proof vs the state root
- `GET /proof/tx/{block_index}/{tx_index}` → transaction Merkle proof vs `tx_root`
- `GET /snapshot` → latest finalized state snapshot
- `GET /receipt/{block_index}/{tx_index}` → execution receipt (status, gas, logs)
- `GET /receipt/tx/{tx_id}` → receipt by transaction id
- `GET /tx/{tx_id}` → transaction, its block and receipt (tx index)
- `GET /metrics` → Prometheus text metrics
- `GET /bootstrap` → known peers plus this node's own signed record
- `GET /sync?from_index=N&limit=M` → one page of blocks plus a snapshot; follow `has_more` / `next_from_index`
- `POST /transaction` → submit a signed transaction (JSON body)
- `POST /join` → discover peers from `SAN_BOOTSTRAP` and register

### Submitting a transaction

```python
import requests
from blockchain.address import address_from_public_key
from blockchain.identity import NodeIdentity
from blockchain.Transaction import Transaction

identity = NodeIdentity.from_file("san_key.json")
payload = {
    "chain_id": "san-devnet-1",          # must match the node's SAN_CHAIN_ID
    "sender": identity.public_key_hex,   # full public key (signature check)
    "nonce": 0,                          # next expected nonce for the address
    "receiver": "0x" + "ab" * 20,        # account address
    "value": 10,                         # SAN; up to 8 decimals
}
payload["signature"] = Transaction.sign_payload(payload, identity.private_key)

print("sender address:", address_from_public_key(identity.public_key))
print(requests.post("http://127.0.0.1:8000/transaction", json=payload).json())
```

Response statuses: `pooled` (waiting for the fee threshold), `committed`
(block produced, approved and applied), `rejected` (no controller quorum).

The node recomputes the `fee` itself; `signature` and `fee` are protocol
metadata and are excluded from the signed message.

### Transactions that execute code

Deploying or calling a contract needs gas fields (plain transfers must not set
them):

```python
payload = {
    "chain_id": "san-devnet-1",
    "sender": identity.public_key_hex,
    "nonce": 1,
    "gas_limit": 1_000_000,   # max gas units
    "gas_price": 1,           # base units per gas (minimum 1)
    "contract_code": {
        "command": "deploy",
        "contract_id": "kv",
        "pena_code": "value = 1\nfunction get() {\n return value\n}\n",
    },
}
payload["signature"] = Transaction.sign_payload(payload, identity.private_key)
```

The fee is `size_fee + gas_limit * gas_price`, charged up front; unused gas is
refunded after execution. If the code runs out of gas, the escrow is burned
and all state changes from that transaction are rolled back.

### Staking

Validator commands are normal signed transactions (no gas fields, no value):

```python
signed_transaction(          # bond stake and join the active set
    {"validator": {"command": "deposit", "amount": 500_00000000}},
)
signed_transaction({"validator": {"command": "undelegate"}})   # leave at once, stake unlocks later
signed_transaction({"validator": {"command": "withdraw"}})     # after the unbonding period
signed_transaction(          # slashing evidence: two signed votes, same height, different hashes
    {"validator": {"command": "evidence", "vote_a": vote_a, "vote_b": vote_b}},
)
```

### Governance

Consensus parameters live in the chain state and only change with a
`set_param` transaction approved by ≥2/3 of the active stake (approvals are
signed over chain id + parameter + the transaction's sender/nonce, so they
cannot be replayed):

```python
nonce = client.nonce()
approval = client.governance_approval("unbonding_period", 7, nonce=nonce)
client.governance_set_param("unbonding_period", 7, [approval], nonce=nonce)
```

Governable parameters: `min_validator_stake`, `unbonding_period`, `slash_bps`,
`block_gas_limit`, `proposer_timeout_ms`, `block_reward`.

---

## 🔸 Node-to-Node Protocol (gRPC)

Nodes talk to each other over **gRPC**; the REST API is only for users and
clients. One service (`network/proto/p2p.proto`) is bound to every P2P port:

| RPC | Purpose |
|-----|---------|
| `Session` (bidirectional stream) | Handshake, gossip, votes, requests |
| `Sync` | Paged chain synchronization |
| `Bootstrap` | Peer discovery from a seed |

Inside a `Session`, all messages are JSON objects with a `type` field. Every
connection starts with a signed handshake — `HELLO` (initiator) /
`HELLO_ACK` (responder) — carrying the protocol version, chain id, public key
and timestamp. Messages on a mismatched chain or protocol version are rejected
before anything else.

| Port | Message | Purpose |
|------|---------|---------|
| peer | `HELLO` / `HELLO_ACK` | Protocol + chain handshake |
| peer | `PING` / `PONG` | Health check |
| peer | `PEER_UPDATE` | Announce a signed peer record |
| peer | `DEAD_PEER` | Peer removal gossip |
| peer | `GET_PEERS` / `PEERS` | Peer list exchange |
| peer | `TX` | Mempool gossip |
| peer | `GET_TXS` / `TXS` | Mempool pull (restarts, joins) |
| p2p | `GET_BLOCK` / `BLOCK_NOT_FOUND` | Fetch a missing block (orphan parents) |
| p2p | `BLOCK` | Broadcast an approved block |
| controller | `BLOCK_VOTE_REQUEST` / `BLOCK_VOTE_RESPONSE` | Signed consensus vote |

Peer sessions are rate limited (`SAN_PEER_RATE_LIMIT`) and capped at
`SAN_WS_MAX_SIZE` bytes per message. TLS verifies peer certificates against
`SAN_TLS_CA` (or the system trust store), so self-signed deployments must set
`SAN_TLS_CA`.

---

## 🔁 Consensus and Fees

1. Transactions enter the mempool after signature, nonce and balance checks.
2. When pooled fees reach `SAN_BLOCK_THRESHOLD_FEE`, the node builds a block.
3. Controller nodes (deterministic per epoch, optionally stake-gated) verify
   the block and return **signed votes**.
4. With ≥66% approval the block is committed atomically (balances, nonces,
   contract execution) and broadcast. Otherwise it is dropped untouched.

Fees are charged per serialized byte and per gas unit
(`0.01`–`0.1 SAN/byte` plus `gas_used * gas_price`). The per-byte rate scales
with the parent block's load, so the same block always costs the same on
every node. A per-gas **base fee** (starting at 1, adjusting up to ±12.5% per
block as blocks fill) is **burned**; the rest of `gas_price` is the validator's
tip. Blocks are capped by the chain's `block_gas_limit` parameter.

When validators are staked, the proposer for each height is derived from the
chain id, height and active set; controllers still pre-commit the block, and
validators then sign stake-weighted finality votes. With ≥2/3 of the active
stake voting for a block it becomes **finalized**: reorgs that would rewrite
finalized history are rejected, which turns finality from probabilistic
(longest chain) into an explicit checkpoint.

If two nodes produce competing blocks, the tie is temporary: blocks are
gossiped to every peer, branches are assembled from the orphan buffer, and the
node switches to the longest **fully verified** chain (state and contract
storage are rebuilt by replaying it). Transactions from the reorged-out branch
return to the mempool automatically.

---

## 🔤 Smart Contracts with PENA

```pena
function greet(name) {
  print("Hello " + name)
}

woof greet("Alice")
```

PENA supports: variables, strings, arithmetic (`+ - * / %`), comparisons
(`== != < <= > >=`), logic (`&& || !`), `if / else if / else`, `while`,
`for i, a -> b`, `break` / `continue`, functions with parameters and return
values, lists (`mylist := [1, 2, 3]`) and dictionaries with subscript access
(`balances[owner]`). See `PENA/PENA_docs.md` for the full guide and
`PENA/examples/` for runnable contracts (SANRC20, SANRC721, AMM).

Every construct is lowered to **PENA Assembly (PASM)**, the textual
instruction layer of the SANVM. Contracts can also be written directly in
assembly, and high-level contracts may embed `asm { ... }` blocks:

```pena
counter = 0

function bump() {
  asm {
    GET counter
    PUSH 1
    ADD
    SET counter
  }
}
```

Deploy raw assembly by sending it as `pena_code` (auto-detected) or with
`"language": "asm"`. See `PENA/PENA_docs.md` for the mnemonic reference and
`PENA/examples/assembly/` for runnable examples.

---

## 🔎 Light Clients

A light client only follows headers and verifies what it needs:

```python
from blockchain.merkle import verify_merkle_proof
import requests

base = "http://127.0.0.1:8000"
headers = requests.get(f"{base}/headers", params={"from_index": 0, "limit": 10}).json()["headers"]
tip_root = headers[-1]["state_root"]

proof = requests.get(f"{base}/proof/account/{address}").json()
assert proof["root"] == tip_root
assert verify_merkle_proof(proof["root"], proof["leaf"], proof["proof"], proof["index"])
```

The same pattern works for transactions with `/proof/tx`, and `/snapshot`
returns a finalized state snapshot whose `state` verifies against the
`state_root` of the header at that height.

---

## 🗄️ Storage Architecture

The chain database mirrors what production clients do (geth's Pebble/LevelDB
layout, Bitcoin's block index):

| Namespace | Purpose |
|-----------|---------|
| `m:` | metadata (schema version, finalized checkpoint, genesis fingerprint) |
| `h:` / `b:` | block header and body, keyed by block hash |
| `n:<height>` | canonical height → hash index |
| `r:` | execution receipts keyed by block hash |
| `t:<tx_id>` | transaction index → block, position |
| `S:` | ledger state and contract storage snapshots |
| `k:<height>` | finalized state checkpoints |

Every block commit writes the header, body, canonical index, receipts,
transaction index and the resulting ledger state in **one atomic batch**
(`ChainStore.append_block`), so restarting always resumes at a consistent
height. `SAN_PRUNE_KEEP` prunes old namespaces behind the finality checkpoint,
keeping the newest snapshot as an anchor; a pruned node restarts from that
anchor and replays the remaining window. Schema versions are checked on open.

---

## 🪙 Block Rewards

`SAN_BLOCK_REWARD` (SAN per block, default 0) is a genesis consensus
parameter: when non-zero, the expected proposer keeps producing empty blocks
so validators earn a subsidy even without traffic. Tips and the subsidy are
credited to the address declared in the signed block header, which is set from
`SAN_REWARD_ADDRESS` (or the proposer's own address when unset). Set it in
genesis; every node then credits the same destination.

---

## 🚀 One-Command Node

`scripts/run_node.py` creates or joins a devnet and pays every block reward and
transaction tip to the address you choose. On startup the node **discovers its
peers, finds the longest compatible chain, verifies and replays it, then starts
working** — no manual configuration. A step-by-step setup guide lives in
[INSTALL.md](INSTALL.md).

```bash
# founder: brand-new chain, premine + auto-stake, rewards to 0x...
python scripts/run_node.py --address 0xYourRewardAddress

# joiner: fetch genesis from the seed, pin it, discover + sync + run
python scripts/run_node.py --address 0xYourRewardAddress \
    --bootstrap 127.0.0.1:8000 --expect-genesis-hash <genesis-hash>
```

Startup flow (all automatic):

1. **Discovery** — asks the seed (`SAN_BOOTSTRAP`) over gRPC `Bootstrap`
   (REST `/bootstrap` fallback) for signed peer records.
2. **Longest chain** — asks every peer for its `Status` (chain id, genesis
   fingerprint, height, finality), rejects incompatible chains and picks the
   highest one.
3. **Sync** — pages blocks (`Sync`, 512 at a time) from that peer, verifies
   each one (hash, signature, state root, transactions) and rebuilds the
   ledger by replaying them; a peer snapshot is never trusted.
4. **Work** — gossip, votes, mempool exchange, block production when this node
   is the expected proposer, finality votes.
5. **Rewards** — the block subsidy (`SAN_BLOCK_REWARD`) and tips are credited
   to the `reward_address` declared in the signed block header.

Useful flags: `--stake 0` (observer only), `--stake 1000` (deposit, default),
`--block-reward 2` (founder only), `--key node_key.json`,
`--api-port 8000`, `--db data/node.kv`. In joiner mode the seed's `/genesis`
endpoint supplies the immutable allocation and consensus parameters so the
local genesis hash matches exactly, and the launcher verifies it before
staking.

---

## 🛠️ SDK and CLI

```python
from blockchain.identity import NodeIdentity
from sdk import SanClient

client = SanClient("http://127.0.0.1:8000", NodeIdentity.from_file("san_key.json"))
print(client.transfer("0x" + "ab" * 20, 5))            # auto nonce + signing
client.deposit_stake(500)                               # become a validator
client.deploy_contract("kv", "value = 1\nfunction get() { return value }")
print(client.contract_query("kv", "get"))               # read-only call
print(client.validators()["parameters"])                # chain parameters
print(client.receipt(3, 0))                             # status, gas, logs
```

```bash
python -m sdk.cli --rpc http://127.0.0.1:8000 health
python -m sdk.cli --rpc http://127.0.0.1:8000 balance --address 0x...
python -m sdk.cli --rpc http://127.0.0.1:8000 --key san_key.json send --to 0x... --value 5
python -m sdk.cli --rpc http://127.0.0.1:8000 --key san_key.json deploy --id kv --file kv.pena
python -m sdk.cli --rpc http://127.0.0.1:8000 query --id kv --function get
```

## 🌐 Run a Local Testnet

A brand-new chain starts with **no coins at all**: the genesis allocation *is*
the premine. Keys must exist **before** the chain does, because a node that
auto-generates its key at startup gets a random key that cannot be premined.

```bash
# 1) generate node identities FIRST and print the shared genesis commands
python scripts/genesis_bootstrap.py --count 2 --dir devnet-keys --write-env
```

The helper prints (and writes to `devnet-keys/nodeN.env`) the exact start
command for each node: `keyN.json`, `nodeN.kv` (LMDB) and the shared
`SAN_GENESIS_ALLOCATION="<pubkey1>:1000000,<pubkey2>:1000000"` string. Every
node must use the same `SAN_CHAIN_ID` and the same allocation (it defines
genesis; a mismatch is refused at startup).

```bash
# 2) start the seed (node 1) and the joiner (node 2), from the printed commands
env $(cat devnet-keys/node1.env) python run.py &
env $(cat devnet-keys/node2.env) python run.py &
```

While no validator is staked the chain still runs in bootstrap mode (any
funded node can propose; blocks are accepted locally with no controller
quorum), but **finality does not advance**. Activate validators from the
premined accounts:

```bash
# 3) stake >= SAN_MIN_VALIDATOR_STAKE (default 1000 SAN) on each node
python -m sdk.cli --rpc http://127.0.0.1:8000 --key devnet-keys/key1.json stake --amount 1000
python -m sdk.cli --rpc http://127.0.0.1:8001 --key devnet-keys/key2.json stake --amount 1000
```

Once at least one validator is active, proposer rotation and `/finality`
start working. To hand coins to a user who joins later, send them a transfer
from a premined account (`sdk.cli send --to 0x... --value 10`) — the genesis
allocation itself cannot be changed after the chain starts.


---

## 🧪 Tests

```bash
pip install -r requirements-dev.txt
ruff check .
mypy
pytest                        # 77 tests: unit + storage + 9 live suites
python tests/smoke_p0.py      # API + basic P2P flow
python tests/smoke_p1.py      # money, VM/PENA, contracts, block validation
python tests/smoke_p2.py      # identity, peer security, TLS, persistence
python tests/e2e_network.py         # three real uvicorn nodes + restart catch-up
python tests/stress_consistency.py  # randomized workload + money conservation
```

---

## 🐹 Go Implementation

A Go port of the node lives next to the Python reference in the same module
(`github.com/alibertay/san_network`). The Go packages are parity-tested
against golden fixtures generated by the Python code, which remains the
reference implementation for cross-checking; see
[GO_MIGRATION.md](GO_MIGRATION.md) for the package map and
[docs/chaos-limited-consensus.md](docs/chaos-limited-consensus.md) /
[docs/gossip.md](docs/gossip.md) for the consensus and networking internals.
For day-to-day use, `go run ./cmd/sanup ...` starts and manages the Go node
(see [Start your Go node](#start-your-go-node) above); the raw commands are:

```bash
go build ./...                     # sanup, sane2e, sannode, sancli, sangenesis
gofmt -l .                         # must print nothing
go vet ./...
go test ./... -count=1             # Go unit + parity + discovery tests
go run ./cmd/sane2e                # Go end-to-end devnet check (exits non-zero on failure)

go run ./cmd/sanup --wallet 0xYourAddress --stake 100
go run ./cmd/sannode --address 0xYourRewardAddress  # = scripts/run_node.py
SAN_DB_BACKEND=memory go run ./cmd/sannode serve    # = run.py
go run ./cmd/sancli --rpc http://127.0.0.1:8000 health

CGO_ENABLED=1 go build -tags lmdb ./...             # LMDB persistence (cgo)
```

The default build is cgo-free and uses the in-memory backend; build with
`-tags lmdb` (the bundled LMDB only needs a C compiler) or set
`SAN_DB_BACKEND=memory` explicitly. The Python tests stay available
(`python -m pytest tests/test_units.py tests/test_asm.py -q`) to cross-check
behavior.

---

## Public Devnet on a VPS

The Go node is the implementation to run on a public devnet (the Python tree
is the frozen reference). Peers are found with DNS seeds, explicit bootstrap
addresses and a persisted address manager (`docs/gossip.md` section 2.6); the
same-machine `~/.san/peers.json` registry is only a fallback.

### Ports

All TCP; open them in the firewall/security group of every public node:

| Port | Default | Purpose |
|------|---------|---------|
| `api` | 8000 | REST API (public only if you want an open RPC) |
| `p2p` | 8765 | block gossip, paged sync, peer status |
| `peer` | 8770 | peer sessions, peer-list exchange |
| `controller` | 8769 | signed block-vote requests |

The three gRPC ports serve the same service, so a range rule works. Behind a
NAT/cloud router, forward the same ports and set `--advertise-host` to the
public DNS name or IP; peers dial the advertised ports.

```bash
# Linux firewall example
sudo ufw allow 8000/tcp
sudo ufw allow 8765:8770/tcp
```

### Seed node (new chain)

```bash
go run ./cmd/sanup --seed \
    --host 0.0.0.0 --api-host 0.0.0.0 \
    --advertise-host seed.example.com \
    --data-dir /var/lib/san/seed
```

`--host 0.0.0.0` binds the gRPC ports, `--api-host 0.0.0.0` the REST API
(use `--api-token` if the API must be public). Without `--host 0.0.0.0` the
node only listens on loopback.

### Joining node

```bash
go run ./cmd/sanup \
    --host 0.0.0.0 --api-host 0.0.0.0 \
    --advertise-host node2.example.com \
    --seeds seed.example.com \
    --data-dir /var/lib/san/node2
```

`--seeds` accepts DNS names or `host:port` and is used both to fetch the
genesis (REST, default port 8000 for a bare host) and for P2P discovery
(default peer port 8770). `--bootstrap host:api_port` pins the genesis source
explicitly. The node persists discovered addresses in
`<data-dir>/peers-cache.json` (`--peer-cache FILE`), so a restart reconnects
from the cache even when the seed is temporarily unreachable. Local nodes can
still auto-join through the registry; pass `--no-registry` to disable it
entirely.

### TLS

```bash
# once, per node (include the public DNS name / IP in the SANs)
go run ./cmd/sanup cert --dir /etc/san/certs --advertise-host node2.example.com

# copy ca.crt to every machine and start each node with:
go run ./cmd/sanup ... \
    --tls-cert /etc/san/certs/node.crt \
    --tls-key /etc/san/certs/node.key \
    --tls-ca /etc/san/certs/ca.crt
```

`sanup cert` writes `ca.crt`/`ca.key`, `node.crt`/`node.key` (node key 0600)
and prints the exact trust commands. Peers without the CA fall back to the
system trust store; without `--tls-*` all P2P traffic is plaintext (the node
logs a warning).

### Services

systemd unit (Linux; `SIGTERM` shuts the node down gracefully and flushes the
peer cache):

```ini
[Unit]
Description=SAN Network devnet node
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sanup --host 0.0.0.0 --api-host 0.0.0.0 \
    --advertise-host node2.example.com --seeds seed.example.com \
    --data-dir /var/lib/san/node2
Restart=always
RestartSec=5
Environment=SAN_API_TOKEN=change-me

[Install]
WantedBy=multi-user.target
```

On Windows, run the staged node (`<data-dir>\sanup-node.exe`) as a service
with `sc.exe create`/NSSM; `sanup --stop` uses a forced kill there, so the
peer cache is flushed by the node every 60 seconds instead of on exit.

### General hardening

- Set `--api-token`/`SAN_API_TOKEN` for a public REST endpoint and/or bind
  `--api-host 127.0.0.1` behind a reverse proxy.
- Build production nodes with `CGO_ENABLED=1 go build -tags lmdb` for LMDB
  persistence (`SAN_DB_BACKEND=lmdb`); the default build only has the memory
  backend and says so.
- Tune discovery with `SAN_DNS_SEEDS`, `SAN_PEERS_CACHE`,
  `SAN_OUTBOUND_PEERS` (default 8) and `SAN_MAX_ADDR_ENTRIES` (default 1024).
- Verify a running node with:
  `go run ./cmd/sancli --rpc http://127.0.0.1:8000 health` and
  `go run ./cmd/sanup --data-dir /var/lib/san/node2 --status` (pass the same
  `--api-token` when one is configured).

---

## 🔐 Security Notes

- Prefer `SAN_KEY_FILE` over environment keys; the file is written with `0600`.
- Set a distinct `SAN_CHAIN_ID` per network: signatures are bound to it, so a
  testnet transaction is invalid on mainnet (and vice versa).
- Run the API with **exactly one worker** (`run.py` enforces this): the chain
  lives in-process. Scale with more nodes, not more workers.
- Enable TLS in production and point peers at your CA with `SAN_TLS_CA`.
- Public nodes should keep RPC limits enabled (`SAN_RPC_RATE_LIMIT`,
  `SAN_RPC_MAX_BODY`) and be fronted by a reverse proxy for coarse filtering.
- Sync replays and verifies every block locally; ledger state and contract
  storage are derived, never trusted from a peer snapshot (a peer can withhold
  blocks, but cannot inject state).
- `SAN_PRUNE_KEEP` prunes block bodies behind finality and keeps the newest
  snapshot as the restart anchor; after a restart the node loads that window
  and replays it (state is verified against the snapshot root). While running,
  the node keeps the window it loaded, not the whole history.
- Controller eligibility without stake is sybil-prone; in open networks set
  `SAN_CONTROLLER_MIN_STAKE` and use the validator staking commands plus
  finality for real economic security.
