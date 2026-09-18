# Backup, restore and resynchronization

Section 23 runbook for SAN node data. The goal: an operator can produce a
verified backup, restore it into a fresh data directory, and validate the
restored database, with peer resynchronization documented as the fallback.

## What lives in a data directory

`sanup` uses `--data-dir` (default `data/go-node`) with:

| File | Kind | Rebuildable? |
|------|------|--------------|
| `node.db` (or `SAN_DB_PATH`) | **Consensus state**: blocks, indexes, receipts, state and storage snapshots, finality checkpoint, schema marker | No: losing it loses history, but it can be re-synced from peers |
| `san_key.json` (`SAN_KEY_FILE`) | **Secret**: node identity private key | No: the only irreplaceable file; never publish or log it |
| `peers-cache.json` (`SAN_PEERS_CACHE`) | Cache: address manager | Yes: discovered again from seeds |
| TLS certificate/key, `san.env` | Deployment config | Re-issued/re-written from your deployment repo |

Consensus state is the LMDB file (one file, `NoSubdir`). The identity key and
the peer cache are ordinary files. Nothing else in the directory is consensus
state.

## Tooling

```
sanbackup backup   --data /var/lib/san --out /var/backups/san-2026-09-18
sanbackup restore  --in  /var/backups/san-2026-09-18 --data /var/lib/san
sanbackup validate --data /var/lib/san
```

`backup`:

1. copies `node.db`, `san_key.json` and `peers-cache.json` into a staging
   directory and `fsync`s each copy;
2. validates the source database before declaring success: it opens the store,
   walks every canonical height (hash exists, block body readable, `index`
   matches, previous-hash chain intact), recomputes the state root from the
   persisted state/storage snapshot and compares it with the head block, and
   checks the persisted finality checkpoint is within the chain;
3. records SHA-256 hashes plus the height, state root and finality height in
   `manifest.json` (mode 0600);
4. renames the staging directory into place atomically.

`restore` verifies every file hash against the manifest, refuses unsafe names,
refuses a non-empty destination, and then re-validates the restored database.

`validate` opens the database read-only-in-spirit and reruns the chain/state
root/finality checks. Use it after every restore and after every upgrade.

## Backup procedure (safe)

```bash
# 1. Stop the node gracefully and wait for the process to exit.
systemctl stop san-node            # or: kill -TERM <pid>; sanup --status
sanup --status                     # confirm it is down (connection refused)

# 2. Back up (the database is not being written while the node is down).
sanbackup backup --data /var/lib/san --out /var/backups/san-$(date +%F)

# 3. Restart.
systemctl start san-node
```

Copy the backup off the machine (object storage/USB); a backup on the same
disk does not survive disk failure. Keep the identity key in your secret store
separately from the database backup.

Warm backups (copying the LMDB file while the node runs) are not supported by
this tooling: LMDB is copy-on-write, but a byte-for-byte copy of a live file
can miss the most recent transaction. Stop the node first; peer
resynchronization is cheap compared with restoring a torn database.

## Restore procedure

```bash
systemctl stop san-node
mv /var/lib/san /var/lib/san.old        # or remove it
sanbackup restore --in /var/backups/san-2026-09-18 --data /var/lib/san
sanbackup validate --data /var/lib/san  # height/state root/finality
systemctl start san-node
sanup --status                          # height catches up to the network
```

If validation fails, do not start the node: restore an older backup or fall
back to resynchronization.

## Fallback: resynchronize from peers

The database is reproducible; the key is not.

1. Keep `san_key.json` (restore it from the backup or your secret store).
2. Start the node with a fresh database path
   (`SAN_DB_PATH=/var/lib/san/node.db` after removing the file, or a new
   directory).
3. Provide a peer source (`SAN_BOOTSTRAP`, `SAN_DNS_SEEDS`, or the local
   registry) and let the node catch up.
4. Confirm with `sanup --status` / `sanbackup validate` that the height and
   state root match a trusted peer (`GET /health` exposes `state_root`).

The node re-joins with the same address and stake; no chain data is trusted
from the backup once the peer-synced chain extends it.

## Tests

* `TestBackupRestoreRoundTrip` — copy, manifest hashes, permissions.
* `TestRestoreDetectsTampering` — a modified file fails the hash check.
* `TestRestoreRejectsNewerManifest`, `TestRestoreRejectsUnsafeManifestPath`.
* `TestValidateStoreConsistentChain`,
  `TestValidateStoreDetectsStateRootMismatch`,
  `TestValidateStoreDetectsFinalityBeyondTip`.
