# Go Migration

The SAN Network node has a full Go implementation that lives next to the
Python reference implementation in this repository. The Python code is the
reference: the Go packages are verified against golden fixtures generated
*from* Python (`tools/parity_fixtures.py`), and Python stays in the tree for
cross-checking.

## Phase plan

1. **Foundation** — canonical JSON, post-quantum crypto, addresses,
   transactions/blocks, merkle proofs, economics and the SANVM. Golden
   fixtures cover every pure function.
2. **Persistence and networking** — key-value stores (memory + LMDB),
   `ChainStore` block/state persistence, the generated gRPC protocol and the
   node (peers, gossip, consensus, sync).
3. **Interfaces** — the REST API, the Go SDK and the `sanup` / `sane2e` /
   `sannode` / `sancli` / `sangenesis` commands.
4. **Packaging and operations** — the cgo LMDB backend (`-tags lmdb`), CI
   jobs, the Go Docker image and this document.

## Package map

| Python (reference) | Go |
|--------------------|----|
| `utils/canonical.py` | `internal/canonical` |
| `blockchain/crypto.py`, `blockchain/identity.py` | `internal/crypto`, `internal/ledger` |
| `blockchain/address.py` | `internal/ledger` (`address.go`) |
| `blockchain/Transaction.py`, `blockchain/Block.py` | `internal/ledger` (`transaction.go`, `block.go`) |
| `blockchain/Blockchain.py` | `internal/ledger` (`blockchain.go`) |
| `blockchain/economics.py`, `blockchain/merkle.py` | `internal/ledger` (`economics.go`, `merkle.go`) |
| `blockchain/persistence.py` | `internal/ledger` (`persistence.go`) |
| `blockchain/storage/` (base, memory, LMDB) | `internal/ledger/store` |
| `SANVM/` (VM, assembler, PENA parser, gas, storage) | `internal/sanvm` |
| `network/config.py`, `network/Node.py`, `network/transport.py` | `internal/netnode` |
| `network/proto/p2p.proto` | `internal/netproto` (generated) |
| `app/` (main, routes, limits, metrics) | `internal/api` |
| `sdk/client.py` | `internal/sdk` |
| `sdk/cli.py` | `cmd/sancli` |
| `run.py`, `scripts/run_node.py` | `cmd/sannode` |
| `scripts/genesis_bootstrap.py` | `cmd/sangenesis` |
| `scripts/go_node.py` (deleted) | `cmd/sanup` |
| `tools/go_e2e_check.py` (deleted) | `cmd/sane2e` |
| local peer discovery | `internal/netnode` (`discovery.go`, local registry `~/.san/peers.json`) |

## Documentation

The protocol internals are documented in `docs/`:

- [docs/chaos-limited-consensus.md](docs/chaos-limited-consensus.md) — the
  consensus mechanism (proposer rounds and bounded fallback, block validity,
  finality and slashing, governance, fork choice, determinism).
- [docs/gossip.md](docs/gossip.md) — the P2P layer (signed peer records,
  health checks, message types, propagation, chain sync, hardening).

