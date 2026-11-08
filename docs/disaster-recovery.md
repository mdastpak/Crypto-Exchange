# Backups and disaster recovery

This document is the plan for keeping the exchange's data through failures and recovering it ([#236](https://github.com/mdastpak/Crypto-Exchange/issues/236)). It covers:

- what is backed up and how;
- how fast the service must recover, and how much it may lose;
- how the backups are checked, and how a restore is drilled;
- what to do in each kind of disaster.

The repository provides the checks and the drill. The backups themselves are taken by the database's tools in each environment. The tables below say which part is which.

## Objectives

| Environment | RPO (data that may be lost) | RTO (time to serve again) | PITR retention | Long-term copies |
| --- | --- | --- | --- | --- |
| Production | 5 minutes; 0 for a committed ledger change once the synchronous standby of [#230](https://github.com/mdastpak/Crypto-Exchange/issues/230) runs | 1 hour | 35 days | Monthly, for 7 years or what regulation requires |
| Staging | 1 hour | 4 hours | 7 days | None |
| Development | None | None | None | None |

The RPO bounds what a **loss of the primary and all its standbys** costs. A failover to a synchronous standby loses nothing. `admin backup status -rpo` checks the RPO, and it defaults to the production value.

## What is backed up

| Store | Holds | Backup |
| --- | --- | --- |
| PostgreSQL | Everything that must survive: users, sessions, currencies, balances and the ledger, the audit log, the outbox, the wrapped data keys, platform controls | Continuous WAL archiving plus base backups, encrypted, in immutable storage in another region or account (see below) |
| Vault or OpenBao | The key-encryption key that wraps the data keys ([data-protection.md](data-protection.md)) | Its own storage snapshots, encrypted, kept apart from the database backups, with unseal or recovery keys split between people |
| Redis | The currency cache, sign-in code sessions and attempt counters, the token blacklist, rate-limit buckets | None. After a loss, the cache reloads from PostgreSQL. Pending sign-in codes are lost, so users request new ones. Rate-limit counters start over. The blacklist loses tokens revoked before the loss. Every user request also checks its session in PostgreSQL ([#120](https://github.com/mdastpak/Crypto-Exchange/issues/120)), so a revoked session stays revoked. |
| RabbitMQ | Queued sign-in code deliveries, and events in flight to WebSocket gateways | None. Unsent sign-in codes are lost, so users request new ones. Events are written to the outbox in PostgreSQL first ([#89](https://github.com/mdastpak/Crypto-Exchange/issues/89)), and the relay sends again what the broker did not confirm. |

**A database backup is useless without the matching KEK.** The wrapped data keys in `data_keys` unwrap only with the KEK that wrapped them. Restore the key management service from the backup that matches the database backup, not from a newer one whose KEK was rotated and the old version deleted. `admin restore verify -keys` proves the pair matches.

### PostgreSQL

Use pgBackRest, WAL-G, or the point-in-time recovery of a managed service. Whichever tool you use:

- **Archive every WAL segment.** Set `archive_mode = on`, and set `archive_command` (or the tool's own) to push segments to the backup storage.
- **Bound the archive delay.** Set `archive_timeout` to at most the RPO, for example `60s`. An idle database otherwise keeps its newest changes in a segment that is never archived. `admin backup status` fails when it is not set.
- **Take base backups regularly.** For example, a full backup every week and a differential every day. Recovery replays the WAL from the newest base backup before the target, so frequent base backups shorten the RTO.
- **Encrypt the backups.** For example, with `repo1-cipher-type = aes-256-cbc` in pgBackRest, or `WALG_LIBSODIUM_KEY` in WAL-G. Keep the passphrase in the secret store, apart from the database credentials.
- **Store them immutably, off-site.** Use object storage in another region or account, with object lock (S3 Object Lock in compliance mode, or the equivalent) for at least the PITR retention. The database server's credentials may write new objects, but must not delete or overwrite them. A compromised server, or ransomware, then cannot remove the backups.
- **Expire them by retention.** Keep the PITR window and the monthly copies above. With pgBackRest, set `repo1-retention-full` and `repo1-retention-archive`. Run expiry from a host other than the database.

A minimal pgBackRest configuration of the primary, to adapt:

```ini
[global]
repo1-type=s3
repo1-s3-bucket=exchange-backups-<env>
repo1-s3-region=<another region>
repo1-s3-endpoint=s3.<region>.amazonaws.com
repo1-path=/pgbackrest
repo1-cipher-type=aes-256-cbc
repo1-retention-full=5
repo1-bundle=y
process-max=4
compress-type=zst

[exchange]
pg1-path=/var/lib/postgresql/data
```

Add these settings to `postgresql.conf`:

```ini
archive_mode = on
archive_command = 'pgbackrest --stanza=exchange archive-push %p'
archive_timeout = 60s
wal_level = replica
```

## Monitoring

Run `admin backup status` against the primary every minute, from a cron job or a monitoring probe, and alert when it exits with `1`. It reads `pg_stat_archiver`, and fails when:

- archiving is off;
- `archive_timeout` is 0 or longer than the RPO;
- no segment was archived yet, or the newest is older than the RPO (`-rpo`, 5 minutes by default);
- archiving failed and has not succeeded since. WAL then piles up on the primary until its disk fills, so this alert is urgent.

```sh
admin -config /etc/exchange/config.yaml backup status -rpo 5m
```

The same command prints the newest archived segment and the counts of archived and failed attempts. Also alert, from the backup tool, when no base backup has completed for longer than its schedule plus a margin; for example, with `pgbackrest info --output=json`. Metrics for both alerts belong in the observability work of [#50](https://github.com/mdastpak/Crypto-Exchange/issues/50).

## Restore drills

A backup that has never been restored is not a backup. Restore every month on staging, and after any change to the backup setup:

1. Choose a target time, and note an audit event from just before it (`GET /admin/audit`) and one from just after it.
2. Restore the newest base backup before the target into an isolated environment, with the archived WAL up to the target. For example, `pgbackrest --stanza=exchange --type=time --target="<time>" --target-action=promote restore`. Restore the key management service from its matching backup.
3. Check the restored database with the binary of the release that was running at the target. It never migrates or changes the database:

   ```sh
   admin -config restored.yaml restore verify -not-before <event before> -not-after <target> -keys
   ```

   It fails, and the restored database must not be put in service, when:
   - the schema version is not the binary's, or a migration is marked dirty;
   - a constraint is not validated, or an index is invalid;
   - a balance breaks its invariants ([#43](https://github.com/mdastpak/Crypto-Exchange/issues/43));
   - the audit log's append-only triggers are missing or disabled;
   - the newest audit event is outside the target;
   - a data key does not unwrap with the configured KEK.

   It also prints the rows of the main tables, to compare with the source at the target. The ledger reconciliation of [#60](https://github.com/mdastpak/Crypto-Exchange/issues/60) and the row seals of [#139](https://github.com/mdastpak/Crypto-Exchange/issues/139) will add their checks to it.
4. Record the time each step took. The total must stay within the RTO.

`scripts/dr/restore-drill.sh` runs the same drill on Docker, with archived WAL and `pg_basebackup` in place of the backup tool. The **Restore drill** workflow runs it every month, and on pull requests that change the migrations or the checks. Run it locally with `scripts/dr/restore-drill.sh`.

## Runbook

In every scenario:

- The on-call engineer leads. The head of engineering decides on a restore that loses data.
- Compliance decides what to tell regulators.
- Put the service under maintenance first (`admin platform maintenance`, or the admin API; see [api.md](api.md#maintenance-and-lockdowns)), with a message for users. Lift it only after `restore verify` passes and a smoke test of sign-in and the currency list succeeds.
- Every step is recorded in the incident's timeline.

| Scenario | What to do |
| --- | --- |
| **The primary fails** | Promote the synchronous standby ([#230](https://github.com/mdastpak/Crypto-Exchange/issues/230)). No restore is needed, and nothing committed is lost. Rebuild a new standby from a base backup. |
| **The primary and its standbys are lost, or the region fails** | Restore the newest base backup with all archived WAL in another region, and restore the key management service there. Run `restore verify -keys`. Point the instances at it and lift the maintenance. Up to the RPO of changes may be lost. Reconcile the ledger against the custody records before reopening withdrawals ([#60](https://github.com/mdastpak/Crypto-Exchange/issues/60), [#70](https://github.com/mdastpak/Crypto-Exchange/issues/70)), and keep them locked until then (`admin platform lockdown -feature withdrawals`). |
| **A bad migration, or an accidental `DELETE` or `UPDATE`** | Lock the affected features, or put the service under maintenance. Find the time of the change in the audit log or the PostgreSQL log. Restore to just before it in an isolated environment, with `restore verify -not-after <the change>`. Then either copy the lost rows back into production, if later changes must be kept, or switch to the restored database, if the loss since then is accepted. The decision is the head of engineering's. |
| **Ransomware, or a compromised server** | Isolate the hosts, and rotate the database, storage, and key management credentials. The immutable backups cannot have been deleted. Restore to before the earliest sign of compromise in a clean environment, built from images, not from the compromised hosts. Run `restore verify -keys`. Rotate the JWT signing keys and every data key afterwards ([data-protection.md](data-protection.md)). |
| **The key management service is lost** | Restore it from its backup, which must hold the KEK version that wrapped the current data keys, and unseal it with the split keys. Without it, sealed rows and encrypted fields cannot be read; see [data-protection.md](data-protection.md). |
| **Redis or RabbitMQ is lost** | Replace it empty. Nothing is restored; see [What is backed up](#what-is-backed-up). |

Tell users through the maintenance message. After any restore that lost data, tell the affected users, and compliance, what was lost.
