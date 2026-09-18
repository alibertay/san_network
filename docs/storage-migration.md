# Storage and protocol schema migration policy

This document is the section 22 policy for SAN node databases and wire
schemas: what is versioned, how a node behaves when it meets a newer schema,
the supported upgrade path, and the backup-before-upgrade rule.

## Versioned schemas

| Schema | Constant | Stored/encoded at | Meaning |
|--------|----------|-------------------|---------|
| Chain store layout | `ledger.StoreSchemaVersion` (`internal/ledger/persistence.go`) | `m:schema_version` in the database | Key prefixes, metadata layout, block/receipt/state snapshot encoding used by `ChainStore` |
| Block header | `ledger.SchemaVersion` (`internal/ledger/block.go`) | `version` field of every block header | Hashed block header fields and their encoding |
| P2P protocol | `netnode.ProtocolVersion` | `HELLO` handshake | Wire message set and handshake fields |

Genesis trust anchors the chain: a node that opens a different genesis is not
migrated, it is rejected.

## Detection and refusal behavior

* On open, `ChainStore.ensureSchema` reads `m:schema_version`:
  * missing -> the current version is written (fresh database);
  * malformed -> `store.SchemaMismatch` ("Invalid schema version marker") and
    the node refuses to start;
  * newer than the build -> `store.SchemaMismatch` with the on-disk and
    supported versions and the node refuses to start. A newer schema is never
    opened silently or downgraded;
  * older or equal -> opened in place. The marker is left untouched; a future
    migration rewrites it.
* `BlockFromDict` rejects a block whose `version` is newer than
  `ledger.SchemaVersion`, so a newer block cannot be imported even if it slips
  past persistence.
* The P2P handshake rejects incompatible `ProtocolVersion` values with a
  reason.

Regression tests: `TestSchemaVersionNewerRefused`,
`TestSchemaVersionInvalidRefused`, `TestSchemaVersionOlderAccepted`
(`internal/ledger/schema_version_test.go`), `TestLMDBNewerSchemaRefused`
(`internal/ledger/lmdb_crash_test.go`, `-tags lmdb`).

## What bumps which version

| Change | Action |
|--------|--------|
| New metadata key, new index, additive feature that old code can ignore | Bump `StoreSchemaVersion` and add a forward migration in `ensureSchema` |
| Renamed/removed key prefix, changed value encoding, changed atomic commit shape | Bump `StoreSchemaVersion`, write the migration, keep it idempotent |
| Block header field added/removed/reordered or hash inputs changed | Bump `ledger.SchemaVersion` (consensus-breaking: coordinated upgrade) |
| New wire message or handshake field | Bump `ProtocolVersion`; old nodes reject with a reason |
| REST/SDK-only change | No database migration |

State and storage snapshots are stored under fixed keys (`m:__state`,
`m:__storage`) and are covered by the state root in every block; a migration
that rewrites them must still reproduce the committed state root or refuse to
start.

## Supported upgrade path

1. Stop the node gracefully (SIGTERM) and wait for it to exit.
2. Back up first: `sanbackup backup --data /var/lib/san --out /var/backups/san-<version>`
   (see `docs/backup-restore.md`).
3. Install the new binary.
4. Start the node. Startup migrations (when present) run before any block is
   served; a failure aborts startup.
5. Validate: `sanbackup validate --data /var/lib/san`. Confirm the height,
   state root and finality checkpoint.
6. Rollback: stop the node, restore the backup into a fresh data directory
   (`sanbackup restore --in <backup> --data /var/lib/san`), or resynchronize
   from peers at the old version (see the fallback below).

## Fallback: resynchronize from peers

If a database cannot be migrated or validated, the node is not lost: the
identity key is the only irreplaceable secret. Keep `san_key.json`, start the
node with a fresh `SAN_DB_PATH`, and let it catch up from peers
(`Synchronize`); the address, stake and balances come back from chain data.
This is always available and is the reason the key file is backed up
separately from the (rebuildable) database and peer cache.

## Versioning the backup format

`internal/backup` writes a manifest with `version`. Restoring a manifest newer
than the build fails with `backup manifest version N is newer than this build`
(`TestRestoreRejectsNewerManifest`). Backups are therefore forward-compatible
across node upgrades: keep the old backup until the new revision validates.
