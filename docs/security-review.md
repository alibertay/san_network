# Go Implementation Security Review (First Devnet)

**Scope:** the Go node and tooling that ship the devnet: `cmd/sanup`,
`cmd/sannode`, `cmd/sancli`, `cmd/sane2e`, `internal/netnode`,
`internal/api`, `internal/sdk`, `internal/ledger`, `internal/ledger/store`,
`internal/sanvm`, `internal/crypto`, `internal/canonical`.
The Python tree is the frozen reference and was not modified.

**Method:** automated tooling first, then a manual code review of every
untrusted input path, consensus fallback, resource bound and crypto call
site. Critical/High findings were fixed with regression tests.

**Date:** 2026 (pre-devnet final pass).

---

## 1. Tools run

| Check | Command | Result |
|-------|---------|--------|
| Formatting | `gofmt -l .` | clean |
| Build | `go build ./...` | pass |
| Vet | `go vet ./...` | pass |
| Unit tests | `go test ./... -count=1` | pass |
| Race detector (WSL + gcc, Go 1.26.0 linux) | `go test -race -count=1 ./internal/netnode ./internal/api ./internal/sdk ./internal/ledger/...` | pass (3 races found and fixed, see F1/F2) |
| Static analysis | `go run github.com/securego/gosec/v2/cmd/gosec@latest ./...` | 43 findings, no Critical/High exploitable; triaged below |
| Fuzzing / panic sweeps | `go test -run=^$ -fuzz=FuzzDecode` (canonical), `FuzzAssemble` (sanvm), `FuzzBlockFromDict` (ledger), `FuzzDispatchPeerMessage` (netnode), 8 s each | ~1.8M execs, zero panics/crashes |
| Table-driven panic tests | `internal/canonical/canonical_test.go`, `internal/sanvm/hardening_test.go`, `internal/ledger/hardening_test.go`, `internal/netnode/hardening_test.go`, `internal/api/hardening_test.go` | pass |
| End-to-end | `go run ./cmd/sane2e` | `5 passed, 0 failed` |
| Python reference | `python -m pytest tests/test_units.py tests/test_asm.py -q` | pass (82) |

gosec findings are dominated by informational classes: `G115` integer
conversions (all values originate locally or are range-clamped), `G304`
config-supplied file paths, `G204` `exec.Command` with integer arguments,
`G117` the key file intentionally serializing `private_key`, `G301/G302/G306`
directory/log/executable modes (tightened where it matters), and generated
protobuf `unsafe` usage (`G103`).

---

## 2. Findings

Severity in brackets; `[fixed]` means a change plus regression test landed in
this pass, `[documented]` means reviewed and accepted for a first devnet.

