# SAN Network benchmarks

`cmd/sanbench` measures the Go implementation (`cmd/sannode`) and prints a
human table or machine-readable JSON. Raw TPS numbers are meaningless without
methodology: this document defines what is measured, how, on which hardware,
and what the numbers do **not** cover.

## How to run

```sh
go run ./cmd/sanbench            # fast default mode (no network lab)
go run ./cmd/sanbench --json     # machine-readable results
go run ./cmd/sanbench --full     # larger workloads + local network lab
go run ./cmd/sanbench --network  # only the local propagation/sync lab
```

The fast default mode finishes in a few seconds on a laptop CPU; `--full`
adds the 5000-transaction block and the two-node network lab and takes a few
minutes.

## Methodology

| Benchmark | Measured operation | Method |
| --- | --- | --- |
| `mldsa44.keygen` | ML-DSA-44 key pair generation | `crypto.GenerateKeypair` (CIRCL, FIPS 204), ≥5 iterations |
| `mldsa44.sign` / `.verify` | detached sign and verify | loop over a fixed message with one key pair |
| key/signature sizes | wire sizes | compile-time constants from `internal/crypto` |
| `transaction.sign` | full payload signing (canonical serialization + ML-DSA) | plain SAN transfer payloads, distinct nonces |
| `transaction.validation` | `ledger.VerifyTransaction` | signature check + canonical re-serialization |
| `transaction.serialize` | `canonical.Marshal` | same payloads, timed separately |
| `transaction.average_bytes` | signed payload size | includes the 2624-char public key and 4840-char signature as hex |
| `size.*` | post-quantum wire sizes | signed with a real key and canonical encoding; see `docs/pq-bandwidth.md` |
| SANVM workloads | `ContractManager` deploy + repeated calls | PENA compiled with `sanvm.CompilePena`, isolated storage per workload |
| `block_validation.Ntx` | `netnode.Node.VerifyBlock` | pre-built signed blocks; block construction excluded; in-memory chain |
| `network.block_propagation` | seed finds a new block → joiner reports the same height | local `sanup` nodes, median of 5 |
| `network.transaction_commit` | submission on the seed → joiner sees the sender nonce advanced | local nodes, median of 5 |
| `network.sync_throughput` | joiner restarts ~10 blocks behind → catches up | local nodes, blocks/second |

Wall-clock time is read with Go's monotonic clock. The VM benchmarks include
a deployment warm-up before timing. Rates are `operations / elapsed seconds`
and may round to zero on hosts with a coarse timer when the operation count is
very small (Windows smoke runs); the committed default iteration counts avoid
that.

Rates are single-process and single-stream. They are not parallel throughput,
they do not include consensus, networking (except the explicit network lab) or
disk I/O, and they are not a promise of public-devnet TPS.

## Reference results

Host: AMD Ryzen 7 8845HS (8 cores / 16 threads), 32 GB RAM, Windows 11,
Go 1.26.0, `crypto/circl.mldsa44`. Run with `go run ./cmd/sanbench` (default
mode, 200 iterations). Re-run on your own hardware before quoting.

| Benchmark | Value | Unit |
| --- | --- | --- |
| ML-DSA-44 keygen | 12,436 | ops/sec |
| ML-DSA-44 sign | 14,460 | ops/sec |
| ML-DSA-44 verify | 29,132 | ops/sec |
| ML-DSA-44 public key | 1,312 | bytes |
| ML-DSA-44 private key | 2,560 | bytes |
| ML-DSA-44 signature | 2,420 | bytes |
| Transaction sign | 4,341 | ops/sec |
| Transaction validation | 18,317 | ops/sec |
| Transaction serialization | 21,727 | ops/sec |
| Transaction average size | 7,613 | bytes |
| VM simple transfer (`inc`) | 132,899 | calls/sec |
| VM storage-heavy (`fill(200)`) | 198,748 | calls/sec |
| VM arithmetic-heavy (`work(200)`) | 200,300 | calls/sec |
| VM SANRC20 `transfer` | 34,020 | calls/sec |
| VM AMM `swap0For1` | 25,669 | calls/sec |
| Block validation, 10 tx | 4,977 tx/sec | 497.7 blocks/sec |
| Block validation, 100 tx | 5,095 tx/sec | 51.0 blocks/sec |
| Block validation, 1,000 tx | 5,031 tx/sec | 5.0 blocks/sec |

Wire sizes from the same run:

| Object | Size |
| --- | --- |
| Transfer transaction | 7,611 bytes |
| Contract deploy transaction | 7,717 bytes |
| Contract call transaction | 7,671 bytes |
| Finality vote | 7,620 bytes |
| Controller approval | 7,625 bytes |
| Signed peer record | 7,654 bytes |
| Block with 10 tx | 76,502 bytes |
| Block with 100 tx | 761,672 bytes |
| Block with 1,000 tx | 7,614,271 bytes |

The dominant term everywhere is the ML-DSA-44 material encoded as hex: a
public key (1,312 B → 2,624 hex chars) and a signature (2,420 B → 4,840 hex
chars) are attached to every transaction, vote and peer record. See
`docs/pq-bandwidth.md` for the resulting bandwidth model.

## Local network lab (`--network`)

The lab spawns a founder and a joiner through `cmd/sanup` on loopback, lets
the founder build a 15-block backlog, then measures joiner catch-up, block
propagation and transaction commit latency. The benchmark nodes get a raised
REST rate limit via the harness so health polling does not throttle the
measurement; this does not change any node default.

Reference run (same host as above):

| Benchmark | Median | Notes |
| --- | --- | --- |
| Sync throughput | 26.4 blocks/sec | 15-block catch-up from start |
| Block propagation | 5.8 ms | seed observed → joiner observed, 5 ms poll resolution |
| Transaction commit | 7.0 ms | `POST /transaction` accepted → joiner nonce advanced |

Results depend on the host scheduler and are only meaningful relative to each
other; the JSON report includes every sample, the median and p90.

## Caveats

- Windows results can be skewed by timer granularity and antivirus; the
  reference numbers above were produced on Windows, so Linux numbers may be
  higher.
- `block_validation` disables block-signature and state-root requirements and
  pools at most 32 signing keys; it exercises the same transaction, execution
  and fee-validation code as a live block but not validator/proposer checks.
- VM call rates measure the interpreter only: gas metering is included, but
  state persistence, networking and consensus are not.
- Async random restarts, reorgs and byzantine traffic are exercised by
  `cmd/sansoak`, not by `sanbench`.
