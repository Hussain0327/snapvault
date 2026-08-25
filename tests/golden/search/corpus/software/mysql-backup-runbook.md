# MySQL Backup Runbook

This runbook describes the backup process for the `catalog` MySQL database on host `db-catalog-02`.
Priya Nair maintains this document and reviews it every quarter.

## Schedule

A systemd timer, not cron, triggers `mysqldump-nightly.service` at 03:40 UTC.
The unit calls `mysqldump --single-transaction --routines catalog` and streams the output through `zstd -19` before writing it to `/srv/backups/mysql/`.
The compressed file is roughly 9 GB most nights.

## Upload and retention

After the dump completes, `rclone` copies the file to the `sv-db-backups` bucket under `mysql/catalog/`.
Retention here is shorter than for the Postgres cluster: only 7 days of nightly dumps are kept, because the catalog database is fully rebuildable from the product feed within about two hours if needed.

## Manual backup

For an ad-hoc dump, run:

```bash
mysqldump --single-transaction -h db-catalog-02 -u backup_svc catalog > catalog.sql
```

Avoid running this during the 13:00 UTC price-sync job; the two together have pushed replication lag past 90 seconds in the past.

## Restore procedure

Restore into a scratch database with:

```bash
mysql -h scratch-02 -u backup_svc catalog_verify < catalog.sql
```

Unlike the Postgres runbook, MySQL restores here are checked manually rather than on a fixed schedule, because the small size makes automation less valuable.
Priya checks a restore by hand roughly once a month.

## Known issues

The most frequent failure is disk pressure on `db-catalog-02`: the dump directory fills up if two nights of files are left behind by mistake.
A cleanup step now runs before each dump and deletes anything older than 7 days locally, which fixed the recurring outage from January.