| # | Severity | Finding | Status |
|---|----------|---------|--------|
| F1 | Critical | Data race on `Blockchain.Parameters`: `blockProductionLoop` read `ProposerTimeout()` without `n.mu` while `simulateBlock(apply=true)` wrote the parameter map under `n.mu`. Race detector reproduced it in `-race` runs. | [fixed] `node_peers.go` takes `n.mu`; `node.go` peer-count reads locked. `-race` now clean. |
| F2 | Medium | Test-only race: `discovery_test.go` read `node.PEERS` without the lock. | [fixed] uses `node.Peers()`. |
| F3 | High | Unbounded HTTP request body: the limiter only checked the (spoofable/absent) `Content-Length`, then `io.ReadAll` read forever -> memory DoS. | [fixed] `http.MaxBytesReader` in `internal/api/limits.go`, 413 mapping in `json.go`; tests cover chunked and lying headers. |
| F4 | High | gRPC handlers had no panic recovery. A panic inside `PeerSession`/`Sync`/`Status` (spawned goroutine or handler) would crash the whole node. | [fixed] unary/stream recovery interceptors + `runPeerSession` guard in `internal/netnode/recover.go`, wired in `transport.go`; `recover_test.go` asserts a panicking transport only fails its session. |
| F5 | High | `sanup --stop` killed the PID recorded in `sanup.json` without checking what the process is; a stale/tampered PID could kill an unrelated process. | [fixed] `processImageName` + `isNodeProcess` (`cmd/sanup/proc_*.go`, `status.go`); refuses to kill anything that is not `sanup-node`/the launcher binary. `proc_test.go`. |
| F6 | Medium | Block timestamps were never required to be numeric: a string/object/oversized-int timestamp made `TimestampFloat()` return `NaN`, and every `NaN` comparison is false, silently bypassing median-time, future-drift, past-drift and minimum-interval checks. | [fixed] `Block.HasNumericTimestamp()` + rejection in `verifyBlock`; `*big.Int` handled in `TimestampFloat`; `TestBlockTimestampMustBeNumeric`. |
| F7 | Medium | A key file (or env pair) with a private key that does not match its public key was accepted; the node would advertise a key it cannot sign for. | [fixed] `crypto.KeysMatch` and the check in `ledger.NewIdentity`; `hardening_test.go` covers mismatched file, matching file and round trip. |
| F8 | Medium | Mempool had no cap (`append` from remote TX/TXS); a funded peer could grow it without bound. | [fixed] `SAN_MAX_MEMPOOL` (default 8192) checked in `validateTransactionForMempool` after `prunePool`; divergence from Python documented in code and here. Test asserts the cap. |
| F9 | Medium | REST server had no read/idle timeouts (slow-body connections could be held open). | [fixed] `ReadTimeout` 30 s and `IdleTimeout` 120 s in `internal/api/run.go` (`ReadHeaderTimeout` already existed). |
| F10 | Low | Exported `sanvm.Encode` indexed/asserted `instruction.Operands[0]` unchecked. Not reachable from parser output, but an embedder could panic it. | [fixed] length/type guards; malformed-instruction tests. |
| F11 | Low | Key/registry/data directories were created 0755 and node logs 0644. | [fixed] key dir 0700, registry dir 0700, sanup data dir 0700, log file 0640. |
| F12 | Low | `DEAD_PEER` claims are ignored (only local health checks evict) - verified as intended anti-eclipse behavior. | [documented] |
| F13 | Low | `canonical.Decode` replaces lone UTF-16 surrogates with U+FFFD (encoding/json behavior); Python keeps the surrogate. Signatures over such exotic strings would differ between implementations. Normal Go-only traffic is unaffected. | [documented] |
| F14 | Low | `\U` literals above `0x10FFFF` become U+FFFD in Go while Python raises; string-literal edge case only. | [documented] |
| F15 | Low | Sender goroutine of an outbound session can block on gRPC flow control if a hostile peer stops reading; `PeerStream.Close` then waits for the sender. One goroutine leak per outstanding send attempt (bounded by block-request/session limits). | [documented] residual |
| F16 | Info | gosec `G115` conversion warnings (`uint64->int64`, `int->int32`, `int64->rune/byte`): inputs are local counters, range-clamped sync limits, or hash bytes where negative modulo is corrected. No input path reaches them with attacker-controlled out-of-range values. | [documented] |

---

## 3. Fallback-path checklist (verified in code)

