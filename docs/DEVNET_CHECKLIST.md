# Public Devnet Launch Checklist

Objective launch gate for a public SAN devnet. A checked box means the item has
been verified on the current revision; unchecked items are outstanding work and
must be resolved before announcing the devnet.

## Build, tests and CI

- [x] Go unit tests pass (`go test ./... -count=1`)
- [x] Linux gofmt/vet clean (`gofmt -l .` empty, `go vet ./...`)
- [x] Python reference tests pass (`pytest tests/test_units.py tests/test_asm.py`)
- [x] Python lint/type checks pass (`ruff check .`, `mypy`)
- [x] Go race detector clean on concurrent packages (WSL, gcc/cgo)
- [x] LMDB backend builds and store tests pass (`-tags lmdb`)
- [x] Linux cross-compile (amd64, arm64)
- [x] Go end-to-end check (`go run ./cmd/sane2e`, 6 steps)
- [ ] GitHub Actions green on the target commit (lint fixes pushed; confirm run)
- [ ] Fuzz targets run clean in CI (short smoke job)
- [ ] Nightly soak job scheduled

## Network and consensus

- [ ] 10-node devnet startup verified end to end
- [ ] 10-node test with independent data directories and ports
- [x] Partition scenarios A-D automated (5/5, 7/3, proposer isolation, full split)
- [ ] Long-running soak test (`cmd/sansoak`) passes for the agreed duration
- [x] Fork-choice torture suite (randomized branches, delayed parents)
- [x] Crash-consistency tests (kill during commit/finality/prune) pass
- [x] Validator churn suite (join, undelegate, withdraw, slash) passes
- [x] Finality stress suite (out-of-order, duplicate, conflicting votes) passes
- [x] Mixed Go/Python implementation policy documented and enforced in code

## Persistence and recovery

- [x] LMDB atomic block/state commit verified
- [x] Snapshot save/load and mismatch pruning covered by tests
- [x] Restart-and-catch-up path exercised (`wide_area_test`, `sane2e`)
- [ ] Backup/restore runbook executed on a VPS-style install
- [x] Database schema-version refusal path tested against a newer fixture
- [x] Explicit migration policy documented and linked from `INSTALL.md`

## Configuration and secrets

- [x] Key files written with 0600 and data dirs with 0700 on Linux
- [x] Private keys and API tokens never logged
- [ ] Public-devnet config profile (`deploy/san.env.example`) reviewed and frozen
- [ ] Production nodes refuse development defaults (no in-memory DB, no open faucet)
- [x] Controller count and minimum stake validated at startup with clear warnings
- [ ] API token required (or reverse proxy enforced) for public REST exposure

## Observability and operations

- [ ] Metrics expanded to the full chain/tx/network/consensus set
- [ ] `/health` and `/ready` expose lifecycle states (starting/syncing/ready/degraded)
- [ ] Version information exposed (`san-node version` + REST)
- [ ] JSON structured logging with configurable levels
- [ ] Dashboard and alerts configured for a public deployment
- [ ] Log review confirms no secret leakage

## Genesis and protocol

- [x] Genesis fingerprint checked across peers before sync
- [ ] Genesis file frozen and published for operators
- [ ] Handshake carries chain id, genesis hash, protocol and software version
- [ ] Incompatible protocol versions are rejected with a useful reason
- [x] Version endpoint exposes protocol and schema versions
- [ ] Upgrade strategy documented (rolling vs coordinated)

## Deployment

- [x] `deploy/install.sh` + systemd unit verified on Linux (WSL)
- [x] systemd hardening (NoNewPrivileges, ProtectSystem, ReadWritePaths, limits)
- [x] Docker image (LMDB, non-root, healthcheck, SIGTERM)
- [ ] Docker image built and smoke-tested in CI
- [x] Firewall/NAT/port documentation, TLS certificate workflow, DNS seeds
- [x] Faucet with token, cooldowns and caps
- [ ] 3 seed + 7 validator public topology deployed and monitored

## Release readiness

- [ ] Version tag created
- [ ] Release binaries reproducible/checksummed
- [ ] Known limitations documented (below) and reviewed
- [ ] Internal security review re-run against the frozen revision
- [ ] External independent security audit (not yet commissioned)

---

# Public Devnet Known Limitations

These are accepted limitations for the public devnet. They must stay documented
until the corresponding work lands.

- **No formal verification of Chaos Limited Consensus.** Safety and liveness
  arguments are documented in `docs/chaos-limited-consensus.md` but not
  machine-checked.
- **No view-change certificate.** Proposer fallback advances rounds locally; the
  round change is justified by timestamp rules, not by a signed quorum.
- **No timeout slashing.** Only provable equivocation is slashable; offline or
  slow validators are not penalized on-chain.
- **Controller quorum is not finality.** Controller approvals are an
  availability/pre-commit gate; finality comes from stake-weighted 2/3 votes.
- **Eclipse and Sybil risk.** Addrman bucketing is simplified (per-subnet caps
  on inbound sessions, the peer table and outbound slots; no feeler
  connections), DNS seeds are a centralization point, and peer reputation
  (scoring, temporary bans, persisted score in the peer cache) is local only.
  Seed lists must be diversified and monitored. Regression tests:
  `TestInboundCapsPerIPAndSubnet`, `TestSubnetPeerTableCap`,
  `TestSelectOutboundPlanKeepsSubnetDiversity`, `TestPeerBanExponentialCooldown`,
  `TestAddrmanPersistsPeerScore`.
