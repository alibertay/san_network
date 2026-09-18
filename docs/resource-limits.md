# Resource limits audit (section 13)

Every remotely influenced in-memory structure and the cap that bounds it. A
"metric" column names the counter incremented when the limit rejects work
(Go-only counters are appended after the Python-parity `/metrics` prefix and
are rendered by `internal/api`). Configurable knobs are `SAN_*` environment
variables; defaults match the code.

| Structure | Bound | Config | Metric | Test |
|-----------|-------|--------|--------|------|
| Mempool (`transactionPool`) | 8,192 txs, prune-before-reject | `SAN_MAX_MEMPOOL` | `mempool_rejected` | `TestMempoolLimitRejectsAndCounts` |
| Orphan pool (`orphans`) | 64 blocks, oldest evicted | `SAN_MAX_ORPHANS` | `orphans_evicted` | `TestOrphanLimitEvictsAndCounts`, `TestByzantineOrphanFloodIsBounded` |
| Staged votes per height | 8 competing hashes | constants | `staged_votes_rejected` | `TestStagedVoteHashCapRejectsAndCounts` |
| Staged hashes (all heights) | 256 | constants | `staged_votes_rejected` | `TestStagedVoteHashCapRejectsAndCounts` |
| Staged voters per hash | 128 | constant | `votes_dropped_height` | `TestByzantineVoteBufferIsBounded` |
| Vote seen-cache (`seenVotes`) | 8,192 keys, then reset | constant | `vote_seen_cache_resets` | `TestSeenVoteCacheStaysBounded` |
| Vote size | 16,384 canonical bytes | constant | `votes_dropped_invalid` | - |
| Finality vote history | 4,096 heights, older dropped | constant | - | `TestByzantineVoteBufferIsBounded` |
| Peer table (`PEERS`) | `SAN_MAX_PEERS` (64) | `SAN_MAX_PEERS` | `peers_rejected_table` | `TestPeerTableLimitRejectsAndCounts` |
| Inbound sessions per IP | 16 | `SAN_MAX_INBOUND_PER_IP` | `peers_rejected_inbound` | `TestInboundCapsPerIPAndSubnet` |
| Inbound sessions per subnet | 64 | `SAN_MAX_INBOUND_PER_SUBNET` | `peers_rejected_subnet` | `TestInboundCapsPerIPAndSubnet` |
| Peer table per subnet | 16 | `SAN_MAX_PEERS_PER_SUBNET` | `peers_rejected_subnet` | `TestSubnetPeerTableCap` |
| Outbound slots per subnet | 2 | `SAN_OUTBOUND_PER_SUBNET` | - | `TestSelectOutboundPlanKeepsSubnetDiversity` |
| Address-manager cache | `SAN_MAX_ADDR_ENTRIES` (1,024), LRU-ish eviction | `SAN_MAX_ADDR_ENTRIES` | - (eviction is normal) | `TestAddrManagerAddDedupeAndEvict` |
| Invalid peer records | rejected before any map insert | - | `peer_invalid_records` | `TestPeerHandshakeVerifiersNeverPanic` |
| In-flight block requests | 512, reject when full | `SAN_MAX_BLOCK_REQUESTS` | `block_requests_rejected` | `TestBlockRequestLimitRejectsAndCounts` |
| Concurrent sync runs | single-flight (`syncInFlight`) | - | - | - |
| Sync batch / max blocks | 128 / 50,000 | `SAN_SYNC_BATCH`, `SAN_SYNC_MAX_BLOCKS` | - | - |
| Recent receipts | 128 heights, older evicted | constant | - | `TestReorgRebuildsReceiptsTxIndexAndSnapshots` |
| HTTP request body | `SAN_RPC_MAX_BODY` (1 MiB), header + read side | `SAN_RPC_MAX_BODY` | `http_body_rejected` | `TestBodyLimitIncrementsRejectionCounter`, `TestOversizedBodyRejectedWithoutContentLength`, `TestLyingContentLengthRejected` |
| HTTP rate limiter | per-IP sliding window; map capped at 10,000 clients | `SAN_RPC_RATE_LIMIT`, `SAN_RPC_RATE_WINDOW` | - | `TestRateLimit` |
| gRPC message size | `SAN_WS_MAX_SIZE` (1 MiB) recv/send | `SAN_WS_MAX_SIZE` | - | `TestByzantineOversizedEnvelopeDropped` |
| SANVM value size | 65,536 serialized bytes | constant | VMError | `TestValueAndIntegerLimits` |
| SANVM integer size | 4,096 bits | constant | VMError | `TestValueAndIntegerLimits` |
| SANVM stored collections | 65,536 items (Go-only) | `VM.MaxCollectionItems` | VMError | `TestCollectionItemCapRejectsCleanly` |
| SANVM steps / stack / call depth | 100,000 / 1,024 / 64 | `VM.SetMaxSteps`, `SetMaxStack` | typed errors | `TestSANVMLimits` |
| SANVM gas | optional limit per run; opcode costs + operand-size and superlinear charging | transaction `gas_limit` | `OutOfGas` | `TestSANVMLimits`, `TestListRemoveScanIsPricedForLargeLists` |
| Contract bytecode | bounded by the transaction/body size (1 MiB) and gas; no dedicated cap | - | - | `TestFuzzContract` |
| Logs per tx | bounded by steps (100,000) and gas (PRINT = 10 gas); no dedicated cap | - | - | `TestFuzzVMBytecode` |
| Transactions per block | bounded by `block_gas_limit` and the fee market; no count cap (consensus rule shared with Python) | `SAN_BLOCK_GAS_LIMIT` | - | `TestGovernanceGasLimitEffect` |

## Residual items (documented, not silently ignored)

* **Total contract storage is not globally capped.** Per-write gas is priced
  by value size and the resulting state root commits every change, so growth
  is expensive and visible, but there is no "maximum storage bytes" knob. A
  devnet-level cap would be a consensus change (Python parity) and is left
  for a protocol version bump.
* **Logs per transaction have no dedicated cap**, only the step/gas bounds
  above. A block-level receipts budget would likewise be a consensus change.
* **Transactions per block have no count cap**, only the block gas limit,
  serialized-size fees and the 1 MiB transport message limit. Adding a count
  cap changes block validity and is left for a protocol version bump.
* **Address-manager eviction is not counted** (eviction is the intended
  steady state); only invalid records are counted.
* The metrics above named as Go-only do not exist on a Python reference node;
  the common `/metrics` prefix is unchanged.
