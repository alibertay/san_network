# Rolling upgrade procedure

This document defines how a running SAN devnet moves from version X to X+1
one node at a time. It covers: protocol compatibility gates, the operator
steps for a 10-node network, verification at each step, and the exact
limitations of the current protocol.

## Compatibility gates

| Gate | Where | Behavior |
| --- | --- | --- |
| P2P protocol version | `netnode.ProtocolVersion` checked in `Node.VerifyHello` | A peer whose `protocol` field differs is rejected; the existing session is closed. Regression test: `TestByzantineHandshakeRejections`. |
| Chain id | `VerifyHello` + sync payload | A peer on another chain is rejected. |
| Genesis allocation fingerprint | `Node.Synchronize` (`genesis_allocation`) | Peers with a different genesis allocation cannot sync; the node refuses. |
| DB schema version | `ledger.SchemaVersion` + store metadata | A newer schema refuses to load (tested against a newer fixture). |
| Software version in handshake | **not implemented** | `HelloPayload` does not carry the semantic software version or the genesis hash; compatibility is currently enforced by protocol version + chain id + genesis fingerprint. Adding `software_version` / `genesis_hash` to HELLO is a Batch E requirement and needs a protocol version bump. Regression stub: `TestReadyStateLifecycle` (version metadata exposed via REST) plus this document's checklist item. |
| REST API | additive only | New endpoints/fields are added; existing shapes are preserved. `/ready` is new, `/health` only gained fields. |

Because the handshake rejects a different `protocol` value, **do not bump
`ProtocolVersion` in an upgrade that is meant to roll**. Bump it only for a
breaking wire change, and then upgrade as a coordinated stop-the-world
restart.

## 10-node rolling upgrade (X → X+1, compatible)

Preconditions:

- X+1 keeps `ProtocolVersion` unchanged and only adds fields/endpoints.
- New nodes accept the older state format (schema version unchanged or
  backward compatible).
- Each node's data directory is backed up (`sanbackup`) and the release
  binary is checksummed.

Steps (one node at a time, order: non-validators first, then validators by
ascending stake):

1. Build/install the new binary on node 1 (`deploy/install.sh` is
   idempotent and restarts the service it manages). For manual installs,
   stage the new `sannode`/`sanup`/`sancli` files.
2. Drain nothing: the node keeps its identity and data directory. Restart
   with `systemctl restart san-node` (SIGTERM, graceful, 30 s stop timeout).
3. Watch `GET /ready` until it reports `ready` again and `GET /health`
   height/finalized height catch up to the network within the convergence
   window (height spread <= 3).
4. Verify it still peers (`san_peers` gauge), still votes
   (`san_votes_received` increases), and that the validator set reported by
   `/validators` matches the other nodes.
5. Repeat for the next node. Wait for step 3 on each node before touching the
   next one; never restart two validators at once for a devnet that must keep
   finality (a 2/3 vote quorum needs the stake online).
6. After the last node, confirm:
   - all 10 nodes report identical `state_root` at the same height;
   - finalized height spread within `docs/DEVNET_CHECKLIST.md` tolerance;
   - `san_validation_failures`, `san_handshakes_failed` and
     `san_sync_failures` are not climbing;
   - `sanup version` and `/health.version_info` agree on the new revision.

## Rollback

If step 3 does not converge within ~2 minutes, stop the upgraded node and
restore the previous binary (the data directory is unchanged and the old
binary is schema-compatible). If a schema-migrating upgrade was involved,
roll back by restoring the `sanbackup` snapshot instead; see
`docs/backup-restore.md`.

## Coordinated (breaking) upgrade

If `ProtocolVersion` changes, the steps above will partition the network (new
nodes reject old handshakes). Announce a maintenance window, stop all nodes,
install X+1 everywhere, then start the founder first so the network rebuilds
its peer graph. Finality resumes when 2/3 of the stake is online; verify with
`/finality` and `/ready`.

## Verification hooks

- `deploy/deploy_test.go` lints the systemd unit (graceful SIGTERM,
  hardening, no unsupported `WatchdogSec`) and the installer's version
  injection.
- `internal/netnode` handshake tests pin protocol/chain/freshness rejection.
- `cmd/sansoak --short` exercises restarts (churn) and asserts convergence,
  finality, state-root equality and money conservation afterwards.
- `/ready` distinguishes `starting` / `syncing` / `ready` / `degraded`, so an
  operator (or an orchestrator) can gate traffic on a single endpoint.
