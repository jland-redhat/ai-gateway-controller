# Dashboard / manager / admin query performance audit

Date: 2026-11-03
Scope: every read behind `/dashboard`, `/manager`, `/account`, the admin
console, and the partner usage APIs. Measurements were taken against the
dogfood CNPG cluster, which now holds the full November EnMaaS import
(2.35M `usage_events` rows, 1.35M of them in November; 31K `usage_hourly`
rows). Production was not queried or changed.

## What the EnMaaS Performance Readout asks of us

| Readout item | What it requires | Current state |
| --- | --- | --- |
| PSAP-3133 — dashboard load | Pre-aggregated rollups, response caching, no duplicate user queries, cheaper savings math | Done for the six dashboard aggregates (`usage_hourly` + `usage_tool_hourly`), 60s LRU + single-flight, savings CTE reuses the rollup. |
| PSAP-3111 — table growth / full scans | Dashboards must not scan the whole ledger on every load | Done on the rollup path. The manager org table was still scanning raw rows and is fixed in this change. |
| PSAP-3113 — cost recomputation | Use stored `cost_usd` instead of re-pricing every row under load | Done on all rollup reads. Raw paths (`/recent`, user drill-down, parity fallback) still price per row by design. |
| PSAP-3125 — DB observability | Keep DB pressure visible in Grafana | Pool gauges, dashboard cache counters, and manager-cache hit/miss/skip counters are exported. |
| Shared DB with ingestion | Dashboard queries must not compete with the write path | Dashboard reads use the read pool (replica when configured). The two remaining hot reads that must stay fresh use the primary and are bounded. |

## Query inventory and measurements

All timings are warm-cache, single-connection, from `EXPLAIN (ANALYZE,
BUFFERS)` or `\timing` on dogfood. "Rollup" means `usage_hourly` /
`usage_tool_hourly`; "raw" means `usage_events`.

### Admin dashboard (polled every 30s per open tab)

| Endpoint | Source | Measured | Notes |
| --- | --- | --- | --- |
| `/overview` | rollup | 23 ms | 25K rows, seq scan + `count(distinct)` |
| `/groups` | rollup | 36 ms | |
| `/users` (30 rows, no savings) | rollup | 47 ms | plus one directory enrich query (4 ms) |
| `/models` | rollup | ~40 ms | |
| `/tools` | `usage_tool_hourly` | 49 ms | top-9 pie is rendered client-side from this same response; no extra query |
| `/geo` | rollup + directory PK join | 59 ms | |
| `/cost-centers` | rollup + directory PK join | 41 ms | |
| `/recent` | raw, primary-pinned | 1–24 ms | must stay fresh for activity sparks; see follow-up |
| `/directory-users` | `partner_users` | 3 ms | now wrapped in the response cache |

Eleven requests per refresh, cached for 60s with single-flight, so a single
admin tab issues roughly six computations per minute, not eleven per poll.
The new map, sparks, and pies are **client-side renderings of these existing
responses**; they add no new SQL.

### Manager page (loaded on demand, no polling)

| Endpoint | Source | Before | After |
| --- | --- | --- | --- |
| `/api/v1/org/usage` (all org, November) | raw → rollup | **9.28 s**, ~1.07M shared buffers | **71 ms**, 32K buffers |
| `/api/v1/org/charts` (5 aggregates) | rollup | ~250–400 ms total | unchanged |
| `/api/v1/org/person` (3-day drill-down) | raw → rollup | 232 ms | 17 ms |

