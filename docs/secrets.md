# Secret management on a VPS

Recommended handling for the secrets a public SAN node holds: the node identity
key, TLS certificates, the CA private key and the optional API token.

## 1. What the node stores

| Secret | Default location | Required mode | Notes |
|--------|------------------|---------------|-------|
| Node identity (ML-DSA-44) | `<data-dir>/san_key.json` | `0600` file, `0700` dir | signs blocks, votes, peer records, faucet transfers |
| TLS node key | `/etc/san/certs/node.key` | `0600` | per node, unique |
| TLS CA key | CA machine only, e.g. `/etc/san/ca/ca.key` | `0600` | **never** copy to node machines |
| CA certificate | `/etc/san/certs/ca.crt` | `0644` | public trust anchor, distribute widely |
| API token | `/etc/san/san.env` (`SAN_API_TOKEN`) | `0600` file, `0700` parent | bearer auth for the REST API |
| Database + peer cache | `<data-dir>` | `0700` dir | contains chain state, not keys |

`deploy/install.sh` creates `/var/lib/san` and `/etc/san` with `0700`/`0750`
permissions and `san_key.json` with `0600`; `ledger.NodeIdentity.Save` also
tightens an existing wider key file on every load/save, and
`internal/tlsutil` writes both CA and node private keys `0600`. Regression
tests: `TestSecretFileAndDirectoryPermissions` (`internal/ledger`),
`TestGenerateCertsSharedCASignsBothNodes` (`internal/tlsutil`).

## 2. Recommendations

1. **Dedicate a user.** Run the node as an unprivileged service user (`san`);
   the systemd unit already sets `User=san`, `NoNewPrivileges`, `ProtectSystem`
   and `ReadWritePaths=/var/lib/san`.
2. **Keep the CA key offline.** Generate the CA once (`sanup cert --ca-only
   --dir /etc/san/ca`) and sign node certificates from it on that machine; copy
   only `node.crt`/`node.key` to the target and `ca.crt` to every node. Never
   `scp` `ca.key`.
3. **Use a long random API token.** `openssl rand -hex 32` into `san.env`
   (mode `0600`, owned by root or the service user). The API compares tokens in
   constant time; do not reuse the token anywhere else. Prefer
   `SAN_API_HOST=127.0.0.1` plus a reverse proxy when the API must be public.
4. **Never export secrets in the shell.** Put `SAN_API_TOKEN` in
   `san.env` loaded by systemd, not on the command line (shell history,
   `ps` output) or in world-readable files. The launcher passes the child
   environment directly; no secret is written to the log.
5. **Do not log secrets.** `internal/sanlog` redacts private keys (JSON and
   `key=value`), bearer headers, query tokens and PEM blocks in text and JSON
   modes (`TestRedactionCoversCredentials`,
   `TestRedactionCoversEnvironmentAndTLSSecrets`,
   `TestJSONHandlerRedactsFields`). The node never dumps `os.Environ()`. Still,
   treat log shipping as sensitive and restrict `journalctl` access.
6. **Back up deliberately.** The offline `sanbackup` tool backs up the
   database, not the keys: back up `san_key.json` and the TLS material
   separately, encrypted at rest (e.g. `age`/`gpg`), and restore the key file
   before the database. See [backup-restore.md](backup-restore.md).
7. **Rotate on a schedule and on suspicion.**
   * **API token**: replace `SAN_API_TOKEN` on one node at a time and
     `systemctl restart san-node`; clients must be updated within the rolling
     window.
   * **Node identity key**: identities are chain state; rotating means a new
     address, new stake and a new peer record. Withdraw stake first, stop the
     node, archive the old key, start with the new one, re-deposit.
   * **Node TLS certificate**: re-sign `node.crt`/`node.key` with the same CA
     (`sanup cert --ca-dir ...`) and restart; peers verify the CA, not the
     individual certificate, so no peer action is needed.
   * **CA**: rotating `ca.key` invalidates every node certificate; distribute
     the new `ca.crt` to all nodes before restarting any of them, and expect a
     coordinated handshake failure window otherwise.
8. **Watch for leaks.** Alert on file-mode drift (`find /var/lib/san /etc/san
   -perm /o+r -type f`), unexpected `ca.key` copies, and log lines containing
   `PRIVATE KEY`.
9. **Separate the faucet.** The faucet spends the node's liquid balance; run it
   on a dedicated, deliberately funded node with `SAN_API_TOKEN` set. Public
   mode refuses an unauthenticated faucet (`SAN_ALLOW_INSECURE_PUBLIC=1` is the
   explicit test-only override).

## 3. Quick audit checklist

```bash
sudo stat -c '%a %U %n' /var/lib/san /var/lib/san/san_key.json \
    /etc/san /etc/san/san.env /etc/san/certs/node.key /etc/san/ca/ca.key
# expect 700 dirs, 600 keys and san.env, 644 ca.crt

sudo grep -R "PRIVATE KEY" /var/log/journal 2>/dev/null | head
sudo ss -lntp | grep -E '8000|8765|8769|8770'
```

Related: [public-devnet.md](public-devnet.md) (CA workflow),
[security-review.md](security-review.md) (residual risks),
[backup-restore.md](backup-restore.md) (backup scope).
