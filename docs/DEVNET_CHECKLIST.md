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
- [x] Nightly soak job scheduled (`.github/workflows/soak.yml`)

## Network and consensus

- [ ] 10-node devnet startup verified end to end
- [ ] 10-node test with independent data directories and ports
- [x] Partition scenarios A-D automated (5/5, 7/3, proposer isolation, full split)
- [ ] Long-running soak test (`cmd/sansoak`) passes for the agreed duration
  (`cmd/sansoak --short` passes locally/CI and the nightly workflow runs a
  10-minute 5-node soak; the 24h soak still needs a VPS run)
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
  (`TestSecretFileAndDirectoryPermissions`)
- [x] Private keys, API tokens and TLS keys never logged (redaction covers PEM,
  bearer/query tokens, environment-style assignments and JSON fields:
  `TestRedactionCoversCredentials`, `TestRedactionCoversEnvironmentAndTLSSecrets`,
  `TestJSONHandlerRedactsFields`); the process environment is never dumped
- [x] Public-devnet config profile (`deploy/san.env.example`) reviewed and
  frozen; public mode refuses memory storage, tokenless API/faucet, zero
  controllers, missing genesis fingerprint and legacy handshakes
  (`TestPublicDevnetProfileValidationMatrix`, `TestValidatePublicProfileMatrix`)
- [x] Production nodes refuse development defaults (no in-memory DB, no open
  faucet, no unauthenticated non-loopback API)
- [x] Controller count and minimum stake validated at startup with clear warnings
- [x] API token required (or loopback bind enforced) for public REST exposure
- [x] Secret-handling runbook for VPS operators (`docs/secrets.md`)

## Observability and operations

- [x] Metrics expanded to the full chain/tx/network/consensus set
- [x] Benchmark methodology documented (`cmd/sanbench`, `docs/benchmarks.md`)
- [x] Post-quantum bandwidth analysis documented (`docs/pq-bandwidth.md`)
- [x] `/health` and `/ready` expose lifecycle states (starting/syncing/ready/degraded)
- [x] Version information exposed (`sanup version` + REST `/health.version_info`)
- [x] JSON structured logging with configurable levels (`SAN_LOG_FORMAT`, `SAN_LOG_LEVEL`)
- [ ] Dashboard and alerts configured for a public deployment
- [ ] Log review confirms no secret leakage (redaction is unit-tested:
  `TestRedactionCoversCredentials`, `TestJSONHandlerRedactsFields`; a real
  public log review is still due)

## Genesis and protocol

- [x] Full genesis fingerprint checked before startup, in HELLO, in peer
  records and before sync (legacy `genesis_allocation` remains the protocol-2
  fallback)
- [x] Genesis file frozen and published for operators (`deploy/genesis.json`,
  `--genesis-file`/`SAN_GENESIS_FILE`, installed to `/etc/san/genesis.json`;
  round-trip, mismatch and invalid-file tests in `internal/genesis`,
  `TestDeployGenesisFileLoads`)
- [x] Handshake carries chain id, genesis fingerprint, protocol and software
  version, and capabilities (`docs/protocol.md`; protocol bumped to 3 with an
  explicit `SAN_ALLOW_LEGACY_HANDSHAKE=1` window for the frozen Python
  reference; tests `TestHelloAcceptRejectMatrix`, `TestLegacyHandshakeWindow`)
- [x] Incompatible protocol/genesis/chain/capability peers are rejected with a
  useful reason and per-reason metrics (`handshake_rejected_*`)
- [x] Version endpoint exposes protocol and schema versions
- [x] Upgrade strategy documented (rolling vs coordinated, `docs/rolling-upgrade.md`)
- [x] Genesis hash and fingerprint printed prominently at node startup and in
  `sanup --status` (`TestStartupLogsGenesis`)

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
  `TestSeenVoteCacheStaysBounded`. Batch D appends the chain/tx/network/
  consensus/runtime set after those (`transactions_accepted`,
  `validation_failures`, `bytes_sent`/`bytes_received`, `peers_inbound`/
  `peers_outbound`, `controller_approvals`/`controller_failures`,
  `go_goroutines`, `go_memory_*`, `go_gc_cycles`, `uptime_seconds`,
  `api_rate_limited`); the Python-parity prefix is unchanged and pinned by
  `TestRenderMetricsZeroSnapshotKeepsPythonParityPrefix` and
  `TestRenderMetricsExposesGoOnlyMetrics`.
- **TLS trust equals genesis trust.** Without TLS (or with a shared devnet CA),
  a network-level attacker could serve a different genesis to a joining node.
