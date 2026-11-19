#!/usr/bin/env bash
# Preflight and apply the event-idempotency migration.
#
# This is intentionally not run by the application at startup. The apply path
# requires an explicit maintenance window because it archives duplicate rows,
# rebuilds the derived hourly table, and creates a unique index.
set -euo pipefail

MODE="${1:-check}"

# Prefer an existing libpq service configuration. For DATABASE_URL input,
# translate it into temporary service/password files so the password never
# appears in a psql process argument or command line audit record.
if [[ -z "${PGSERVICE:-}" && -z "${PGHOST:-}" && -n "${DATABASE_URL:-}" ]]; then
  PG_TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$PG_TMP_DIR"' EXIT
  DATABASE_URL="$DATABASE_URL" python3 - "$PG_TMP_DIR" <<'PY'
import os
import sys
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

dsn = urlparse(os.environ["DATABASE_URL"])
if dsn.scheme not in {"postgres", "postgresql"}:
    raise SystemExit("DATABASE_URL must use postgres:// or postgresql://")

host = dsn.hostname or "localhost"
port = str(dsn.port or 5432)
user = unquote(dsn.username or "")
password = unquote(dsn.password or "")
database = unquote(dsn.path.lstrip("/"))
query = parse_qs(dsn.query)
sslmode = query.get("sslmode", [""])[0]

directory = Path(sys.argv[1])
service = directory / "pg_service.conf"
pgpass = directory / "pgpass"
service.write_text(
    "[pricetag-migration]\n"
    f"host={host}\nport={port}\ndbname={database}\nuser={user}\n"
    + (f"sslmode={sslmode}\n" if sslmode else "")
)
escaped_password = password.replace("\\", "\\\\").replace(":", "\\:")
pgpass.write_text(f"{host}:{port}:{database}:{user}:{escaped_password}\n")
service.chmod(0o600)
pgpass.chmod(0o600)
PY
  export PGSERVICEFILE="$PG_TMP_DIR/pg_service.conf"
  export PGPASSFILE="$PG_TMP_DIR/pgpass"
  export PGSERVICE=pricetag-migration
elif [[ -z "${PGSERVICE:-}" && -z "${PGHOST:-}" ]]; then
  echo "set DATABASE_URL, PGSERVICE, or PGHOST/PGUSER/PGDATABASE" >&2
  exit 2
fi

psql_cmd() {
  psql -v ON_ERROR_STOP=1 "$@"
}

case "$MODE" in
check)
  duplicate_count="$(psql_cmd -At -c "SELECT COUNT(*) FROM (SELECT event_id FROM usage_events GROUP BY event_id HAVING COUNT(*) > 1) duplicates")"
  if [[ "$duplicate_count" != "0" ]]; then
    echo "duplicate event_id values found: $duplicate_count" >&2
    psql_cmd <<'SQL'
\pset pager off
SELECT event_id, COUNT(*) AS rows, MIN(id) AS retained_id,
       ARRAY_AGG(id ORDER BY id) AS ids
  FROM usage_events
 GROUP BY event_id
HAVING COUNT(*) > 1
ORDER BY rows DESC, event_id;
SQL
    exit 1
  fi
  echo "no duplicate event_id values found"
  ;;
apply)
  [[ "${ALLOW_DOWNTIME:-no}" == "yes" ]] || {
    echo "refusing apply: set ALLOW_DOWNTIME=yes after stopping ingestion" >&2
    exit 2
  }

  # The lock coordinates operators running this migration concurrently. The
  # deployment must still be quiesced: normal event inserts do not acquire
  # this lock until the application-level migration is complete.
  psql_cmd <<'SQL'
SELECT pg_advisory_lock(hashtext('pricetag:event-idempotency'));
CREATE TABLE IF NOT EXISTS usage_event_duplicate_archive (
  original_id BIGINT PRIMARY KEY,
  event_id TEXT NOT NULL,
  archived_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  row_data JSONB NOT NULL
);
BEGIN;
LOCK TABLE usage_events, usage_hourly IN ACCESS EXCLUSIVE MODE;
WITH retained AS (
  SELECT event_id, MIN(id) AS retained_id
    FROM usage_events
   GROUP BY event_id
), duplicates AS (
  SELECT e.*
    FROM usage_events e
    JOIN retained r ON r.event_id = e.event_id
   WHERE e.id <> r.retained_id
)
INSERT INTO usage_event_duplicate_archive (original_id, event_id, row_data)
SELECT id, event_id, to_jsonb(duplicates)
  FROM duplicates
ON CONFLICT (original_id) DO NOTHING;

WITH retained AS (
  SELECT event_id, MIN(id) AS retained_id
    FROM usage_events
   GROUP BY event_id
)
DELETE FROM usage_events e
 USING retained r
 WHERE r.event_id = e.event_id
   AND e.id <> r.retained_id;

TRUNCATE usage_hourly;
INSERT INTO usage_hourly (
  hour, username, group_name, model, provider, requests,
  prompt_tokens, completion_tokens, total_tokens,
  cached_input_tokens, cache_creation_tokens, cost_usd
)
SELECT date_trunc('hour', e.timestamp), e.username, COALESCE(e.group_name, ''),
       e.model, e.provider, COUNT(*), SUM(e.prompt_tokens),
       SUM(e.completion_tokens), SUM(e.total_tokens),
       SUM(e.cached_input_tokens), SUM(e.cache_creation_tokens),
       COALESCE(SUM(e.cost_usd), 0)
  FROM usage_events e
 GROUP BY date_trunc('hour', e.timestamp), e.username,
          COALESCE(e.group_name, ''), e.model, e.provider;
COMMIT;
SELECT pg_advisory_unlock(hashtext('pricetag:event-idempotency'));
SQL

  # CREATE UNIQUE INDEX is deliberately outside a transaction and outside
  # application startup. The service's generic ON CONFLICT clause works both
  # before and after this index exists.
  psql_cmd -c \
    'CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_usage_events_event_id ON usage_events (event_id)'
  ;;
*)
  echo "usage: DATABASE_URL=... $0 check|apply" >&2
  exit 2
  ;;
esac