| Area | Behavior verified | Files |
|------|-------------------|-------|
| Proposer rounds | `currentRoundLocked` advances one round per `proposer_timeout` capped at `max(len(active), max_proposer_rounds)`; `verifyBlock` rejects `round < 0`, `round >= cap`, and `round > 0` without the timestamp justification. | `node_consensus.go`, `node_state.go` |
| No-validator / bootstrap | `isExpectedProposerLocked` returns true when no active validators; `verifyBlock` skips the proposer check for an empty set; blocks are accepted locally without controllers. | `node_consensus.go`, `node_state.go`, `node_sync.go` |
| State root | `RequireStateRoot` (default true) enforced live, on sync payloads, on persisted windows and on replayed blocks; `.StateRoot != nil` is re-computed and compared. | `node_state.go`, `node_sync.go` |
| MTP / min interval | Median-time-past(11), future drift (+120 s), live past drift (-120 s) and `min_block_interval_ms` checked; non-numeric timestamps now rejected (F6). | `node_state.go`, `blockchain.go` |
| Reorg depth | Blocks at/below `tip - MaxReorgDepth` are not buffered; a reorg may not rewrite finalized history (`candidate[finalizedHeight]` must match). Replayed state is restored on failure. | `node_fork.go` |
| Orphans | `MaxOrphans` bound with oldest-eviction; `bestOrphanChain` has loop protection; connection only to the current tip. | `node_fork.go` |
| Controller quorum | 0 controllers -> local accept; otherwise >= 66 % of the selected set, each vote verified for chain id, block hash, advertised key and ML-DSA signature. | `node_sync.go` |
| Finality | Stake frozen at commit (`finalitySets`), 2/3 threshold, equivocation evidence bounded (256), votes bounded by lookahead and per-hash voter caps. | `node_consensus.go` |
| Sync | Page cap 512 server-side, `SyncMaxBlocks` total, schema/chain/genesis-allocation checks, every block fully verified and committed one by one; state snapshots from peers are never adopted. | `node_sync.go`, `node.go` |
| Auto-discovery | Registry TTL freshness, lock file with stale-lock recovery, atomic rename, self-exclusion (`isSelf`/`pruneSelfPeers`), bounded port-range probing, throttled background sync (single in-flight). | `discovery.go`, `node_peers.go` |
| Peer limits | `MaxPeers` enforced, PEERS replies truncated to `2*MaxPeers` and verified record-by-record; message rate window per session; session/sync semaphores (256/8); queues 256. | `node_peers.go`, `transport.go` |
| Crypto | ML-DSA-44 (CIRCL) verify-before-trust for peer records, HELLO, votes, blocks and transactions; every signed payload goes through `internal/canonical` (no `encoding/json` on signed bytes); `KeysMatch` on load. | `crypto.go`, `identity.go`, `node_peers.go`, `node_consensus.go`, `ledger/*` |
| Replay protection | Chain id in blocks/txs/votes/HELLO, per-account nonces, tx id = hash of the signed payload; persisted chain verifies the genesis allocation fingerprint. | `transaction.go`, `node.go`, `node_state.go` |
| Process/CLI | `sanup` spawns the staged child with no arguments (no argument injection), detached; `--stop` checks the process image (F5); `--wallet` must match the key-file address; data/key paths come from the operator CLI, not remote input. | `cmd/sanup/*` |

---

## 4. Residual risks (public-devnet pass update)

The first-devnet residuals were re-reviewed before the public devnet on
VPSs; each entry below states the new status.

1. **TLS is opt-in, but one command now enables it.** `sanup cert`
   (`cmd/sanup/certs.go`, `internal/tlsutil`) generates a devnet CA and a node
   certificate (ECDSA P-256, SANs = advertise host + localhost, key 0600);
   `sanup --tls-cert/--tls-key/--tls-ca` sets the `SAN_*` variables. A Go test
   starts two nodes with generated certs and completes the HELLO handshake
   over TLS (`internal/netnode/tls_test.go`). Residual: peer certificates are
   verified but not pinned to node identities (no mTLS), and certificate
   rotation is manual. Without `SAN_TLS_*` traffic is still plaintext and the
   node logs a warning.
2. **REST API authentication is now available.** `SAN_API_TOKEN` (or
   `sanup --api-token`) requires `Authorization: Bearer <token>` on every
   route, compared with `crypto/subtle.ConstantTimeCompare`; failures use the
   FastAPI shape `401 {"detail": "Unauthorized"}` (`internal/api/auth.go`,
   `internal/api/auth_test.go`). The SDK sends the token
   (`SanClient.SetToken`), and sanup threads it through health/status/genesis
   calls. Residual: unset by default (devnet mode) and the rate limiter is
   still the only abuse control in that case; bind the API to a private
   interface or a reverse proxy for public nodes.
3. **LMDB still needs cgo, with an actionable error.** Requesting
   `SAN_DB_BACKEND=lmdb` in a build without the tag now fails with a message
   naming the path, `-tags lmdb` and the memory fallback
   (`internal/ledger/store/lmdb_stub.go`, test `lmdb_stub_test.go`).
   Production VPS builds compile with `CGO_ENABLED=1 go build -tags lmdb`;
   `deploy/install.sh` does that automatically when gcc is available and the
   `deploy/Dockerfile` image always uses the LMDB backend (distroless,
   non-root, `/var/lib/san` volume). The LMDB map grows automatically and is
   not disk-quota limited.
