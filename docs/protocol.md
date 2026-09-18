# P2P protocol and version handshake

This document is the normative reference for the SAN node handshake and the
protocol-version upgrade rules. It complements
[docs/gossip.md](gossip.md) (message catalog, discovery) and
[docs/rolling-upgrade.md](rolling-upgrade.md) (operator procedure).

## 1. HELLO / HELLO_ACK

Every `Session` stream starts with a signed `HELLO`; the server answers with a
signed `HELLO_ACK`. Both are canonical JSON objects; the signature covers every
field except `signature`.

| Field | Since | Meaning |
|-------|-------|---------|
| `type` | v1 | `HELLO` or `HELLO_ACK` |
| `protocol` | v1 | `netnode.ProtocolVersion` (**3** since Batch E) |
| `chain_id` | v1 | chain identifier, e.g. `san-devnet-1` |
| `genesis` | v3 | full genesis fingerprint (SHA-256 hex, see section 3) |
| `software` | v3 | semantic software version (`internal/version`) |
| `capabilities` | v3 | sorted capability names this peer supports |
| `public_key` | v1 | ML-DSA-44 public key (hex) |
| `timestamp` | v1 | UNIX seconds; must be within `HelloTTL` (60 s) |
| `signature` | v1 | ML-DSA-44 signature over the canonical payload |

Capabilities currently defined:

| Capability | Meaning |
|------------|---------|
| `genesis-fingerprint-v1` | the peer binds to the full genesis fingerprint (required) |
| `software-version-v1` | the peer reports its semantic software version (required) |

Unknown capabilities are ignored, so a newer peer may advertise additions
without a protocol bump.

## 2. Reject rules

`Node.helloRejectReason` checks the following in order. Every rejection
increments `handshakes_failed` plus `handshake_rejected_<reason>` and logs a
structured `handshake rejected` record with the reason.

| Reason | Rule |
|--------|------|
| `type` | `type` is not `HELLO`/`HELLO_ACK` |
| `malformed` | missing/non-numeric `protocol`, missing `timestamp`, missing key/signature |
| `protocol` | `protocol` is neither the current version nor (in legacy mode) 2 |
| `chain` | `chain_id` differs from the local chain |
| `genesis` | (v3) `genesis` is missing or differs from the local fingerprint |
| `software` | (v3) `software` is missing or empty |
| `capability` | (v3) a required capability is missing |
| `stale` | timestamp is more than `HelloTTL` away from local time |
| `signature` | canonical payload signature does not verify |

The same binding is enforced elsewhere:

* **Peer records** (`SelfPeerRecord`/`VerifyPeerRecord`) carry `genesis`; a
  record without it is rejected unless legacy mode is active.
* **`Status`/`Sync` payloads** carry `genesis`; `Synchronize` refuses a peer
  with a different (or, in strict mode, missing) fingerprint.
* **Persisted state** stores `genesis_fingerprint` and refuses to load a chain
  created from a different genesis.
* **Block/transaction/vote/peer-record signatures** all bind `chain_id`
  already; the genesis fingerprint closes the remaining cross-network gap.

## 3. Genesis fingerprint

The fingerprint is `hex(SHA-256(canonical_json))` over:

```json
{
  "format_version": 1,
  "chain_id": "...",
  "allocations": { "<address>": <base units> },
  "parameters": {
    "block_reward": <units>, "min_block_interval_ms": <ms>,
    "proposer_timeout_ms": <ms>, "block_gas_limit": <gas>,
    "unbonding_period": <blocks>, "slash_bps": <0..10000>,
    "min_validator_stake": <units>
  },
  "validators": ["<address>", ...]
}
```

`internal/genesis` defines the file format and computes the fingerprint; the
published canonical file is [deploy/genesis.json](../deploy/genesis.json).
`REST GET /genesis` exposes it as `genesis_fingerprint`, and `sanup --status`
prints it. A node with a different chain id, allocation, consensus parameters
or validator bootstrap list produces a different fingerprint and is rejected
during the handshake and before sync.

## 4. Versioning and compatibility policy

* `ProtocolVersion` (`internal/netnode/node.go`) is the **wire** version.
  **3** adds the full genesis fingerprint, software version and capabilities to
  HELLO and hardens peer records/sync. Protocol **2** fits in the explicit
  legacy window below.
* **Compatible change** (no bump): adding optional fields/capabilities,
  REST endpoints or fields, or anything a previous peer can ignore.
* **Breaking change** (bump): changing the signed HELLO shape in a way old
  nodes reject, changing required fields, message semantics or block
  validation.
* A protocol bump is a **coordinated** upgrade: the network partitions for the
  duration (new nodes reject old handshakes and vice versa). Follow
  `docs/rolling-upgrade.md` section "Coordinated (breaking) upgrade" when the
  constant changes.
* Rolling upgrades only work when `ProtocolVersion` is unchanged; see
  `docs/rolling-upgrade.md`.

### Legacy window (testing and migration only)

`SAN_ALLOW_LEGACY_HANDSHAKE=1` makes the node:

* accept protocol-2 `HELLO`s (chain id, freshness and signature only, no
  genesis/software/capability checks),
* emit protocol-2 `HELLO`s itself, and
* accept peer records, `Status` and `Sync` payloads without `genesis`,
  falling back to the allocation fingerprint.

This exists for the Go/Python interop harness (the frozen Python reference is
protocol 2) and for one-time mixed-version migrations. A public-devnet node
**refuses** to start with legacy mode enabled unless
`SAN_ALLOW_INSECURE_PUBLIC=1` overrides the profile check. Legacy peers are
only safe on a trusted, isolated test network: they do not prove the full
genesis, so a MITM could pin them to a fork with the same chain id and
allocation.

## 5. Upgrade checklist

For a coordinated protocol bump:

1. Announce the maintenance window and publish the new genesis fingerprint and
   binary checksums.
2. Stop all nodes; install X+1 everywhere; keep data directories.
3. Start the founder first, then seeds, then validators; confirm each
   `sanup --status` line shows the same `genesis` fingerprint.
4. Watch `san_handshakes_failed` / `san_handshake_rejected_*` and
   `san_sync_failures`: any non-zero rate after the window means a node is
   still on the old protocol or another network.
5. Roll back by restoring the previous binaries and data (same schema) or the
   `sanbackup` snapshot when a schema migration was involved.

Regression coverage: `TestHelloAcceptRejectMatrix`, `TestLegacyHandshakeWindow`,
`TestPeerRecordGenesisBinding`, `TestPeerStatusAndSyncCarryGenesis`,
`TestGenesisFingerprintMismatchFailsStartup` (all in
`internal/netnode/genesis_test.go`).
