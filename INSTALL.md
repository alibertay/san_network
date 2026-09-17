# SAN Network — Setup Guide

One command brings a node up. On startup the node **discovers its peers, finds
the longest compatible chain, verifies and replays it, and starts working**.
Every block reward and transaction tip is credited to the address you choose.

---

## 1. Requirements

- Python 3.11 or newer
- pip
- At least 1 GB free disk for the database (more if you never prune)
- **TCP** ports below. A second node on the same machine shifts the whole
  block by +1 (8001 / 8701 / 8711 / 8721); on one machine only the ports of
  node 1 need to be reachable from outside.

| Port | Default | Protocol | Purpose | Reachable by |
|------|---------|----------|---------|--------------|
| `api` | 8000 | HTTP/REST | Users, SDK, block explorer, `run_node.py` progress | your clients (public if you want a public RPC) |
| `p2p` | 8700 | gRPC | Block gossip, paged sync, peer status | other nodes (must be open for a public node) |
| `peer` | 8710 | gRPC | Peer discovery / gossip sessions | other nodes (must be open for a public node) |
| `controller` | 8720 | gRPC | Signed block-vote requests | other nodes (must be open for a public node) |

Notes:

- All four ports are TCP; the three P2P ports speak the same gRPC service, so
  if you want a simpler firewall, open `8700-8720` as a range.
- A **single-node local test needs no open inbound ports** — everything stays
  on `127.0.0.1`. Only open ports when other machines must join.
- Local/RPC-only nodes can keep `api` private and only expose the P2P range.
- Nodes bind to `SAN_HOST` (default `127.0.0.1`) and advertise
  `SAN_ADVERTISE_HOST` to peers. For a public node set both, e.g.
  `SAN_HOST=0.0.0.0 SAN_ADVERTISE_HOST=node.example.com`.
- Firewall example (Linux, ufw):

```bash
sudo ufw allow 8000/tcp        # REST API (only if public)
sudo ufw allow 8700:8720/tcp   # gRPC P2P range
```

- Behind NAT/cloud security groups, forward the same ports to the machine
  running the node and set `SAN_ADVERTISE_HOST` to the public hostname.

Sanity check once it is up:

```bash
curl -s http://127.0.0.1:8000/health     # {"status":"ok","height":...}
```

## 2. Install

```bash
git clone <repo> san_network && cd san_network

python3 -m venv .venv
source .venv/bin/activate           # Windows: .venv\Scripts\activate
pip install -r requirements.txt
```

Verify:

```bash
python -m pytest -q        # 77 tests
```

## 3. One-command node

### 3a. Founder (new chain)

```bash
python scripts/run_node.py --address 0xYourRewardAddress
```

What it does:

1. Creates the `node_key.json` identity (if missing).
2. Writes the **genesis premine** (default 1,000,000 SAN to the node's own
   account so it can stake).
3. Sets the **block reward** (default **2 SAN per block**) and routes rewards
   to the address you passed.
4. Starts the chain with an LMDB database and opens the REST + gRPC P2P ports.
5. **Stakes automatically** as a validator (default 1000 SAN).

Expected output:

```
[run_node] founder mode: premine 1000000 SAN to the node identity
[run_node] starting
  reward address : 0x1111111111111111111111111111111111111111
  REST API       : http://127.0.0.1:8000
  P2P (gRPC)     : 127.0.0.1:8700, peer 8710, controller 8720
[run_node] node is up and synced: height=1 peers=0 finalized=0
[run_node] validator stake submitted: {...}
```

### 3b. Joiner (join an existing chain)

```bash
python scripts/run_node.py --address 0xYourRewardAddress \
    --bootstrap 127.0.0.1:8000 \
    --expect-genesis-hash <founder-genesis-hash>
```

Get the hash from the founder node:

```bash
curl -s http://127.0.0.1:8000/genesis | python -c "import sys,json; print(json.load(sys.stdin)['genesis_hash'])"
```

Pinning is optional but recommended. Without it the launcher warns that the
join is trust-on-first-use; with it, both the seed's reported hash and the
locally rebuilt genesis hash are verified and the script exits hard on any
mismatch.

## 4. What happens automatically on startup

