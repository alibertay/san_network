# SAN Network — Independent Security Audit

> **Scope:** `network/`, `blockchain/`, `SANVM/`, `app/` (consensus, network, storage, VM)
> **Method:** Independent model review (OpenAI Codex CLI, `--sandbox read-only`) — three
> finding rounds plus successive verification rounds. Every finding was fixed and covered by a
> regression test, then re-reviewed until the fix was confirmed closed.
> **Result:** All findings are closed; the final verification round reports **no new
> critical/high issues**. Ready for an operator-controlled devnet.
> **Date:** 2026

---

## 1. Summary of rounds

| Round | Focus | Findings | Status |
|-------|-------|----------|--------|
| 1 | Consensus, money, storage, network, VM | 7 (2 critical, 3 high, 2 medium) | closed |
| 2 | Re-audit | 10 (6 high, 4 medium) | closed |
| 3 | Verification | 6 partial + 2 new | closed |
| 4 | Verification | 2 remaining | closed |
| 5 | Final verification | both items CONFIRMED CLOSED, no new issues | closed |
| 6 | P2P migration to gRPC + block rewards | 1 critical, 5 high | closed |
| 7–10 | Verification of rounds 6 fixes | 4 partial + 2 new | closed |
| 11 | Final verification | both items CONFIRMED CLOSED, no new issues | closed |

---

## 2. Round 1 — findings and fixes

| # | Severity | Finding | Fix | Regression test |
|---|----------|---------|-----|-----------------|
| 1 | critical | Block validation accepted an attacker-chosen `round`; a validator could select itself as proposer and produce immediately | Round claims are bound to **timestamp evidence**: for `round > 0`, `timestamp >= parent.timestamp + round × proposer_timeout × 0.8`; bounded by `max_proposer_rounds`; `proposer_timeout_ms` is a chain parameter | `test_round_deadline_is_consensus_state`, `test_round_timing_rejects_unjustified_rounds` |
| 2 | critical | Finality weighted a historical height from the **current** validator registry; stake could be withdrawn and the remainder could finalize alone | Voting weights are frozen at commit time (`_finality_sets`); tally uses only the frozen set | finality-set unit tests |
| 3 | medium | Stale transaction index after a reorg could return the wrong transaction | Index is rebuilt; lookups re-verify block hash and transaction id | `test_stale_tx_index_is_not_served` |
| 4 | medium | Reorgs left snapshots from the discarded branch | `delete_mismatched_snapshots` plus canonical-hash matching | `test_chain_store_snapshots_replace_and_delete` |
| 5 | high | Unsigned `DEAD_PEER` records allowed arbitrary peer eviction (eclipse) | Remote dead-peer claims are ignored; only local health checks evict | `test_dead_peer_claims_are_ignored` |
| 6 | high | `LIST_APPEND` cost was constant; contract storage growth was unmetered | Per-value storage gas plus a 64 KiB value cap | VM gas tests |
| 7 | high | State roots omitted `vmdata`/`vmfuncs` | `state_entries` covers every persisted namespace | state-root tests |

---

## 3. Round 2 — findings and fixes

| # | Severity | Finding | Fix |
|---|----------|---------|-----|
| 1 | high | Sync could bypass the local-clock round gate and commit the same block | Local gate removed; **timestamp evidence** is enforced on every path (live, sync, replay) |
| 2 | high | `_commit_block` did not call `verify_block`; unsigned local blocks could be committed | `_commit_block` performs full validation; production without a signing key is refused |
| 3 | high | A failed reorg did not restore `_finality_sets`/votes | `_capture_state`/`_restore_state` (finality sets, votes and receipts included) |
| 4 | high | A pruned node started sync from a relative index | `from_index = tip.index + 1` (absolute height) |
| 5 | high | Reorg replay persisted block by block; a crash left an inconsistent database | Replay is fully in memory; one atomic `replace_chain` batch |
| 6 | high | With `snapshot_interval > prune_keep`, pruning could delete the newest snapshot header | `cutoff = min(finalized - prune_keep, latest_snapshot)`; no pruning without an anchor; startup warning |
| 7 | high | VM integer DoS: `MUL` cost 2 gas regardless of operand size | `MAX_INT_BITS = 4096`; `ADD/SUB/MUL/DIV/MOD` priced by operand size |
| 8 | high | `state_root` was optional (a signed block could omit it) | `require_state_root` (default on): mandatory on every non-genesis block, including loaded windows |
| 9 | medium | `_finality_sets` and pending votes were lost on restart | `finality_state` meta (sets + votes + checkpoint, last 256 heights), restored on startup |
| 10 | medium | A non-string `contract_id` crashed execution and poisoned the mempool | `Transaction.validate_contract_code`, `ContractManager` type guard, `except Exception` in `_run_tx_execution` (escrow burned) |

