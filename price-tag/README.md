# PriceTag Metering Service

A development metering backend for AI inference gateways — provides CloudEvents-compatible token usage ingestion, per-user balance checks, and usage aggregation for testing an `external-metering` gateway filter.

**Use this to test metering without deploying OpenMeter or a production billing system.**

## What it does

| Endpoint | Purpose |
|----------|---------|
| `POST /api/v1/events` | Ingest CloudEvents v1.0 token usage events |
| `GET /api/v1/customers/{id}/entitlements/{key}/value` | Check user balance (quota - usage) |
| `GET /dashboard` | Authenticated usage dashboard |
| `GET /api/v1/org/usage` | Authenticated, scope-limited team usage |
| `GET /health`, `GET /ready` | Liveness and readiness probes |

## Architecture

- **[docs/dashboard-architecture.md](docs/dashboard-architecture.md)** —
  the full data path with diagrams and measured numbers: how a request
  becomes a frozen-cost ledger row, the transactionally-maintained
  hourly rollups, the replica + cache + read-switch layers, the standing
  consistency check that auto-falls back on drift, and the ops knobs.
  Panel reads: 19.8 ms → 1.2 ms, flat in ledger size.
- [docs/dashboard-scaling-plan.md](docs/dashboard-scaling-plan.md) — the
  plan of record and review history behind those layers.