Both documents cover the Go implementation and point out where Python
differs. `README.md` links them from the Go section. For the day-to-day
bring-up workflow (`go run ./cmd/sanup`, `go run ./cmd/sane2e`) see
[Start your Go node](#start-your-go-node) below.

## Parity fixtures

`tools/parity_fixtures.py` runs the Python implementation and writes
`internal/parity/testdata/foundation.json` (canonical encoding and float
formatting, addresses, merkle/state roots, economics, identities,
transactions, blocks, chain replay, chain-store round-trips and SANVM
programs/contracts/errors/asm). The Go tests in `internal/parity` embed the
file through `parity.Foundation()` and compare Go results against it.

Regenerate the fixture after any intentional Python change (never edit it by
hand, and never change Python behavior to satisfy Go):

```bash
python tools/parity_fixtures.py
go test ./internal/parity/ -count=1
```

## Build, test and run

```bash
go build ./...           # cmd/sanup, cmd/sane2e, cmd/sannode, cmd/sancli, cmd/sangenesis
gofmt -l .               # must print nothing
go vet ./...
go test ./... -count=1   # unit tests + parity fixtures + discovery integration test
go run ./cmd/sane2e      # end-to-end devnet check (exits non-zero on failure)

# LMDB backend (cgo; the bundled LMDB only needs a C compiler):
CGO_ENABLED=1 go build -tags lmdb ./...
CGO_ENABLED=1 go test -tags lmdb ./internal/ledger/store/ -count=1
```

The default build has no cgo and therefore no LMDB support: the node falls
back to the in-memory store. Set `SAN_DB_BACKEND=memory`, or build with
`-tags lmdb` for persistence. All `SAN_*` variables and port assignments match
the Python implementation (see `README.md`).

```bash
# one-command devnet node (port of scripts/run_node.py)
go run ./cmd/sannode --address 0xYourRewardAddress

# env-configured server (port of run.py), in-memory backend
SAN_DB_BACKEND=memory go run ./cmd/sannode serve

# wallet/explorer (port of sdk/cli.py)
go run ./cmd/sancli --rpc http://127.0.0.1:8000 health
```

### Start your Go node

`cmd/sanup` is the all-Go single-command launcher (no Python at runtime, no
`scripts/go_node.py`):

```bash
go run ./cmd/sanup --wallet 0xYourAddress            # start, no stake
go run ./cmd/sanup --wallet 0xYourAddress --stake 100
go run ./cmd/sanup --status
go run ./cmd/sanup --stop
```

It creates `<data-dir>/san_key.json` on first run (the wallet must match that
key), funds the wallet at genesis when it founds the chain, picks free ports
when the defaults are busy, and reconciles `--stake` on every start (deposit,
or all-or-nothing undelegate + withdraw + re-stake when lowering). For a
single-machine devnet it defaults to `SAN_DB_BACKEND=memory`,
`SAN_UNBONDING_PERIOD=0`, `SAN_MIN_VALIDATOR_STAKE=0` and
`SAN_CONTROLLER_COUNT=0` (blocks are accepted locally; finality votes still
run).

**Automatic peer discovery.** Nodes publish a signed `SelfPeerRecord` in a
local registry (`~/.san/peers.json`, overridable with `SAN_PEER_REGISTRY`) and
read/verify the other entries while they run. Start a second node with
`--data-dir` and no `--bootstrap`: it finds a reachable registry node, fetches
its genesis and joins the same chain. The discovery loop also probes the
default local REST (8000-8010) and peer (8770-8780) port ranges so plain
`sannode` processes are found too. `--bootstrap host:api_port` still takes
priority, `--seed true|false|auto` overrides founder/joiner detection, and
stopping a node removes its registry entry.

`go run ./cmd/sane2e` is the test-only end-to-end check (all Go): it starts
three nodes through `sanup` (seed + two auto-discovered joiners) and verifies
SAN transfers, a SANRC20 deploy/mint/transfer, a custom KV/counter contract
and stake 0 → 100 → 70, then cleans every process and port up and exits
non-zero on failure (the Python `tools/go_e2e_check.py` and
`scripts/go_node.py` were deleted).

## Python cross-check

Python remains runnable and unchanged:

```bash
python -m pytest tests/test_units.py tests/test_asm.py -q
python tools/parity_fixtures.py
```

## Docker and CI

- `Dockerfile` — Python reference image (unchanged).
- `Dockerfile.go` — executable Go definition of the Go image (distroless
  static, in-memory backend). The Go toolchain parses every root `*.go` file,
  so the file prints the Dockerfile instead of being one:
  `go run Dockerfile.go | docker build -f- -t san-network-go .`.
- `docker compose --profile go up -d san-node-go` — optional Go node beside
  the Python one (host ports 18000 / 18765 / 18769 / 18770).

CI (`.github/workflows/ci.yml`) keeps the Python lint/test/smoke jobs and adds
`go-test` (gofmt, `go vet`, `go test ./...`) and `go-lmdb` (installs
`build-essential`, then `CGO_ENABLED=1 go build -tags lmdb ./...` and the
store round-trip test). The `docker` job builds both images.