4. **Auto-discovery is no longer same-machine only.** DNS seeds
   (`SAN_DNS_SEEDS`), explicit bootstrap lists (`SAN_BOOTSTRAP`, comma
   separated), a persisted address manager (`SAN_PEERS_CACHE`), outbound
   slots (`SAN_OUTBOUND_PEERS`, default 8), reconnect backoff and gossip-fed
   addresses implement the wide-area path; the file registry and loopback
   probe are documented fallbacks only. Evidence: `addrman_test.go`,
   `dnsseed_test.go`, `outbound_test.go`, `gossip_addrman_test.go` and the
   two-node bootstrap/cache/restart test `wide_area_test.go`. Residual: no
   subnet bucketing or feeler connections (see `docs/gossip.md` limitations),
   and DNS seeds remain a centralization/censorship point mitigated by
   bootstrap lists and the cache.
5. **Outbound-flow-control goroutine leak (F15) is fixed.**
   `PeerStream.Close` waits at most 500 ms for the sender, then closes the
   gRPC connection to unblock a sender stuck in flow control; `CloseSend` is
   only called once the sender stopped (calling it concurrently with `Send`
   races inside gRPC, found by `-race`).
   `internal/netnode/transport_leak_test.go` reproduces the hostile
   non-reading peer and asserts Close returns and goroutines unwind.
6. **Python surrogate/`\U` edge cases stay documented non-issues.**
   Implementing Python's lone-surrogate round trip exactly is not possible in
   Go's UTF-8 strings (Decode normalizes to U+FFFD), and Python's `json`
   rejects `\U` escapes just like Go. `internal/canonical/surrogate_test.go`
   shows every string the protocol signs or hashes (hex keys/hashes,
   addresses, chain ids, type names) is ASCII and round-trips byte-identically,
   so the divergence is unreachable in protocol traffic. Mixed Go/Python
   networks must not place lone surrogates in signed fields.
7. **gRPC stubs unchanged.** The generated stubs were not audited beyond the
   default message-size caps (`SAN_WS_MAX_SIZE`, 1 MiB) and the
   session/sync semaphores; this remains a residual for a first public devnet.

### 4.1 Verification of the public-devnet pass

| Check | Command | Result |
|-------|---------|--------|
| Formatting | `gofmt -l .` | clean |
| Build | `go build ./...` | pass |
| Vet | `go vet ./...` | pass |
| Unit tests | `go test ./... -count=1` | pass |
| Race detector (WSL + gcc) | `go test -race -count=1 ./internal/netnode/... ./internal/api/... ./internal/sdk/... ./internal/ledger/...` | pass, no races |
| End-to-end | `go run ./cmd/sane2e` | `5 passed, 0 failed` |
| Python reference | `python -m pytest tests/test_units.py tests/test_asm.py -q` | `82 passed` |
| TLS | `internal/netnode/tls_test.go` (generated CA/certs, HELLO + PING/PONG over TLS) | pass |
| REST auth | `internal/api/auth_test.go` | pass |
| F15 leak | `internal/netnode/transport_leak_test.go` | pass |
| Wide-area two-node | `internal/netnode/wide_area_test.go` (registry disabled, bootstrap-only join, transfer, cache restart) | pass |

---

## 5. Files added/changed in this pass

**Added:** `internal/netnode/recover.go`,
`internal/canonical/canonical_test.go`,
`internal/sanvm/hardening_test.go`,
`internal/ledger/hardening_test.go`,
`internal/netnode/hardening_test.go`,
`internal/netnode/recover_test.go`,
`internal/api/hardening_test.go`,
`cmd/sanup/proc_test.go`, `docs/security-review.md`.

**Edited (Go only):** `internal/netnode/node_peers.go`,
`internal/netnode/node.go`, `internal/netnode/node_consensus.go`,
`internal/netnode/node_state.go`, `internal/netnode/transport.go`,
`internal/netnode/config.go`, `internal/netnode/discovery.go`,
`internal/netnode/discovery_test.go`, `internal/api/limits.go`,
`internal/api/json.go`, `internal/api/run.go`, `internal/ledger/block.go`,
`internal/ledger/identity.go`, `internal/crypto/crypto.go`,
`internal/sanvm/asm.go`, `cmd/sanup/status.go`, `cmd/sanup/state.go`,
`cmd/sanup/proc_windows.go`, `cmd/sanup/proc_other.go`.

---

## 6. Files added/changed in the public-devnet pass

