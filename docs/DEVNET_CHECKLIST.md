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
- [ ] Partition scenarios A-D automated (5/5, 7/3, proposer isolation, full split)
- [ ] Long-running soak test (`cmd/sansoak`) passes for the agreed duration
- [ ] Fork-choice torture suite (randomized branches, delayed parents)
- [ ] Crash-consistency tests (kill during commit/finality/prune) pass
- [ ] Validator churn suite (join, undelegate, withdraw, slash) passes
- [ ] Finality stress suite (out-of-order, duplicate, conflicting votes) passes
- [ ] Mixed Go/Python implementation policy documented and enforced in code

## Persistence and recovery

- [x] LMDB atomic block/state commit verified
- [x] Snapshot save/load and mismatch pruning covered by tests
- [x] Restart-and-catch-up path exercised (`wide_area_test`, `sane2e`)
- [ ] Backup/restore runbook executed on a VPS-style install
- [ ] Database schema-version refusal path tested against a newer fixture
- [ ] Explicit migration policy documented and linked from `INSTALL.md`

## Configuration and secrets

- [x] Key files written with 0600 and data dirs with 0700 on Linux
- [x] Private keys and API tokens never logged
- [ ] Public-devnet config profile (`deploy/san.env.example`) reviewed and frozen
- [ ] Production nodes refuse development defaults (no in-memory DB, no open faucet)
- [ ] Controller count and minimum stake validated at startup with clear warnings
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
- **Eclipse and Sybil risk.** Addrman bucketing is simplified, DNS seeds are a
  centralization point, and peer reputation is local only. Seed lists must be
  diversified and monitored.
- **Weak subjectivity / long-range protection is not implemented.** A new node
  trusts the genesis fingerprint and the peer set it learns from.
- **Limited production history.** No long-running public deployment yet; crash
  and partition behavior is covered by tests, not by years of operation.
- **SANVM and PENA are experimental.** Gas metering, limits and fuzzing exist,
  but the VM and language have not been externally audited.
- **TLS trust equals genesis trust.** Without TLS (or with a shared devnet CA),
  a network-level attacker could serve a different genesis to a joining node.
- **Mixed-implementation policy.** Python remains the reference and fixture
  source; the Go node is the canonical protocol implementation. Live Go/Python
  interoperability is not continuously tested.
- **External security audit has not been performed.** The published review is an
  internal, automated/adversarial code review (`docs/security-review.md`).

Not mainnet ready. Public devnet only.
