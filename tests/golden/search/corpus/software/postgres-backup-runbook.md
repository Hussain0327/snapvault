# PostgreSQL Backup Runbook

This runbook covers the nightly backup process for the `orders` Postgres database running on `db-primary-01`.
Dana Whitfield owns this runbook and should be paged first if a backup fails.

## Schedule

A cron job on `db-primary-01` runs `pg_dump` every night at 02:15 UTC and writes the compressed dump to `/var/backups/postgres/`.
The job is defined in `/etc/cron.d/pg-backup` as:

```text
15 2 * * * postgres /usr/local/bin/pg-nightly-backup.sh
```

The backup script pipes the dump through `gzip -9` before writing it to disk, then uploads the file to the `sv-db-backups` S3 bucket under the prefix `postgres/orders/`.
Backups older than 14 days are deleted from the bucket by a lifecycle rule.

## Manual backup

To take a manual backup outside the schedule, run:

```bash
pg_dump -h db-primary-01 -U backup_user -F c orders > orders.dump
```

The dump takes about six minutes against the current 40 GB database.
Do not run a manual backup during the 09:00-11:00 UTC batch import window, since the extra I/O has caused import jobs to time out twice this year.

## Restore procedure

Restoring to a scratch instance is the standard way to verify a backup before trusting it.
Provision a temporary instance, then run:

```bash
pg_restore -h scratch-01 -U backup_user -d orders_verify orders.dump
```

Restore verification runs every Sunday at 04:00 UTC and posts the result to the `#db-alerts` channel.
A restore that takes longer than 25 minutes should be treated as a warning sign that the dump grew unexpectedly.

## Retention and storage

The S3 bucket currently holds about 14 days of nightly dumps, totaling roughly 68 GB.
Increasing retention to 30 days was discussed in the March infra review but was deferred because of the added storage cost.

## Failure response

If the cron job fails, PagerDuty pages the on-call engineer within five minutes.
The most common cause of failure in the last quarter was the `backup_user` role losing its `REPLICATION` privilege after a permissions audit; re-granting it resolves the issue immediately.
Check `/var/log/postgres/pg-backup.log` for the exact error before escalating.
