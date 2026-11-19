# Tool-rollup normalization rollout

The `tool_rollup_normalization_version` marker rebuilds
`usage_tool_hourly` when the user-agent normalization expression changes.
During a rolling deployment, however, old pods can still write the previous
classification. Use the two-phase rollout below whenever the normalization
version changes.

## Phase 1: drain old writers

Set both flags to `false` and roll out that configuration using the currently
deployed image:

```text
DASHBOARD_USE_ROLLUPS=false
LIVE_ROLLUPS_ENABLED=false
```

Wait until every pod is running the configuration with both flags disabled.
Verify the rollout has no old pods before continuing. Dashboard reads use the
raw ledger during this phase, and ingestion no longer writes either rollup
table synchronously.

## Phase 2: deploy and activate the new normalization

Deploy the new metering image while both flags remain `false`. Startup rebuilds
the additive tool rollup from `usage_events` using the new normalization and
records the new normalization version. Keep the flags disabled until all pods
are running the new image and the rebuild has completed.

Then set both flags to `true` and roll out that configuration:

```text
DASHBOARD_USE_ROLLUPS=true
LIVE_ROLLUPS_ENABLED=true
```

Verify the rollup status is ready/healthy and confirm Pi variants such as
`pi`, `pi/…`, and `pi (darwin …)` appear as one `Pi` category in the tool
dashboard. Recent Activity continues to display the original user-agent.

## Rollback

If validation fails, set `DASHBOARD_USE_ROLLUPS=false` and
`LIVE_ROLLUPS_ENABLED=false` first. Roll back the image only after raw reads
and non-rollup ingestion are active. This prevents an older writer from
mixing its normalization into a table produced by the newer image.