The org table was the one real regression: it joined every login to every raw
event in the window and re-priced them, then ran an `EXISTS` subquery per
output row. It now aggregates `usage_hourly` and precomputes the manager set
when live rollups are healthy. `LastUsed` is hour-granular on that path (the
rollup's resolution), and a non-hour-aligned `since` starts at the next full
hour so the rollup path cannot count a whole boundary hour of pre-window data.
The raw path keeps the exact requested window and remains the parity fallback.

### User account page

`/api/v1/me/usage/{summary,models,recent}` are scoped to the caller's logins
and bounded by the window. They ride the same rollup/cache paths as the
admin dashboard. No N+1 and no all-user scans.

### Admin console

| Surface | Query | Measured |
| --- | --- | --- |
| People list | paginated `partner_users` + `COUNT(*)` | 3–76 ms with a search term (3.1K rows) |
| Roles | `ListAllPartnerUsers` | ~2 ms |
| Model allowlists | `partner_user_model_allowlists` | tiny table |
| Key search | MaaS API, not the DB | n/a |
| Pricing repricing | chunked `usage_events` scan + `usage_hourly` rebuild | explicit admin job |

The reprice job is the only admin action that touches the whole ledger. It is
chunked and idempotent, but it should still be run off-peak; it is not part of
normal page load.

## Findings and resolutions

1. **Manager org usage scanned the raw ledger (fixed).** An all-organisation
   November window was 9.3 s and read ~8 GB of buffers from the shared primary.
   That is exactly the "dashboard load competing with ingestion" failure the
   readout warns about. Now 71 ms from `usage_hourly` (130× faster), with the
   raw query retained for the parity-fallback case.
2. **`enrichDashboardUsers` read from the primary (fixed).** It was the only
   dashboard aggregate still on the primary while its parent query used the
   read pool. It now uses `reader()`.
3. **`/directory-users` bypassed the response cache (fixed).** It was queried
   on every 30s dashboard refresh despite being static directory data. It now
   rides the 60s cache.
4. **`/recent` filter walk-back (fixed).** Unfiltered it is an index-only
   backwards scan (1 ms). A group filter with no recent matches used to walk
   the timestamp index until it found 20 rows: 22 ms for a small group after
   48K rows scanned, growing with table age. Filtered lookups now carry a
   30-day lower bound (so a group with no matches in that window returns
   quickly and deliberately does not surface older matches), and two
   concurrent indexes match the exact query shapes:
   `((COALESCE(NULLIF(group_name, ''), 'unknown')), timestamp DESC)` for the
   group filter and `(username, timestamp DESC)` for the user filter.
5. **Raw fallback was a cliff (fixed).** If `DASHBOARD_USE_ROLLUPS=true` but
   the parity probe went unhealthy, all eight aggregates silently reverted to
   raw queries over the full window. Parity is now an alert, not a read gate:
   once a deployment has confirmed parity green, a later red check keeps
   serving the continuously rebuilt rollups (and logs/metrics the alert).
   The flag remains the explicit rollback. The last-known verdict is persisted
   in `rollup_meta` and hydrated on boot only while it is younger than the
   parity sync max age (30 minutes by default), so a restart after a long
   outage does not reopen the cliff on an ancient green. There is no runtime
   max-staleness fallback; the runbook alert is the operator signal.
6. **`/org/person` now uses the rollup too (fixed).** The raw drill-down was
   232 ms for the busiest user; the hourly variant is 17 ms. Both org
   rollup paths start at the next full hour for a partial `since`, avoiding
   the boundary-hour over-count inherent in `hour >= since`. `MAX(e.hour)` on
   this path is intentionally hour-truncated: it answers "last active hour",
   not the exact event timestamp.
7. **Manager endpoints are cached per caller and per resolved scope
   (fixed).** `/api/v1/org/usage` and `/api/v1/org/charts` share the dashboard
   TTL and single-flight. The cache wrapper runs `scopeRoot` (the real
   authorization gate) before every lookup and keys on caller + resolved root
   + query, so a revoked role or subtree cannot read a cached response.
   Responses larger than 2 MiB are returned but not stored, and
   `metering_org_cache_{hits,misses,skips}_total` expose the behavior.
8. **Partner batch report moved to the read replica (fixed).**
   `/api/v1/usage/reports` keeps its raw aggregate because it exposes
   `reasoning_tokens`, which `usage_hourly` does not carry; moving it off the
   primary removes the contention without changing the response. The report
   joins directory data from the primary with usage from the replica: usage
   counts can lag by the replica's replication delay, and a directory rename
   can be visible slightly before the matching usage rows. Both are bounded
   by normal replication, not by the report window.

## Follow-ups implemented in the performance follow-up PR

1. **Filtered-recent indexes.** Two concurrent indexes match the exact query
   shapes: `idx_usage_events_group_ts` on
   `((COALESCE(NULLIF(group_name, ''), 'unknown')), timestamp DESC)` and
   `idx_usage_events_user_ts` on `(username, timestamp DESC)`. They run in a
   background index-ensure loop, not the startup migration path: each attempt
   has its own 30-minute timeout, an invalid/not-ready leftover from a
   cancelled concurrent build is dropped and retried, validity is re-checked
   after creation, and failure never fails startup (it only leaves filtered
   `/recent` slower). An `EXPLAIN` integration test pins the predicate and
   ordering shape; for a mid-size group the timestamp walk is already ~20 ms.
2. **Parity red no longer falls back to raw.** Reads are gated on
   flag + backfill-ready + parity confirmed once, with the last verdict
   hydrated from `rollup_meta` on boot only while fresh. A red check is an
   alert; the rollups keep being rebuilt every maintenance tick.
3. **`/org/person` uses `usage_hourly`** when live, with the raw query kept
   as the parity fallback. `/api/v1/usage/reports` goes to the read replica.
   (It exposes `reasoning_tokens`, which the rollup does not carry.)
4. **Manager response cache** for `/api/v1/org/usage` and `/org/charts`,
   keyed by caller + resolved scope root + query, same TTL as the dashboard
   cache, with `scopeRoot` running before every lookup. The wrap lives in
   `cmd/main.go` and a structural test guards it against a rebase dropping it.
   Cached bodies are capped at 2 MiB and hit/miss/skip counters are exported.

## Parity-red runbook

Serving continuously rebuilt rollups while parity is red is an accepted
trade-off: the alternative is full-window raw scans on the shared primary.
There is no automatic runtime max-staleness fallback, so alert on it. Boot-time
hydration of the last green is bounded to the parity sync max age (at most 30
minutes), so a long outage does not silently trust an ancient verdict:

- `metering_rollup_parity_green == 0` for 15 minutes (or the series is absent /
  NaN because the worker could not publish a verdict)
- `time() - metering_rollup_parity_checked_at_seconds > 900` (maintenance stalled)
- `rate(metering_org_cache_skips_total[5m]) > 0` for a sustained period
  (manager responses are exceeding the 2 MiB cache cap)

On alert: check `rollup_meta.parity_healthy` / `parity_checked_at`, inspect the
`ROLLUP PARITY RED` log line for the mismatching shapes, and compare raw vs
`usage_hourly` for that window. If the rollup is wrong, rebuild the affected
hours; if it cannot be repaired quickly, set `DASHBOARD_USE_ROLLUPS=false` as
the explicit rollback to raw (accepting the primary load) until it is fixed.

## Remaining follow-up

`usage_events` retention (PSAP-3111) is still the structural fix for the raw
paths (`/recent`, the parity fallback, and the reprice job). The app-level
bounds above keep those reads predictable, but the table itself keeps growing.
