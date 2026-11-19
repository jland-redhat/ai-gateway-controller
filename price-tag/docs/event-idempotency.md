# Event Idempotency Migration

The gateway delivers usage CloudEvents at least once. The `event_id` is the
idempotency key, but historical databases may contain duplicate rows from
cutover/recovery operations. This migration must be run explicitly; it is not
part of application startup.

## Preconditions

- Provide `DATABASE_URL` through the approved secret mechanism, or configure
  standard libpq `PGSERVICE`/`PG*` variables. The script translates a
  `DATABASE_URL` into temporary `PGSERVICEFILE`/`PGPASSFILE` files, so the
  password is not passed in a process argument.
- Stop or drain gateway event ingestion for the maintenance window.
- Take/verify a database backup.
- Run the read-only preflight first:

```bash
DATABASE_URL="$DATABASE_URL" ./scripts/event-idempotency-migration.sh check
```

## Apply

The apply path archives duplicate rows as JSON, keeps the lowest `usage_events.id`
for each `event_id`, rebuilds `usage_hourly` from the retained ledger, and then
creates the unique index concurrently:

```bash
ALLOW_DOWNTIME=yes DATABASE_URL="$DATABASE_URL" \
  ./scripts/event-idempotency-migration.sh apply
```

The script uses a PostgreSQL advisory lock to prevent two operators from
running it concurrently. `ALLOW_DOWNTIME=yes` is an explicit acknowledgement
that normal ingestion has been stopped or drained; the database lock protects
the cleanup/rebuild transaction but does not coordinate with application code
that has not adopted the migration yet.

After the index exists, duplicate CloudEvents are acknowledged by the
application's `ON CONFLICT DO NOTHING` insert path and do not increment the
hourly rollup twice.

## Verification

```sql
SELECT event_id, COUNT(*)
  FROM usage_events
 GROUP BY event_id
HAVING COUNT(*) > 1;

SELECT indexname
  FROM pg_indexes
 WHERE tablename = 'usage_events'
   AND indexname = 'uq_usage_events_event_id';

SELECT (SELECT COUNT(*) FROM usage_events),
       (SELECT COUNT(*) FROM usage_hourly);
```

Keep `usage_event_duplicate_archive` until the normal retention/backup policy
has covered the migration and the post-migration parity check is green.
