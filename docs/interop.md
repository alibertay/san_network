# Go/Python interoperability policy and harness

## Policy

* The **Go node is the canonical protocol implementation** (`internal/netnode`,
  `cmd/sannode`). Bug fixes and new wire behavior land there first.
* The **Python implementation** (`network/Node.py`, `run.py`, `app/`) is the
  **reference implementation and fixture source**. It is frozen for features;
  it is not modified by the Go work.
* Genuine wire compatibility is a tested property, not a claim: the harness in
  `internal/netnode/interop_live_test.go` (`//go:build interop`) starts mixed
  nodes and compares consensus-visible state. Production compatibility is only
  claimed for the checks listed as verified below. Anything not in the matrix
  is explicitly out of policy scope until it is added to the harness.

## Running the harness

```sh
# All interop scenarios (Go mesh + live Python when available)
go test -tags interop ./internal/netnode -run TestInterop -v -timeout 15m

# Go-only fallback (no Python needed)
go test -tags interop ./internal/netnode -run TestInteropGoMesh -v
```

The Python-side tests skip cleanly when no `python`/`python3` with
`fastapi`, `uvicorn` and `grpcio` is available. The Python node is spawned as
`python run.py` from the repository root with a sanitized environment
(`SAN_HOST=127.0.0.1`, ephemeral ports, `SAN_ADVERTISE_HOST=127.0.0.1`,
in-memory DB, TLS off) and killed when the test ends.

The harness builds its Go nodes with `AllowLegacyHandshake = true`
(`interopConfig`): the frozen Python reference speaks protocol 2, while Go
protocol 3 requires the full genesis fingerprint, software version and
capability list in HELLO. `SAN_ALLOW_LEGACY_HANDSHAKE=1` is the documented,
explicit compatibility window (`docs/protocol.md`) and is refused by the
public-devnet profile. Python files are never modified by the harness.

## Verified matrix

| Scenario | Test | Status |
|----------|------|--------|
| Go mesh: genesis + chain id agreement | `TestInteropGoMesh/genesis_and_chain_id_agree` | verified |
| Go mesh: handshake + status | `TestInteropGoMesh/handshake_and_status` | verified |
| Go mesh: tx + block propagation, `tx_root`/`state_root` equality | `TestInteropGoMesh/tx_and_block_propagation` | verified |
| Go mesh: stake deposit propagation | `TestInteropGoMesh/stake_deposit_propagates` | verified |
| Go mesh: finalized height/hash equality | `TestInteropGoMesh/finalized_height_and_hash_agree` | verified |
| Go mesh: contract deploy + query on a synced peer | `TestInteropGoMesh/contract_deploy_propagates` | verified |
| Go mesh: catch-up after restart | `TestInteropGoMesh/sync_catch_up_after_restart` | verified |
| Go mesh: shared-store restart | `TestInteropGoMesh/shared_store_restart` | verified |
| Go seed + Python joiner: genesis hash, handshake, transfer block, `state_root` | `TestInteropGoSeedPythonJoins` | verified |
| Python seed + Go joiner: genesis hash, `/bootstrap` adoption, deposit + transfer blocks | `TestInteropPythonSeedGoJoins` | verified |
| Three mixed nodes (Go seed, Go joiner, Python joiner) converge on one tip/root | `TestInteropThreeMixedNodes` | verified |
| SANRC20 token transfer across implementations | - | not automated yet |
| Stake undelegate/withdraw across implementations | - | not automated yet |
| Governance transaction across implementations | - | not automated yet |
| Finality-vote gossip across implementations | - | not automated yet (Go mesh votes are cast in-process) |

Verified on Windows (Go 1.26, Python 3.13 with grpcio/fastapi/uvicorn
installed); the live tests use loopback only.

## Known interop asymmetries (unresolved)

* **Interop-1 - Python `Bootstrap` RPC omits its own record.** The Python
  `Bootstrap` servicer returns only `{"peers": PEERS}`; the Go servicer appends
  its own signed `SelfPeerRecord`. A Go node bootstrapping against a fresh
  Python seed therefore gets an empty peer list from gRPC. The harness points
  the Go joiner at the Python **REST API port**, where Go's `/bootstrap`
  fallback (`DiscoverPeers`) adopts the signed record. Regression:
  `TestInteropPythonSeedGoJoins` (subtest `pure_grpc_bootstrap_unsupported` is
  a documented skip).
* **Bootstrap-proposer race.** While no validators are staked both
  implementations may propose (bootstrap fallback) and each gossips its own
  block for the same transaction, producing two valid competing branches at
  the same height. The harness stakes one identity before submitting traffic;
  production devnets must stake validators before enabling traffic.
* **Python has no Go-only discovery features.** The address manager, outbound
  slots, peer scoring/caps and the local registry are Go-only; no wire message
  changed, so mixed gossip still works, but only Go nodes benefit from them.
* **Python cannot read the Go peer cache**, so a restarted Python node relies
  on `SAN_BOOTSTRAP`/seeds again.
