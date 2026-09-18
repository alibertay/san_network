# Post-quantum bandwidth analysis

Every SAN transaction, vote and peer record carries ML-DSA-44 material. This
document quantifies what that means for a public devnet, using sizes measured
by `cmd/sanbench` (`bench.Sizes()`) and the wire rules of the Go node. Raw
numbers come from a reference run; re-measure on your hardware with:

```sh
go run ./cmd/sanbench --json | jq .results
```

## Measured object sizes

| Object | Bytes | Content |
| --- | ---: | --- |
| Transfer transaction | 7,611 | 2,624-char public key + 4,840-char signature + fields |
| Contract deploy transaction | 7,717 | same + PENA source |
| Contract call transaction | 7,671 | same + function/params |
| Finality vote | 7,620 | public key + signature + height/hash |
| Controller approval (`BLOCK_VOTE_RESPONSE`) | 7,625 | public key + signature + hash |
| Signed peer record | 7,654 | public key + signature + host/ports |
| Handshake (HELLO + HELLO_ACK) | ~15,300 | two signed records |
| Block header (no transactions) | ~380 | index, hashes, timestamp, state root |
| Block, 10 transactions | 76,502 | 7,650 bytes/tx including overhead |
| Block, 100 transactions | 761,672 | 7,616 bytes/tx |
| Block, 1,000 transactions | 7,614,271 | 7,614 bytes/tx |

The constant term is the hex-encoded public key and signature: 1,312 bytes and
2,420 bytes on the wire become 2,624 and 4,840 characters. Keys and
signatures are **not** compressed in the current protocol.

## Traffic model

Assumptions (all conservative, flood-based):

- Every block is gossiped to every peer (full flood broadcast).
- Every transaction is gossiped to every peer (transaction relay).
- Every active validator broadcasts one finality vote per height to every
  peer.
- One block per second (the devnet minimum interval) while the transaction
  rate fits in a block.
- `N` nodes, all validators for the vote term; no reorgs, no sync traffic,
  no handshakes.
- Per-block cost: `block(R) = 380 + 7,614 × R` bytes for `R` transfers.
- Per-node inbound and outbound are symmetric for tx/block gossip:
  `(N-1) × (tx × R + block(R) + vote)`.
- Controller approval: the current `BLOCK_VOTE_REQUEST` carries the **full
  block**, so the proposer sends `block(R)` to each controller per block and
  each controller answers with a 7,625-byte approval.

### Per-node steady-state traffic (Mbps, symmetric)

| Nodes | 1 tx/s | 10 tx/s | 100 tx/s |
| ---: | ---: | ---: | ---: |
| 10 | 1.7 | 11.5 | 110.2 |
| 50 | 9.1 | 62.8 | 600.0 |
| 100 | 18.4 | 126.9 | 1,212.2 |

Controller overhead on top (proposer outbound, per block):
`10 controllers × block(R)` → 0.06 Mbps at 10 tx/s, 6.1 Mbps at 100 tx/s,
61 Mbps at 1,000 tx/s. Each controller additionally receives a full block per
height and returns a 7.6 KB approval.

### Reading the table

- At **1 tx/s** even 100 nodes need <20 Mbps; the PQ overhead is acceptable.
- At **10 tx/s** a 100-node flood consumes ~127 Mbps per node, which is
  beyond many consumer/VPS links once ingress and egress are counted.
- At **100 tx/s** the model exceeds 1 Gbps per node: the design needs relay
  changes before it can scale to three-digit node counts at that rate.
- The dominant term is transaction relay: 7.6 KB × (N-1) × R. Block gossip
  and votes are smaller until blocks approach the transaction rate.

## Safe batching / reduction suggestions

These are **not implemented** (consensus determinism and wire rules must not
change without a protocol version bump), but they are compatible with the
current state machine and are recorded for the next protocol revision:

1. **Do not include the full block in `BLOCK_VOTE_REQUEST`.** The controller
   gate only needs the block hash (and optionally the header) to approve
   availability; the full block is already gossiped separately. This removes
   `10 × block(R)` from the proposer at no safety cost.
2. **Batch transaction relay instead of per-tx flood.** The wire protocol
   already supports `TXS` pages of up to 64 transactions; relaying pages only
   to outbound peers and letting the peer table gossip addresses (rather than
   every tx to every peer) changes O(N²) traffic to near O(N).
3. **Compress the gRPC session** (gzip). Hex-encoded PQ material is
   incompressible per byte, but the JSON envelope and repeated public keys
   compress; expect roughly a 1.3–1.6× reduction, not 10×.
4. **Compact key encoding** (base64url or binary instead of hex) would cut
   ~25% of signature/key bytes; it is a wire-format change and requires a
   protocol version bump.
5. **Certificate/aggregate signatures** for finality votes are the roadmap
   answer (one post-quantum aggregate instead of N per height) but are a
   consensus change and must not be attempted ad hoc.

## Reproducing

`cmd/sanbench` prints the sizes above with `size.transfer_tx`,
`size.finality_vote`, `size.peer_record`, `size.block_*` rows. The traffic
model lives only in this document; it uses the measured sizes and the
flood-gossip rules in `internal/netnode` (`gossipTransaction`, `gossipBlock`,
`broadcastVote`, `sendToControllers`, `requestBlockVote`).