- **Weak subjectivity / long-range protection is not implemented.** A new node
  trusts the genesis fingerprint and the peer set it learns from.
- **Limited production history.** No long-running public deployment yet; crash
  and partition behavior is covered by tests, not by years of operation.
- **SANVM and PENA are experimental.** Gas metering, limits and fuzzing exist,
  but the VM and language have not been externally audited. Batch C added
  Go-only collection caps (`MaxCollectionItems`), superlinear charging above
  1,024 items for `LIST_REMOVE`/`DICT_KEYS`, and Python `str/list * int`
  repetition. The one documented order deviation is `DICT_KEYS` (Go sorted vs
  Python insertion order; whitelisted in the differential comparator). Fuzz
  targets `FuzzParser`, `FuzzAssembler`, `FuzzVM`, `FuzzVMBytecode`,
  `FuzzContract` plus the retained seed
  `internal/sanvm/testdata/fuzz/FuzzVM/36c112d8fc0ef853` cover the parser,
  assembler, bytecode and contract paths; a CI fuzz smoke job is still
  outstanding. Regression tests: `TestMixedTypeComparisonsNeverPanic`,
  `TestCollectionItemCapRejectsCleanly`,
  `TestListRemoveScanIsPricedForLargeLists`,
  `TestDictKeysOrderDeviationIsWhitelisted`.
- **Go/Python VM differential coverage is bytecode-level only.** The section 15
  fuzzer (`internal/parity/differential_fuzz_test.go`) generates bytecode
  programs and compares stack/logs/storage/gas/error type/timing against the
  Python VM; it does not run whole contracts through both nodes. Intentional
  deviations are whitelisted and documented in
  `docs/chaos-limited-consensus.md` section 10. Regression tests:
  `TestDifferentialFuzzGoPythonVM`, `TestDictKeysOrderDeviationIsWhitelisted`,
  `TestParityRegressionFixtures`.
- **Crash consistency has two coverage levels.** In-process fault injection
  (`internal/ledger/crash_consistency_test.go`, memory backend) runs on every
  platform; the real SIGKILL/process-kill harness
  (`TestLMDBProcessKillConsistency`) requires `-tags lmdb` and runs in WSL/Linux
  (`CGO_ENABLED=1`). The LMDB path was exercised, but a longer soak on real
  hardware is still advisable.
- **Backup tooling is offline-only.** `sanbackup`
  (`internal/backup`, `cmd/sanbackup`) requires the node to be stopped
  (SIGTERM + wait) before `backup`; there is no live-snapshot path. Restore
  validates the canonical chain, state root and finality checkpoint. Tests:
  `TestBackupRestoreRoundTrip`, `TestRestoreDetectsTampering`,
  `TestValidateStoreDetectsStateRootMismatch`.
- **New resource-limit counters are Go-only metrics** appended after the
  Python-parity prefix (`mempool_rejected`, `orphans_evicted`,
  `block_requests_rejected`, `staged_votes_rejected`, `peers_rejected_table`,
  `vote_seen_cache_resets`, `http_body_rejected`); a Python node's `/metrics`
  does not expose them. The full section 13 audit (per-structure bound,
  configurable knob, counter and residual uncapped paths: total contract
  storage, logs per tx, txs per block) is in
  [docs/resource-limits.md](resource-limits.md). Regression tests: `TestMempoolLimitRejectsAndCounts`,
  `TestOrphanLimitEvictsAndCounts`, `TestBlockRequestLimitRejectsAndCounts`,
  `TestStagedVoteHashCapRejectsAndCounts`, `TestPeerTableLimitRejectsAndCounts`,
  `TestSeenVoteCacheStaysBounded`.
- **TLS trust equals genesis trust.** Without TLS (or with a shared devnet CA),
  a network-level attacker could serve a different genesis to a joining node.
- **Mixed-implementation policy.** Python remains the reference and fixture
  source; the Go node is the canonical protocol implementation
  (`docs/interop.md`). The opt-in harness
  (`go test -tags interop ./internal/netnode -run TestInterop`, skipped
  cleanly without Python) verifies Go-vs-Go and live Go/Python genesis
  agreement, handshake, transfer/block propagation, stake deposit, sync
  catch-up, state roots and contract deploy; SANRC20 transfers, on-chain
  undelegate/withdraw, governance and finality-vote gossip across
  implementations are not automated yet. Go-only hardenings that differ from
  the Python reference without changing wire rules or block validity: a
  persisted finality checkpoint that contradicts the canonical chain is a
  fatal startup error instead of a logged ignore, controller records are
  deduplicated by signing key and stale ones dropped from selection, and a
  replaced branch's verified blocks are retained as orphans. Regression tests:
  `TestPersistedFinalityContradictionFailsStartup`, `TestDedupeControllers`,
  `TestStaleControllerRecordIsDropped`,
  `TestReorgRetainsAbandonedBranchAncestors`. Known asymmetry Interop-1:
  Python's `Bootstrap` RPC omits its own signed record (harness uses the REST
  fallback; documented in `docs/interop.md`).
- **External security audit has not been performed.** The published review is an
  internal, automated/adversarial code review (`docs/security-review.md`).

Not mainnet ready. Public devnet only.
