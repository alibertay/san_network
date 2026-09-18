# Public devnet runbook

This is the operator and joiner runbook for a public SAN devnet: 10 seed VPSs
run by the operator, then strangers join with their own nodes. It assumes the
Go node (`cmd/sanup`, `cmd/sannode`, `cmd/sancli`) and the `deploy/` artifacts.

Related reading:

- [README](../README.md#public-devnet-on-a-vps) — quick reference.
- [INSTALL.md](../INSTALL.md#11-public-devnet-on-a-vps-go) — installer details.
- [docs/gossip.md](gossip.md) — discovery, addrman, DNS seeds, ports.
- [docs/security-review.md](security-review.md) — residual risks.

Contents:

1. [Operator runbook](#1-operator-runbook)
2. [Shared devnet CA](#2-shared-devnet-ca)
3. [DNS seeds and ports](#3-dns-seeds-and-ports)
4. [Systemd, monitoring and upgrades](#4-systemd-monitoring-and-upgrades)
5. [Recommended `san.env` for seeds](#5-recommended-sanenv-for-seeds)
6. [Publishing the seed list](#6-publishing-the-seed-list)
7. [Faucet](#7-faucet)
8. [Joiner runbook](#8-joiner-runbook)
9. [Security notes](#9-security-notes)
10. [Canonical genesis and the public-devnet profile](#10-canonical-genesis-and-the-public-devnet-profile)

---

## 1. Operator runbook

Ten VPSs (2 vCPU / 4 GB / 40 GB is plenty for a devnet) with a public IPv4
(and ideally IPv6) each:

1. **Prepare one machine as the CA machine.** It can be a seed, but treating it
   as the "certificate authority" keeps `ca.key` off the other nine.
2. **Create the shared CA once and sign one certificate per node** (section 2).
3. **Install the node on every machine** with the one-command installer. Use
   `--no-cert` on a public devnet: the installer's default certificate flow
   creates a fresh CA per machine, which is only right for a single-host test.
   `--no-systemd` keeps the service stopped until the certificates are in
   place:

   ```bash
   git clone <repo> san_network && cd san_network
   sudo bash deploy/install.sh --no-cert --no-systemd --advertise-host seed1.example.com
   sudo install -d -m 0750 /etc/san/certs
   sudo install -m 0644 ca.crt /etc/san/certs/ca.crt
   sudo install -m 0644 node.crt /etc/san/certs/node.crt
   sudo install -m 0600 node.key /etc/san/certs/node.key
   sudo chown -R san:san /etc/san/certs
   sudo systemctl enable --now san-node
   ```

   The first seed founds the chain (`SAN_SEED=true`, or leave `auto` so the
   first machine that finds no reachable seed founds it). Install the other
   nine with `SAN_SEED=false` and point them at the seed list.
4. **Publish DNS records** for the seed list (section 3).
5. **Firewall** the four ports (section 3); expose REST `8000` at least on the
   seeds (joiners fetch `/genesis` from them) and only behind a token/reverse
   proxy anywhere else.
6. **Start the founder** (`SAN_SEED=true`, `SAN_STAKE=1000`, DNS seeds empty),
   verify height climbs, then start the other seeds.
7. **Fund a faucet node** (section 7) if you want to hand out SAN to new
   wallets; keep the limits tight.
8. **Monitor** with `/metrics` and `journalctl` (section 4).

Operator command shape (repeat per machine, adjusting host names and ports):

```bash
sanup --foreground \
    --data-dir /var/lib/san --key-file /var/lib/san/san_key.json \
    --host 0.0.0.0 --api-host 127.0.0.1 \
    --advertise-host seed1.example.com \
    --seeds seed.example.com \
    --tls-cert /etc/san/certs/node.crt \
    --tls-key /etc/san/certs/node.key \
    --tls-ca /etc/san/certs/ca.crt \
    --api-token "$SAN_API_TOKEN" \
    --stake 1000
```

`--foreground` is what the systemd unit runs (`Type=simple`); without it
`sanup` detaches a child and `sanup --status`/`--stop` manage it.

---

## 2. Shared devnet CA

One CA signs every node certificate, so every node trusts every peer with a
single `ca.crt` and no per-node pinning. The CA private key never leaves the CA
machine.

```bash
# --- on the CA machine, once ---
sanup cert --ca-only --dir /etc/san/ca
#   ca.crt 0644: copy to EVERY node (SAN_TLS_CA)
#   ca.key 0600: never leaves this machine

# --- per node (run on the CA machine, copy node.* to the target) ---
sanup cert --ca-dir /etc/san/ca --dir /etc/san/certs \
    --advertise-host seed1.example.com
scp /etc/san/certs/node.crt /etc/san/certs/node.key root@seed1:/etc/san/certs/

# distribute the trust anchor to every node
scp /etc/san/ca/ca.crt root@seed1:/etc/san/certs/ca.crt
```

Rules:

- `node.crt` + `node.key` go **only** to the node they were signed for (the
  SANs name that node; `node.key` is 0600).
- `ca.crt` goes to **every** node; `ca.key` **never** leaves the CA machine.
- Re-issuing later uses the same commands; if no `--ca-dir` is given and
  `<dir>/ca.crt` + `<dir>/ca.key` already exist, `sanup cert` reuses the
  existing CA instead of rotating it.
- `SignNodeCert` refuses a missing CA or a key that does not match `ca.crt`.
- Certificates are ECDSA P-256, node certs valid 2 years, CA 10 years.

Start each node with the three TLS flags (or `SAN_TLS_CERT`/`KEY`/`CA` in
`san.env`). `sanup` records TLS in `<data-dir>/sanup.json`, so later
`--status`/`--stop` calls do not need the flags.

---

## 3. DNS seeds and ports

### DNS seed records

Point one name at the seed IPs; nodes resolve it at startup and every 5
minutes (`internal/netnode/dnsseed.go`), taking every A/AAAA record as a
candidate on the **peer** port (`SAN_PEER_PORT`, default 8770).

```dns
seed.example.com.   300  IN  A      203.0.113.10
seed.example.com.   300  IN  A      203.0.113.11
seed.example.com.   300  IN  A      203.0.113.12
; ... one A/AAAA per seed
seed.example.com.   300  IN  AAAA   2001:db8::10
```

- `SAN_DNS_SEEDS=seed.example.com` activates wide-area discovery without
  listing IPs; a `host:port` entry overrides the port.
- DNS seeds are a centralization point: keep `SAN_BOOTSTRAP` (explicit
  `host:port` fallbacks) and the persisted address cache
  (`<data-dir>/peers-cache.json`) as backups.
- A bare `--seeds host` also means REST `host:8000` for the genesis fetch.

### Ports and firewall

| Port | Default | Protocol | Purpose | Exposure |
|------|---------|----------|---------|----------|
| `p2p` | 8765 | gRPC/TLS | block gossip, paged sync, status | public on every seed/node |
| `peer` | 8770 | gRPC/TLS | peer sessions, peer-list exchange | public on every seed/node |
| `controller` | 8769 | gRPC/TLS | signed block-vote requests | public (same service) |
| `api` | 8000 | HTTP(S) | REST API: `/genesis`, `/health`, `/bootstrap`, `/metrics`, `/faucet` | at least the seeds must be reachable by joiners; otherwise token + reverse proxy |
| `ca.key` | — | — | CA private key | **never** exposed/networked |

The three gRPC ports serve the same service, so one range rule works:

```bash
sudo ufw allow 8765:8770/tcp        # P2P range (required on seeds)
sudo ufw allow 8000/tcp             # REST: seeds need this reachable
# nftables equivalent
sudo nft add rule inet filter input tcp dport 8765-8770 accept
```

Behind NAT/cloud security groups, forward the same ports and set
`--advertise-host` to the public DNS name or IP; peers dial the advertised
ports. **Joiners need their own P2P ports reachable for inbound peers** (or at
least for the outbound connections they initiate), and their advertised host
must resolve back to them.

### Genesis fetch and trust

A joiner fetches `GET /genesis` from the first reachable seed
(`--seeds`/`--bootstrap`/peer cache, in that order), then adopts the genesis
allocation, chain id and consensus parameters into `SAN_*` variables before
its own node starts. Sync only accepts blocks whose chain id and genesis
fingerprint match, so a wrong or tampered genesis fails loudly instead of
forking silently — but the fetch itself is only as trustworthy as the
transport: **use TLS + the shared CA on seeds**, or a MITM could serve a
different genesis (see section 9).

---

## 4. Systemd, monitoring and upgrades

```bash
# state
systemctl status san-node
journalctl -u san-node -f                     # node logs (stderr)
sanup --data-dir /var/lib/san --status        # height, peers, balance, stake
sanup --data-dir /var/lib/san --json --status # machine-readable

# metrics (Prometheus text format)
curl -s http://127.0.0.1:8000/metrics | grep -E '^san_(height|peers|validators|mempool)'

# upgrade (idempotent installer: rebuild + restart)
git pull && sudo bash deploy/install.sh

# stop / uninstall
sanup --data-dir /var/lib/san --stop          # SIGTERM, wait, then SIGKILL
sudo bash deploy/install.sh --uninstall       # keep data; --purge removes it
```

Suggested alerts: `san_height` not advancing for > 2 minutes, `san_peers` at
0 for > 5 minutes, finalized height stalled, disk usage of the data
directory, and systemd unit restarts.

---

## 5. Recommended `san.env` for seeds

```ini
SAN_PUBLIC_DEVNET=1           # enforce the public safety profile
SAN_GENESIS_FILE=/etc/san/genesis.json   # canonical genesis (same on every node)
SAN_SEED=auto                 # first machine founds; others join via seeds
SAN_DATA_DIR=/var/lib/san
SAN_HOST=0.0.0.0
SAN_API_HOST=127.0.0.1        # reverse-proxy the REST port on seeds
SAN_API_PORT=8000
SAN_ADVERTISE_HOST=seed1.example.com
SAN_DNS_SEEDS=seed.example.com
SAN_PEERS_CACHE=/var/lib/san/peers-cache.json
SAN_PEER_REGISTRY=/var/lib/san/peers.json
SAN_CHAIN_ID=san-devnet-1
SAN_STAKE=1000
SAN_DB_BACKEND=lmdb
SAN_DB_PATH=/var/lib/san/node.db
SAN_API_TOKEN=<long-random-token>
SAN_TLS_CERT=/etc/san/certs/node.crt
SAN_TLS_KEY=/etc/san/certs/node.key
SAN_TLS_CA=/etc/san/certs/ca.crt
SAN_OUTBOUND_PEERS=8
SAN_MAX_ADDR_ENTRIES=1024
SAN_RPC_RATE_LIMIT=120
# optional public faucet, see section 7 (requires SAN_API_TOKEN)
# SAN_FAUCET=1
# SAN_FAUCET_AMOUNT=10
# SAN_FAUCET_MAX=100
# SAN_FAUCET_COOLDOWN=60
```

`deploy/install.sh` writes this file from `deploy/san.env.example` (with
`@DATA_DIR@`/`@CONFIG_DIR@` substituted). Every option has a `SAN_*`
environment equivalent; explicit command-line flags win.

---

## 6. Publishing the seed list

Publish at least the DNS seed name plus a plain-text/machine-readable list, so
joiners can fall back if DNS is censored:

```text
# seed.example.com — SAN devnet san-devnet-1
seed1.example.com   203.0.113.10   8765/8770/8000
seed2.example.com   203.0.113.11   8765/8770/8000
...
genesis_hash=<hash from GET https://seed1.example.com/genesis>
chain_id=san-devnet-1
ca_sha256=<sha256 of ca.crt>
```

Keep the list in the repository or a website; announce the chain id and
`ca.crt` fingerprint so joiners can verify what they downloaded. Publish
`ca.crt` itself alongside the list.

---

## 7. Faucet

Any node can act as a faucet: it signs a normal transfer from its own identity
through the ordinary mempool path, so the funded transaction behaves exactly
like a user transfer.

```ini
SAN_FAUCET=1
SAN_FAUCET_AMOUNT=10     # default when the request omits an amount
SAN_FAUCET_MAX=100       # hard cap per request
SAN_FAUCET_COOLDOWN=60   # seconds per address and per IP
```

```bash
# enable at start (or set SAN_FAUCET=1 in san.env)
sanup --faucet --faucet-amount 10 --faucet-max 100 --faucet-cooldown 60 ...

# POST /faucet {"address":"0x...","amount":10}
curl -s -X POST https://seed1.example.com/faucet \
     -H 'Content-Type: application/json' \
     -H "Authorization: Bearer $SAN_API_TOKEN" \
     -d '{"address":"0x0123...","amount":10}'
# -> {"amount":"10","status":"pooled","tx_id":"..."}

# or through the CLI
sanup faucet --to 0x0123... --amount 10 --rpc https://seed1.example.com \
     --tls-ca /etc/san/certs/ca.crt --api-token "$SAN_API_TOKEN"
```

Controls built in: `SAN_FAUCET=1` required (default off, otherwise 404),
address validation (`ledger.IsValidAddress`), `amount <= SAN_FAUCET_MAX`,
per-address cooldown, per-IP sliding-window rate limit, `SAN_API_TOKEN` when
set (constant-time bearer compare, 401 otherwise), and the node's own balance
check. The faucet spends the node's liquid balance; fund the faucet node with
only what you are willing to give away, and watch `san_balance`-style alerts.

---

## 8. Joiner runbook

1. **Install**: `sudo bash deploy/install.sh --advertise-host mynode.example.com`
   (or build manually and run `sanup` directly).

2. **Get your node certificate** from the operator's CA machine (section 2) —
   never send your `node.key` anywhere. Put `node.crt`/`node.key` in
   `/etc/san/certs/` and `ca.crt` next to them.

3. **Start the node** with the seed list, your public host and TLS:

   ```bash
   sanup --seed false \
       --host 0.0.0.0 --api-host 127.0.0.1 \
       --advertise-host mynode.example.com \
       --seeds seed.example.com \
       --genesis-file /etc/san/genesis.json \
       --data-dir /var/lib/san \
       --tls-cert /etc/san/certs/node.crt \
       --tls-key /etc/san/certs/node.key \
       --tls-ca /etc/san/certs/ca.crt
   ```

   Or in `/etc/san/san.env`: `SAN_SEED=false`,
   `SAN_DNS_SEEDS=seed.example.com`, `SAN_ADVERTISE_HOST=mynode.example.com`,
   `SAN_GENESIS_FILE=/etc/san/genesis.json`, `SAN_PUBLIC_DEVNET=1`.

4. **NAT / advertise host**: forward `8765`, `8770` and `8769` TCP on your
   router to this machine, and make `--advertise-host` resolve to your public
   IP. Without a reachable advertised host, peers can still be dialed
   outbound, but inbound peers (and controller votes) will not find you.

5. **Genesis adoption is pinned**: with `--genesis-file
   /etc/san/genesis.json` (or `SAN_GENESIS_FILE`) the launcher loads the
   canonical genesis, verifies the seed's reported `genesis_fingerprint`
   matches it, and passes the chain id, allocation and consensus parameters to
   the node. A seed on another chain is rejected before the node starts; the
   node repeats the check in HELLO and before every sync. Without the flag
   (dev-mode only) it falls back to trust-on-first-use: the first reachable
   seed's `/genesis` is adopted (`--seeds` → peer cache → local registry).

6. **Verify**:

   ```bash
   sanup --data-dir /var/lib/san --status
   #   height           : 123 (finalized 120)
   #   peers            : 8 (validators 10)
   curl -k https://127.0.0.1:8000/health   # or --tls-ca /etc/san/certs/ca.crt
   ```

   `peers >= 1` and a climbing `height` mean discovery and sync work.

7. **Troubleshooting**:
   - **`chain_id`/genesis mismatch**: the node refuses to start or sync when
     the persisted chain was created with a different genesis. Delete the data
     directory only if you are sure you do not need that chain, then re-join;
     otherwise check that you are pointing at the right seed and that you
     copied the published `ca.crt` (a MITM with a different genesis would
     produce exactly this mismatch).
   - **No peers**: check the firewall/NAT rules, that
     `--advertise-host` resolves, and the node log
     (`journalctl -u san-node -f`) for `gRPC bootstrap ... failed`.
   - **TLS handshake failures**: verify `ca.crt` matches the published
     fingerprint and that your certificate SANs include `--advertise-host`.

### Phase 1 without TLS (trade-off)

For a phase-1 devnet you can join with plaintext gRPC and REST by omitting
`--tls-*`. It is simpler (no CA distribution, no certificate SANs to get
right) but everything — including the genesis fetch and API tokens if you use
HTTP — is readable and modifiable in transit; a MITM can serve a different
genesis and eclipse you from the real network. Prefer TLS with the shared CA
for anything public, and treat plaintext as a short-lived test mode.

---

## 9. Security notes

- **Genesis trust follows the transport.** The joiner believes the first
  reachable seed's `/genesis`. Over HTTPS with `--tls-ca ca.crt` that response
  is authenticated against the operator's shared CA; over plain HTTP (or
  `https` with verification skipped) a MITM can serve a different allocation,
  chain id or consensus parameters. With `--genesis-file` the fetched values
  must match the pinned fingerprint before adoption, and the node re-checks the
  fingerprint in HELLO, in peer records and before sync, so a MITM cannot move
  an already-pinned node to another chain. Publish the `ca.crt` fingerprint and
  the genesis fingerprint so joiners can cross-check.
- **`SAN_API_TOKEN`**: required for a public REST port. Tokens are compared in
  constant time; use a long random value, keep it out of shell history
  (`SAN_API_TOKEN` in `san.env`), and put the API behind a reverse proxy if it
  must be broadly reachable.
- **Faucet abuse controls**: opt-in `SAN_FAUCET=1`, per-request cap, address
  cooldown, per-IP rate limit and an optional token. The faucet balance is the
  node's own money — fund a dedicated node.
- **CA hygiene**: `ca.key` never leaves the CA machine; `node.key` is 0600 and
  unique per node. Rotating the CA invalidates every node certificate
  (peers must all receive the new `ca.crt`); prefer re-signing node certs with
  the stable CA.
- **No mTLS**: peer certificates are verified against the CA but not pinned to
  node identities; any holder of a CA-signed certificate can open a peer
  session. Keep the CA machine and `ca.key` offline/limited.
- The full residual-risk list lives in
  [docs/security-review.md](security-review.md); secret handling (key files,
  CA key isolation, API tokens, rotation) is in [docs/secrets.md](secrets.md).

---

## 10. Canonical genesis and the public-devnet profile

### 10.1 The canonical genesis file

`deploy/genesis.json` (installed to `/etc/san/genesis.json`) is the frozen
starting point for a network:

```json
{
  "format_version": 1,
  "chain_id": "san-devnet-1",
  "allocations": {},
  "parameters": {
    "block_reward": "2",
    "min_block_interval_ms": 1000,
    "proposer_timeout_ms": 6000,
    "block_gas_limit": 30000000,
    "unbonding_period": 100,
    "slash_bps": 5000,
    "min_validator_stake": "0"
  },
  "validators": [],
  "fingerprint": "18316ae0ba90922e943010858a5de447bc32636453701321b6ab351c72a8ccb8"
}
```

* Allocations are address (or public key) → SAN amount. The shipped file has no
  premine: validators earn block rewards. **Add the operator/founder
  allocations before freezing** if the network needs a premine.
* `parameters` must stay identical on every node; a changed parameter changes
  the fingerprint and makes the node reject the network.
* `validators` is an optional bootstrap list (addresses or public keys)
  included in the fingerprint for operator coordination; it does not create
  on-chain stake.
* The file declares its own `fingerprint`; loading rejects an edit that was not
  followed by a regenerated fingerprint (`genesis.Load` → `Parse`).
* Every node starts with `--genesis-file`/`SAN_GENESIS_FILE`; `sanup --status`
  and the node's startup log print the chain id, genesis block hash and
  fingerprint. `GET /genesis` returns `genesis_fingerprint` so operators can
  compare nodes. The handshake, peer records and sync all compare it
  (`docs/protocol.md`).

### 10.2 Dev vs public profile

| Concern | Dev/bootstrap (default) | Public devnet (`SAN_PUBLIC_DEVNET=1`) |
|---------|-------------------------|----------------------------------------|
| Database | `SAN_DB_BACKEND=memory` fallback | persistent backend + `SAN_DB_PATH` required |
| Controllers | forced to 0, no quorum | floor `SAN_CONTROLLER_MIN_COUNT` (default 3) or explicit opt-out |
| API auth | optional | `SAN_API_TOKEN` required unless the API binds loopback |
| Faucet | opt-in, may run tokenless on a laptop | refused without `SAN_API_TOKEN` |
| Genesis | dynamic founder premine allowed | pinned fingerprint required (`--genesis-file`/`SAN_GENESIS_FINGERPRINT`) |
| Peers | local registry/bootstrap optional | DNS seeds/bootstrap/cache required |
| Handshake | strict protocol 3 (legacy opt-in for tests) | legacy handshake refused |
| On failure | permissive | startup fails with an actionable error |

`SAN_ALLOW_INSECURE_PUBLIC=1` (or `sanup --allow-insecure-public`) downgrades
the profile failures to warnings for deliberate test deployments. It does not
downgrade a genesis fingerprint **mismatch**, and it must never be set on a
public node.