```
run_node.py
   |
   +- STEP 1: DISCOVERY
   |     Asks SAN_BOOTSTRAP over gRPC `Bootstrap` (REST /bootstrap fallback),
   |     receives signed peer records and verifies them.
   |
   +- STEP 2: LONGEST CHAIN
   |     Asks every peer for its `Status`: chain id, genesis fingerprint,
   |     height and finality. Chains with a different id/genesis are rejected;
   |     the compatible peer with the highest height is selected.
   |
   +- STEP 3: SYNC
   |     Pages blocks over `Sync` (512 at a time; the first page carries the
   |     state/storage snapshot). Every block is verified (hash, signature,
   |     state root, transactions, round, time) and the ledger is rebuilt by
   |     replaying them. A peer snapshot is never trusted directly, and
   |     finalized history can never be rewritten.
   |
   +- STEP 4: WORK
   |     At the chain tip: block gossip, finality votes, mempool exchange,
   |     mempool pull (getdata), orphan parent fetch and block production
   |     when this node is the expected proposer.
   |
   +- STEP 5: REWARDS
         The block subsidy plus transaction tips are credited to the
         reward_address declared in the signed block header and checked
         against the state root, so every node credits the same address.
```

Example joiner output:

```
[run_node] fetched genesis from http://127.0.0.1:18600 (chain_id=san-devnet-1, 1 allocation(s))
[run_node] local genesis verified against the pinned hash
[run_node] syncing... height=0 (best peer: 13) peers=1
[run_node] syncing... height=8 (best peer: 13) peers=1
[run_node] node is up and synced: height=14 peers=1 finalized=14
```

## 5. Verification commands

```bash
curl -s http://127.0.0.1:8000/health      # height, peers, finality, mempool
curl -s http://127.0.0.1:8000/finality    # finalized checkpoint
curl -s http://127.0.0.1:8000/validators  # active validator set and stake
curl -s http://127.0.0.1:8000/account/0xYourRewardAddress
curl -s http://127.0.0.1:8000/genesis     # chain id + immutable genesis data
```

Balance and rewards:

```bash
python -m sdk.cli --rpc http://127.0.0.1:8000 balance --address 0xYourRewardAddress
python -m sdk.cli --rpc http://127.0.0.1:8000 validators
```

## 6. Useful flags

| Flag | Meaning |
|------|---------|
| `--address 0x...` | Reward address (required) |
| `--key node_key.json` | Identity file (generated when missing) |
| `--stake 1000` | Automatic validator deposit (0 = skip) |
| `--stake 0` | Join as an observer/RPC node only |
| `--block-reward 2` | Reward per block (founder mode only) |
| `--api-port 8000` | REST port (P2P ports derive as +700/+710/+720) |
| `--db data/node.kv` | LMDB database file |
| `--chain-id san-devnet-1` | Chain id (joiners get it from the seed) |
| `--bootstrap host:port` | Seed REST endpoint |
| `--expect-genesis-hash <hash>` | Pin the seed's genesis (recommended) |

All settings can also be provided as `SAN_*` environment variables; values
already present in the environment win over the script defaults. In joiner
mode the script **refuses to start** when genesis-affecting variables
(`SAN_CHAIN_ID`, `SAN_GENESIS_ALLOCATION`, `SAN_BLOCK_REWARD`, ...) are set,
so a different local genesis can never be built silently.

## 7. Running multiple nodes

```bash
# node 1 (founder, port base 8000)
python scripts/run_node.py --address 0xAAA...

# node 2 (joiner, port base 8001) on the same chain, pinned
HASH=$(curl -s http://127.0.0.1:8000/genesis | python -c "import sys,json; print(json.load(sys.stdin)['genesis_hash'])")
python scripts/run_node.py --address 0xBBB... --api-port 8001 \
    --bootstrap 127.0.0.1:8000 --expect-genesis-hash "$HASH"
```

Each node keeps its own `nodeN.kv` database and its own port block.

## 8. Docker

```bash
docker compose up --build
```

Edit the `SAN_*` variables in `docker-compose.yml` (especially
`SAN_GENESIS_ALLOCATION`, `SAN_KEY_FILE`, `SAN_DB_PATH`). The image runs
`python run.py`; the same environment variables apply as with
`scripts/run_node.py`.

## 9. Troubleshooting

- **`height` stops growing:** if `/validators` is empty nobody has staked. Send
  a `stake` transaction from a funded account (the founder account is premined).
- **No rewards:** check `--address` and whether the chain is advancing. With
  `block_reward > 0` the expected proposer keeps producing empty blocks, so
  rewards accrue with every finalized block.
- **Joiner does not sync:** `--bootstrap` must be the seed's **REST** port
  (`host:api_port`). With TLS enabled, `SAN_TLS_CA` must be configured or the
  handshake is rejected.
- **Genesis mismatch:** either the pinned hash is wrong or a genesis-affecting
  environment variable is set. The launcher reports this and refuses to run.
- **Disk growth:** tune `SAN_PRUNE_KEEP` (prunes behind finality) together with
  `SAN_SNAPSHOT_INTERVAL`; pruning only happens when a snapshot anchor exists.