- **Mixed-implementation policy.** Python remains the reference and fixture
  source; the Go node is the canonical protocol implementation
  (`docs/interop.md`). Go protocol 3 carries the full genesis fingerprint,
  software version and capabilities; the frozen Python reference still speaks
  protocol 2, so live Go/Python peering runs the Go side with
  `SAN_ALLOW_LEGACY_HANDSHAKE=1` (a trusted-network compatibility window, not
  for public nodes). The opt-in harness
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
- **Soak coverage is CI-short, not 24h.** `cmd/sansoak` spawns or targets
  nodes, generates transfers/SANRC20/contract writes/staking/governance/churn
  activity and asserts money conservation, height/finality convergence,
  state-root and validator-set equality, stuck nodes, mempool bounds and
  resource growth. The 70-second `--short` mode passes locally and in CI; the
  nightly workflow runs a 10-minute 5-node soak; the 24-hour soak still needs a
  VPS run. Report logic is unit-tested without network time (`TestEvaluate*`,
  `TestCheckGrowth`, `TestReportRenderAndPass`).
- **Benchmarks are single-host reference numbers.** `cmd/sanbench` measures
  ML-DSA-44, transaction validation/serialization, SANVM workloads, block
  validation and a loopback network lab. The published table was produced on
  one Windows host (methodology and hardware in `docs/benchmarks.md`); it is
  not a TPS promise and excludes consensus/disk I/O. `--full` adds the
  5000-transaction block and the network lab.
- **Post-quantum bandwidth is modelled, not load-tested.** `docs/pq-bandwidth.md`
  computes per-node traffic for 10/50/100 nodes at 1/10/100 tx/s from measured
  sizes. The model assumes flood transaction/block gossip and full-block
  controller vote requests; the batching suggestions there are not implemented
  because the wire format and consensus determinism must not change ad hoc.
  This is the expected scaling ceiling for a 100-node devnet above ~10 tx/s.
- **Logging is structured, but only key sites carry fields.** `SAN_LOG_FORMAT`
  and `SAN_LOG_LEVEL` configure text/JSON output; `internal/sanlog` redacts
  private keys, tokens, bearer headers and PEM blocks (tests:
  `TestTextFormatKeepsMessageAndFields`, `TestJSONFormatEmitsStructuredRecord`,
  `TestLevelFiltering`, `TestRedactionCoversCredentials`,
  `TestRedactionCoversEnvironmentAndTLSSecrets`). Legacy `log.Printf` call
  sites still produce plain messages. The handshake now carries the software
  version (`software-version-v1`) and startup logs it next to the genesis
  fingerprint, so version-tagged records are available from Batch E onward.
- **Readiness semantics are devnet-grade.** `/ready` returns 503 until the
  node has listeners, identity, genesis and peers; in `SAN_PUBLIC_DEVNET` mode
  it turns `degraded` when no peers are visible or the controller target is
  below the configured minimum. Tests: `TestReadyStateLifecycle`,
  `TestReadyStateDegradedWithoutPeers`, `TestReadyEndpointReportsStarting`.
  It does not yet detect disk-full, clock skew or a stuck-but-reachable chain.
- **Rolling upgrade is documented but unexercised on a multi-host devnet.**
  The protocol-version gate exists and is tested (`TestHelloAcceptRejectMatrix`,
  `TestLegacyHandshakeWindow`); `docs/rolling-upgrade.md` covers the 10-node
  one-at-a-time procedure and the coordinated protocol-3 window. The legacy
  compatibility mode (`SAN_ALLOW_LEGACY_HANDSHAKE=1`) accepts protocol-2 peers
  without the genesis/software/capability checks and is refused by the
  public-devnet profile; it exists for the Python interop harness and
  one-time migrations on trusted networks only. `deploy/deploy_test.go` lints
  the systemd unit, Dockerfile and installer, but no automated multi-host
  upgrade run exists yet.
- **The published genesis file has no premine.** `deploy/genesis.json` ships
  with an empty allocation map (validators earn block rewards); an operator
  who wants a premine must add the allocations and regenerate the fingerprint
  *before* freezing and distributing the file. Loading rejects a hand-edited
  file whose declared fingerprint no longer matches.
- **Public-profile override is a footgun by design.**
  `SAN_ALLOW_INSECURE_PUBLIC=1` / `sanup --allow-insecure-public` downgrades
  every public-devnet safety failure to a warning for local testing; startup
  logs a `WARNING` for each one, and it must never be set on a public node.

Not mainnet ready. Public devnet only.
