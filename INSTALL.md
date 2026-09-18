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

## 10. Go implementation (optional)

The Go port exposes the same `SAN_*` variables, ports and files as the Python
node, so the configuration above applies unchanged.

```bash
go build ./...                                          # needs Go 1.26+
go run ./cmd/sanup --wallet 0xYourAddress --stake 100   # one-command devnet launcher (no Python)
go run ./cmd/sanup --status                             # height, peers, balance, stake
go run ./cmd/sanup --stop
go run ./cmd/sane2e                                     # 3-node end-to-end check (exits non-zero on failure)
go run ./cmd/sannode --address 0xYourRewardAddress      # = python scripts/run_node.py
SAN_DB_BACKEND=memory go run ./cmd/sannode serve        # = python run.py
go run ./cmd/sancli --rpc http://127.0.0.1:8000 health  # = python -m sdk.cli
```

`sanup` nodes discover each other automatically through `~/.san/peers.json`
(override with `SAN_PEER_REGISTRY`): start a second node in another data
directory and it joins the first chain without `--bootstrap`. See the README
for the flags.

The default build is cgo-free and only has the in-memory backend; set
`SAN_DB_BACKEND=memory` or build with LMDB support (the bundled LMDB only
needs a C compiler):

```bash
CGO_ENABLED=1 go build -tags lmdb -o sannode ./cmd/sannode
```

Docker (multi-stage, LMDB backend, distroless non-root runtime):

```bash
docker build -f deploy/Dockerfile -t san-network-go .
docker run -d -p 8000:8000 -p 8765:8765 -p 8769:8769 -p 8770:8770 \
    -v san-go-data:/var/lib/san san-network-go
```

The optional Compose service runs the built image beside the Python node:

```bash
docker compose --profile go up -d --build san-node-go   # API on host port 18000
```

See [GO_MIGRATION.md](GO_MIGRATION.md) for the Python-to-Go package map and
the parity fixture workflow.

## 11. Public devnet on a VPS (Go)

The Go node is the implementation for a public devnet (the Python tree is the
frozen reference). Discovery uses DNS seeds + explicit bootstrap addresses +
a persisted address manager; the same-machine registry is only a fallback.
Full walkthrough: [README](README.md#public-devnet-on-a-vps).

**Open TCP ports** (defaults): `api` 8000, `p2p` 8765, `peer` 8770,
`controller` 8769. The three gRPC ports serve the same service, so one range
rule (`8765-8770`) is enough. Keep 8000 private unless the REST API must be
public (then always set `SAN_API_TOKEN`, ideally behind a reverse proxy).
Behind NAT/cloud security groups forward the same ports and advertise the
public name or IP.

### 11.1 Install (systemd, one command)

```bash
git clone <repo> san_network && cd san_network
sudo bash deploy/install.sh --advertise-host node1.example.com
```

`install.sh` builds `sannode`/`sanup`/`sancli` for the host architecture with
the cgo LMDB backend (`-tags lmdb` when gcc is available), installs them to
`/usr/local/bin`, creates the `san` system user and `/var/lib/san` (0700) +
`/etc/san`, generates a devnet CA/node certificate, writes
`/etc/san/san.env` from `deploy/san.env.example`, and installs + starts the
`san-node.service` systemd unit. It is idempotent: re-run it to upgrade.
Useful flags: `--prefix DIR`, `--data-dir DIR`, `--config-dir DIR`,
`--user NAME`, `--no-cert`, `--no-systemd`, `--uninstall [--purge]`.

```bash
sudo bash deploy/install.sh --advertise-host seed.example.com   # seed / founder
sudo bash deploy/install.sh --advertise-host node2.example.com  # joiner
$EDITOR /etc/san/san.env     # SAN_SEED=true|false, SAN_DNS_SEEDS=..., SAN_STAKE=...
sudo systemctl restart san-node
```

All `sanup` options are read from `SAN_*` variables, so
`/etc/san/san.env` is the single source of truth under systemd. The unit runs
`sanup --foreground` (`Type=simple`), so `SIGTERM` stops the node gracefully
(peer cache flushed, registry entry removed) before systemd's stop timeout.

### 11.2 Run without systemd

```bash
# seed node (new chain)
sanup --seed --host 0.0.0.0 --api-host 0.0.0.0 \
    --advertise-host seed.example.com --data-dir /var/lib/san/seed

# joining node (wide-area discovery; peers via the seed, genesis fetched
# from its REST port 8000)
sanup --host 0.0.0.0 --api-host 0.0.0.0 \
    --advertise-host node2.example.com --seeds seed.example.com \
    --data-dir /var/lib/san/node2
```

`--seeds` accepts DNS names or `host:port`; `--bootstrap host:api_port` pins
the genesis source explicitly. The address cache
(`<data-dir>/peers-cache.json`, `--peer-cache FILE`) reconnects a restarted
node without a seed. `--no-registry` disables the local registry.

### 11.3 TLS

`deploy/install.sh` already generates `/etc/san/certs/{ca,node}.crt|key` and
enables `SAN_TLS_*` in `san.env`. For manual setups:

```bash
sanup cert --dir /etc/san/certs --advertise-host node2.example.com
# copy ca.crt to every machine, then add to each node:
#   --tls-cert /etc/san/certs/node.crt --tls-key /etc/san/certs/node.key \
#   --tls-ca /etc/san/certs/ca.crt
```

With TLS the REST API also speaks HTTPS; `sanup --status`/`--stop` detect
that from the state file, and `sanup cert` writes the node/CA keys 0600.

### 11.4 Operations

```bash
systemctl status san-node
journalctl -u san-node -f                 # node logs (stderr)
sanup --data-dir /var/lib/san --status    # height/peers/balance (add --api-token)
sanup --data-dir /var/lib/san --stop      # SIGTERM, wait up to 15s, then SIGKILL
sudo bash deploy/install.sh --uninstall        # keep data; add --purge to remove it
```

**Firewall** (ufw / nftables):

```bash
sudo ufw allow 8765:8770/tcp              # P2P range
sudo ufw allow 8000/tcp                   # REST API only if public
# nftables equivalent
sudo nft add rule inet filter input tcp dport 8765-8770 accept
```

**Persistence:** production VPS builds should use LMDB
(`CGO_ENABLED=1 go build -tags lmdb`, which `install.sh` does when gcc is
present). Without the tag, `SAN_DB_BACKEND=lmdb` fails with an actionable
error suggesting `-tags lmdb` or `SAN_DB_BACKEND=memory`. The Docker image
(`deploy/Dockerfile`) also builds with the LMDB backend.

On Windows the launcher still uses `taskkill /F`, so the peer cache is
flushed by the node every 60 s instead of on exit; Linux/macOS get the
graceful SIGTERM path.