- Production Postgres: [deploy/cnpg/README.md](deploy/cnpg/README.md)
  (HA, backups, restore) and
  [deploy/readonly-replica/README.md](deploy/readonly-replica/README.md)
  (read-path grants). Full-cluster bring-up lives in the
  [PriceTag deployment repository](https://github.com/redhat-et/pricetag).

## Quick Start

### Docker Compose (local)

```bash
docker compose up
```

### Kubernetes

```bash
kubectl apply -f deploy/
```

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | — | PostgreSQL connection string |
| `PORT` | No | `8080` | HTTP listen port |
| `MONTHLY_TOKEN_QUOTA` | No | `100000000` | Legacy per-user monthly token safety net for the entitlement endpoint. The gateway enforces the response; set a non-positive value only for explicitly unlimited deployments. |
| `QUOTA_ENFORCEMENT_ENABLED` | No | `false` | Controls dollar/token quota blocking. Usage accounting remains active; set `true` only after quota policy is ready. |
| `QOS_ENABLED` | No | `false` | Enables weighted-token QoS classification and the `qosClass`/`qosKeyAlias` response fields. |
| `QOS_PROVIDERS` | No | `cb-glm` | Comma-separated usage providers counted toward the QoS windows. |
| `QOS_WINDOW_SECONDS` | No | `60` | Short classification window. |
| `QOS_HOLD_WINDOW_SECONDS` | No | `300` | Longer guard window that prevents rapid class flapping. |
| `QOS_PREMIUM_MAX_TOKENS` | No | `600000` | Short-window premium/main boundary in weighted tokens. |
| `QOS_RATELIMIT_MIN_TOKENS` | No | `2000000` | Short-window main/ratelimit boundary in weighted tokens. |
| `QOS_HOLD_MAIN_TOKENS` | No | `3000000` | Hold-window premium/main boundary in weighted tokens. |
| `QOS_HOLD_RATELIMIT_TOKENS` | No | `10000000` | Hold-window main/ratelimit boundary in weighted tokens. |
| `QOS_CACHE_READ_WEIGHT` | No | `0.05` | Weight applied to cached input tokens in QoS accounting. |
| `QOS_CLASS_CACHE_SECONDS` | No | `15` | Maximum staleness of the short-window class cache. |
| `QOS_HOLD_CACHE_SECONDS` | No | `60` | Maximum staleness of the hold-window cache. |
| `QOS_EXEMPT_TAG_KEY` | No | `origin` | Partner-user tag key that exempts a user from QoS. |
| `QOS_EXEMPT_TAG_VALUE` | No | `loadtest` | Tag value that triggers the QoS exemption. |
| `QOS_KEY_ALIAS_PREMIUM` | No | `premium` | Gateway key-ring alias for the premium class. |
| `QOS_KEY_ALIAS_MAIN` | No | `main` | Gateway key-ring alias for the main class. |
| `QOS_KEY_ALIAS_RATELIMIT` | No | `main` | Gateway key-ring alias for the ratelimit class. |
| `M2M_AUTH_REQUIRED` | No | `false` | Require a bearer token on gateway-to-metering endpoints. Enable only with matching gateway header injection. |
| `M2M_SHARED_SECRET` | No | — | Runtime secret compared against the M2M bearer token; never commit it. Required when `M2M_AUTH_REQUIRED=true`. |
| `DASHBOARD_USE_ROLLUPS` | No | `false` | Serve aggregate dashboard panels from `usage_hourly` after backfill and parity are green. The raw path remains the rollback switch. |
| `LIVE_ROLLUPS_ENABLED` | No | follows `DASHBOARD_USE_ROLLUPS` | Maintain `usage_hourly` synchronously during ingestion. With raw reads and the default `false`, this avoids the hour-wide advisory lock. Explicit `false` with rollup reads makes dashboard data eventually consistent. |
| `ROLLUP_REFRESH_SECONDS` | No | `300` | Maintenance interval for recent-hour rebuilds and standing parity checks. Monitor `/api/v1/admin/rollups` for refresh lag when live writes are disabled. |
| `ADMIN_USERS` | No | — | Break-glass admin identities used only when the Partner identity store cannot be read. This does not grant access to an unprovisioned or demoted user while the store is healthy. |
| `SUPERADMIN_USERS` | No | — | Break-glass operator identities used only when the Partner identity store cannot be read. Seed an active `super_admin` row in `partner_users` before first protected deployment. |
| `OIDC_ENABLED` | No | `false` | Enables the optional corporate SSO login. Keep disabled until the issuer, redirect URI, and rollout are reviewed. |
| `OIDC_ISSUER_URL` | When OIDC is enabled | — | Exact corporate OIDC issuer URL used for discovery and issuer validation. |
| `OIDC_CLIENT_ID` | When OIDC is enabled | — | Confidential OIDC client ID. |
| `OIDC_CLIENT_SECRET` | When OIDC is enabled | — | Confidential OIDC client secret; inject from a deployment Secret, never commit it. |
| `OIDC_REDIRECT_URL` | When OIDC is enabled | — | Must exactly match the registered callback, for example `/oauth/callback`. |
| `OIDC_POST_LOGOUT_REDIRECT_URL` | No | — | Must be registered with the provider. Sent as `post_logout_redirect_uri` after RP-initiated logout; leave empty to use the provider's own signed-out page. |
| `OIDC_SCOPES` | No | `openid profile email groups` | Space- or comma-separated OIDC scopes. |
| `SESSION_SECRET` | When OIDC is enabled | — | Signs session and OIDC state cookies. Inject a long random value from a deployment Secret; without it sessions do not survive a restart and SSO logins are invalidated. |
| `SESSION_COOKIE_SECURE` | No | `true` | Keep enabled in shared/stage/production. Set `false` only for local HTTP testing. |
| `OIDC_DEVELOPMENT_MODE` | No | `false` | Explicit local/stage-only gate required before enabling OIDC diagnostics or test personas. Keep `false` in production. |
| `OIDC_TEST_LOGIN` | No | `false` | Shows the mock-IdP persona picker. Requires `OIDC_DEVELOPMENT_MODE=true` **and** a loopback request host; it is never shown on a shared dashboard hostname. |
| `OIDC_CLAIMS_DEBUG` | No | `false` | Development/stage-only diagnostic page after OIDC callback. Requires `OIDC_DEVELOPMENT_MODE=true`; never enable in production. |

### QoS alias precedence

When QoS is enabled, metering's `qosKeyAlias` is authoritative for the
`enmaas_policy` gateway filter. The gateway's local `key_alias_*` settings are
only a fallback for older metering responses or when QoS is disabled. If a
deployment customizes gateway aliases, set the matching
`QOS_KEY_ALIAS_PREMIUM`, `QOS_KEY_ALIAS_MAIN`, and
`QOS_KEY_ALIAS_RATELIMIT` values on metering as well, and deploy the metering
configuration before enabling gateway active-key selection.

## CloudEvents Format

The service accepts [CloudEvents v1.0](https://cloudevents.io) with this data schema:

```json
{
  "specversion": "1.0",
  "id": "evt-<uuid>",
  "source": "ai-gateway",
  "type": "inference.tokens.used",
  "subject": "alice",
  "time": "2026-06-10T12:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "user": "alice",
    "group": "engineering",
    "subscription": "premium",
    "provider": "anthropic",
    "model": "claude-opus-4-8",
    "prompt_tokens": 150,
    "completion_tokens": 80,
    "total_tokens": 230,
    "cached_input_tokens": 12000,
    "cache_creation_tokens": 100,
    "reasoning_tokens": 0,
    "duration_ms": 1200
  }
}
```

This format is compatible with [OpenMeter](https://openmeter.io) and can be adapted for other billing backends.

## Supported Token Types

| Token Type | Description | OpenAI field | Anthropic field |
|-----------|-------------|-------------|-----------------|
| Input (new) | Non-cached input tokens | `prompt_tokens` | `input_tokens` |
| Output | Generated tokens | `completion_tokens` | `output_tokens` |
| Cached read | Tokens read from cache | `prompt_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| Cache write | Tokens written to cache | — | `cache_creation_input_tokens` |
| Reasoning | Chain-of-thought tokens | `completion_tokens_details.reasoning_tokens` | — |

## Using This Service as a Metering Provider

This service is a **drop-in metering backend**, in real use behind an AI inference gateway. The gateway's `external-metering` filter sends CloudEvents to whatever URL is configured — this service, OpenMeter, or a commercial billing system — and the API surface is identical across them, so integrations built against one carry over to the others.

```
Development:  meteringURL → pricetag-metering (this repo)
Staging:      meteringURL → OpenMeter
Production:   meteringURL → commercial or custom billing system
```

The CloudEvents v1.0 format is the integration contract — any backend that accepts this schema works.

## Model Pricing

Cost calculation uses per-model pricing from the `model_pricing` table. Prices
are sourced from [LiteLLM's community-maintained database](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) (MIT licensed).

**On startup**, the service:
1. Fetches the latest pricing from LiteLLM's GitHub (10s timeout)
2. Filters to relevant models (Claude, GPT, Gemini)
3. UPSERTs into `model_pricing` (corrects stale prices, adds new models)
4. Falls back to a bundled snapshot if the fetch fails (air-gapped / offline)

**To refresh pricing without restarting:**
```bash
curl -X POST https://<metering-dashboard-route>/api/v1/admin/pricing/refresh
# Returns: { "updated": 3, "total": 239, "source": "fetched", "changed": [...] }
```

**To refresh on restart:**
```bash
oc rollout restart deployment/metering-service -n openshift-ingress
```

Pricing fields per model: `input_cost_per_mtok`, `output_cost_per_mtok`,
`cache_read_cost_per_mtok`, `cache_write_cost_per_mtok` (all USD per million tokens).

## Database

PostgreSQL 14+. Schema is auto-migrated on startup:

- `usage_events` — per-request token usage records
- `model_pricing` — per-model cost rates (auto-synced from LiteLLM on startup)

On the dogfood cluster this runs as a 3-instance CloudNativePG cluster
with continuous WAL archiving + daily base backups to IBM COS (PITR
capable). Architecture diagrams, the DSN secret plumbing, backup layers,
and the full 2026-09-16 cutover record (including what went wrong and
how it was verified clean): **[docs/db-design.md](docs/db-design.md)**.
Day-to-day ops: [docs/db-backup.md](docs/db-backup.md).

### LDAP directory sync

`scripts/sync-ldap-cost-centers.sh` is a read-only LDAP directory sync for
`partner_users`. It matches by the stable `rhatUUID` and keeps cost center,
cost-center description, organization, job/geo/office fields, profile display
fields, and manager links (`tags.manager_uuid` + `manager_user_id`) current.
`email`/`kerberos_id` are opt-in because direct tag writes bypass
`partner_user_logins` reconciliation. Empty LDAP values are skipped, unrelated
tags are preserved, only changed rows are written, and `updated_at` is never
touched. It supports local `psql`, Docker,
and an OpenShift Postgres pod (including reading the DSN from a Secret), always
refuses an empty apply unless explicitly allowed, and writes an apply snapshot
plus a generated rollback SQL file. Sensitive LDIF/CSV/SQL output is written
under `~/enmaas-oidc-local` with mode 600.

The production deployment target is a VPN-connected host because IPA is not
reachable from the OpenShift cluster. The 24-hour service deployment,
configuration, and rollback instructions are in
[deploy/ldap-sync/README.md](deploy/ldap-sync/README.md). See `--help` for the
one-off inventory and dogfood examples.

## Related

- [Partner user API](docs/partner-user-api.md) — Atlas/LDAP directory sync,
  partial profile updates, key lifecycle, and method semantics
- [Partner user usage reports](docs/partner-user-usage-api.md) — UUID-based
  batch reports, historical-login attribution, and open-period date handling
- [Praxis](https://github.com/praxis-proxy/praxis) — the proxy this service was built against
- [Praxis AI](https://github.com/praxis-proxy/ai) — AI filters, including the `external_metering` filter that sends events to this service
- [OpenMeter](https://openmeter.io) — production-grade usage metering

## License

Apache License 2.0