**Added:** `internal/netnode/addrman.go`, `internal/netnode/addrman_test.go`,
`internal/netnode/dnsseed.go`, `internal/netnode/dnsseed_test.go`,
`internal/netnode/outbound.go`, `internal/netnode/outbound_test.go`,
`internal/netnode/gossip_addrman_test.go`,
`internal/netnode/wide_area_test.go`, `internal/netnode/tls_test.go`,
`internal/netnode/transport_leak_test.go`, `internal/tlsutil/tlsutil.go`,
`internal/api/auth.go`, `internal/api/auth_test.go`,
`internal/canonical/surrogate_test.go`,
`internal/ledger/store/lmdb_stub_test.go`, `cmd/sanup/certs.go`,
`cmd/sanup/public_devnet_test.go`.

**Edited (Go only):** `internal/netnode/config.go`,
`internal/netnode/discovery.go`, `internal/netnode/node.go`,
`internal/netnode/node_peers.go`, `internal/netnode/transport.go`,
`internal/netnode/node_test.go`, `internal/netnode/discovery_test.go`,
`internal/netnode/recover_test.go`, `internal/api/server.go`,
`internal/api/run.go`, `internal/sdk/client.go`, `internal/ledger/store/lmdb_stub.go`,
`cmd/sanup/main.go`, `cmd/sanup/genesis.go`, `cmd/sanup/state.go`,
`cmd/sanup/status.go`, `cmd/sanup/stake.go`, `README.md`, `INSTALL.md`,
`docs/gossip.md`, `docs/security-review.md`.

**Not touched:** all Python implementation and reference files.

---

## 7. Linux/VPS deployment pass

- **Graceful stop.** `sanup --stop` verifies the recorded pid is really the
  staged `sanup-node` binary (`/proc/<pid>/exe` with a `/proc/<pid>/cmdline`
  fallback), sends `SIGTERM`, waits up to 15 s for the node's `Stop()` path
  (peer cache saved, registry entry removed) and escalates to `SIGKILL` only
  as a last resort. Windows keeps `taskkill /F`. Key/cache/state files are
  written 0600 (with an explicit `Chmod`, so an existing wider file is
  tightened) and data directories 0700.
- **Self-peer fix.** A node that advertises a public DNS name
  (`SAN_ADVERTISE_HOST`) no longer accepts its own signed record as a peer
  (`isOwnRecord`), so it cannot dial itself or consume an outbound slot.
- **Foreground/systemd mode.** `sanup --foreground` runs the node in its own
  process for `Type=simple` units; every option is readable from `SAN_*`
  (`SAN_SEED`, `SAN_STAKE`, `SAN_TLS_*`, ...), and the state file records TLS
  so `--status`/`--stop` work without repeating the `--tls-*` flags.
- **TLS REST fallbacks.** The SDK client can trust a devnet CA (or skip
  verification for the node's own self-signed pair); `sanup` health/status and
  the node's same-machine discovery probe use HTTPS when TLS is enabled.
- **Lost-gossip recovery.** A node that receives a future block (a gap) or an
  unverifiable tip-adjacent block twice now runs `Synchronize`, and the health
  loop requests a background sync every `SAN_PEER_CHECK_INTERVAL` (30 s) when
  peers exist. Previously a single lost `BLOCK` message left the node behind
  until new transactions or peers appeared (the source of the WSL e2e flake;
  `go run ./cmd/sane2e` then passed three consecutive runs).
- **Deployment artifacts.** `deploy/install.sh`, `deploy/san.env.example`,
  `deploy/san-node.service`, `deploy/Dockerfile`, `Makefile`; the
  `Dockerfile.go` workaround was deleted and CI/compose use
  `deploy/Dockerfile`.
- **Evidence (WSL Ubuntu, Go 1.26.0 linux/amd64, gcc 15).** `go build ./...`,
  `go vet ./...`, `go test ./... -count=1`, `CGO_ENABLED=1 go test -tags lmdb
  ./internal/ledger/store/...` and `CGO_ENABLED=1 go build -tags lmdb ./...`
  pass; `go run ./cmd/sane2e` reports `5 passed, 0 failed`; the wide-area
  two-node test passes with the registry disabled; `deploy/install.sh`
  installs and starts the systemd unit, `/health` answers over HTTPS with the
  API token, and `sanup --stop` stops it gracefully. Cross-compiles clean for
  linux/amd64 and linux/arm64 from Windows.
