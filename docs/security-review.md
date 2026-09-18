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

## 4. Residual risks for the first devnet

1. **TLS is opt-in.** Without `SAN_TLS_CERT/SAN_TLS_KEY` (and `SAN_TLS_CA` on
   peers) all P2P traffic is plaintext; the node logs a warning at startup.
   Acceptable for a loopback/devnet; enable TLS before any public network.
2. **REST API has no authentication.** `/transaction`, `/join` and the read
   routes are open to anyone who can reach the port; the rate limiter is the
   only abuse control. Bind to loopback (`--host 127.0.0.1`) as `sanup` does
   by default.
3. **LMDB needs cgo.** The default Windows build uses the in-memory/no-op
   store (`SAN_DB_BACKEND=memory` under `sanup`); persistence requires a
   `-tags lmdb` build on a machine with a C toolchain. The LMDB map grows
   automatically and is not disk-quota-limited.
4. **Auto-discovery is same-machine only** (file registry + loopback port
   probing); it is not a substitute for real peer discovery.
5. **Outbound-send backpressure** (F15) can leak a blocked goroutine per
   hostile peer that stops reading; bounded but not fixed in this pass.
6. **Python interop edge cases** (F13/F14) for lone surrogates and invalid
   `\U` literals; irrelevant while all nodes run the Go implementation.
7. **No audit of the generated gRPC stubs** beyond default message-size caps
   (`WSMaxSize`, default 1 MiB) and the session/sync semaphores.

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