---

## 4. Rounds 3–5 — verification

Round 3 confirmed 7 of the 10 round-2 items closed and left:

- **`proposer_timeout` moved into consensus state** (`proposer_timeout_ms`, governable
  100–60,000 ms). A different environment value changes the genesis state root, so nodes
  with different settings can never share a chain silently.
- **Reorgs preserve receipts and transaction indexes**: `replace_chain(receipts_by_index=...)`
  rebuilds them in the same batch; unchanged blocks keep their records by hash.
- **No pruning without a snapshot** (a pruned store without an anchor would not reload).
- **`ADD`/`SUB` are metered** like the other arithmetic opcodes.
- **Loaded windows must carry state roots** (`_require_loaded_state_roots`).
- **Malformed finality blobs are ignored safely**; the finalized checkpoint only ever moves
  forward (monotonic across restarts).

Round 5 result: both remaining items `CONFIRMED CLOSED`, `NEW ISSUES: none`.

---

## 5. Round 6 — gRPC transport and block rewards

Node-to-node traffic moved from WebSocket/REST to gRPC
(`network/proto/p2p.proto`: `Session` bidirectional stream, `Sync`, `Bootstrap`; REST is
the user-facing API only). A chain-level `block_reward` and a signed-header
`reward_address` were added (schema v5). Findings and fixes:

| # | Severity | Finding | Fix |
|---|----------|---------|-----|
| 1 | high | Unbounded session buffering; only a per-stream rate limit | Bounded queues (256) on both sides, session semaphore (256) acquired before buffers/tasks, awaited task cleanup, concurrent-Sync semaphore (8) |
| 2 | high | Unauthenticated `Sync` allowed unbounded responses | Server clamps `limit` to [1, 512]; state/storage only on the first page; `blocks_since` bounded slicing |
| 3 | high | Unbounded future finality votes; early equivocation skipped evidence | Staged votes limited to `tip + 64`, 8 hashes per height, 128 voters per hash, 256 hashes total, 16 KiB per vote; evidence recorded on the staging path; non-members pruned at commit; rebroadcast window `max(finalized-8, tip-64, 0)` |
| 4 | **critical** | No consensus block interval: a subsidy chain could mint a burst (including backdated timestamps) | `min_block_interval_ms` genesis parameter; **at least 1 s enforced whenever `block_reward > 0`**; live blocks older than 120 s are rejected (historical sync/replay exempt via a trusted anchor, never via the block's own timestamp) |
| 5 | high | Joiner genesis was trust-on-first-use and served mutable parameters | `/genesis` serves an **immutable** genesis-parameter snapshot; the launcher refuses conflicting environment variables and verifies both the seed's and the local genesis hash (`--expect-genesis-hash`), exiting hard on mismatch |

Rounds 7–10 closed the partial items: session-task leaks on teardown, `blocks_since`
copies, empty marker entries in the vote store, live-reorg backdating bypass, unbounded
`/headers`, the global hash counter, and oversized vote payloads. Round 11: all items
`CONFIRMED CLOSED`, `NEW ISSUES: none`.

---

## 6. Operator notes

- `SAN_PROPOSER_TIMEOUT` only sets the **genesis value**; on-chain governance changes it.
- `require_state_root` is on by default; synthetic tooling can set
  `SAN_REQUIRE_STATE_ROOT=false`.
- When using `SAN_PRUNE_KEEP`, keep it compatible with `SAN_SNAPSHOT_INTERVAL`; the node
  warns and limits pruning to the newest snapshot height.
- Finality checkpoints and in-flight votes are persisted, so finality resumes after a
  restart; a lower checkpoint can never reopen finalized history.
- `BLOCK_PAST_DRIFT` (120 s) applies to live tip extensions only; syncing a chain that was
  produced while this node was offline still works.

---

## 7. Reproduce

```bash
python -m pytest            # 77/77 (unit + storage + 9 live suites)
ruff check .
mypy
python tests/e2e_network.py         # three real uvicorn nodes + restart catch-up
python tests/stress_consistency.py  # randomized workload + money conservation
```
