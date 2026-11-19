# PriceTag Metering Service — GA Readiness

Status as of 2026-09-24: **dogfood/customer-zero quality, not yet GA**.

## What is captured

- The legacy metering-service history is reachable from this repository's
  `main`, including the original Noy and Yossi commits.
- Deployment and OpenShift composition history lives in
  [`redhat-et/pricetag`](https://github.com/redhat-et/pricetag).
- The service includes the CloudEvents ledger, entitlement checks, frozen
  cost accounting, model pricing, org directory, manager views, quotas,
  hourly rollups, parity checks, backups, and read-replica safeguards.
- Dashboard freshness fixes are present: manual refresh bypasses the response
  cache, API responses are `no-store`, and user search includes display names.

Validation currently passing:

```text
go test ./...
go test -race ./...
go vet ./...
```

## GA blockers

### P0 — protect the accounting boundary

1. **Machine-to-machine endpoints need an explicit trust boundary.**
   `/api/v1/events` and the entitlement endpoint are intentionally callable
   without a dashboard session, but the service must be reachable only by the
   gateway or a separately authenticated internal listener. A forged event can
   alter spend; an unauthenticated entitlement call exposes quota state.
   Required controls: internal service/listener split, NetworkPolicy, gateway
   authentication (mTLS or a rotated service credential), rate limits, and
   authorization tests.
   This repository now contains an opt-in `M2M_AUTH_REQUIRED` /
   `M2M_SHARED_SECRET` bearer check; enable it only as part of a coordinated
   gateway rollout that injects the matching `Authorization` header.
2. **Validate and make ingestion idempotent.** This PR adds bounded bodies,
   strict CloudEvents validation, non-negative/range-checked token and status
   checks, and timestamp policy. A unique event identity with safe duplicate
   acknowledgement is implemented by the explicit migration documented in
   [`docs/event-idempotency.md`](event-idempotency.md); it must not be built
   during application startup. Until
   [#7](https://github.com/redhat-et/pricetag-metering/issues/7) is run and
   verified in the target environment, ingestion remains at-least-once:
   replaying an event can double-count the ledger and hourly rollup. This is a
   hard GA prerequisite and `M2M_AUTH_REQUIRED` must remain disabled.
3. **Define failure semantics explicitly.** Fail-open may be acceptable for
   dogfood availability, but GA quota enforcement and event delivery need a
   documented policy, alerting, and an operational kill switch.

### P1 — production operation

4. **Use versioned migrations.** Startup `CREATE/ALTER TABLE` migrations need
   a migration job/tool, locking, rollback guidance, and compatibility checks
   before multi-replica rollout.
5. **Harden the HTTP server.** Add read-header, read, write, idle, and header
   size limits; request cancellation; and readiness that reflects database
   reachability rather than always returning HTTP 200.
6. **Make authentication GA-grade.** Require `SESSION_SECRET` in production,
   support rotation/revocation, and replace API-key-as-dashboard-login with
   the organization's SSO/OIDC path when the product leaves dogfood.
7. **Add operational telemetry.** Export request counts/latency, ingest
   failures, entitlement latency/errors, DB pool health, cache hit rate, rollup
   parity, event lag, and quota-denial counts with alerts and SLOs.
8. **Make pricing auditable.** External LiteLLM pricing refreshes need pinned
   snapshots, effective dates, approval/audit records, and a clear policy for
   historical repricing.
9. **Prove HA and recovery.** Run at least two service replicas with stable
   session/config secrets, test cache behavior across replicas, and perform
   scheduled backup/restore and failover drills.

## Recommended structure before GA

Avoid a broad rewrite now. Keep the current domain packages, but establish
clear boundaries:

- `internal/api/m2m`: authenticated events and entitlement contracts.
- `internal/api/ui`: dashboard, org, quota, and admin APIs.
- `internal/domain`: ledger, pricing, quota, and org services/interfaces.
- `internal/storage/postgres`: repositories and versioned migrations.
- `cmd/metering-service`: startup, listeners, readiness, and lifecycle.

The first structural change should be separate M2M and UI listeners. This
solves the highest-risk deployment problem without coupling the gateway to
dashboard session cookies.

## GA acceptance gates

- Security review signs off the M2M trust boundary and event authenticity.
- Duplicate, forged, malformed, negative, and out-of-order events have tested
  behavior.
- Quota decisions remain correct during rollout, restart, replica skew, and DB
  failover.
- Restore drill completes within the agreed RTO/RPO.
- CI runs tests, race detection, vet, static analysis, secret scanning, image
  scanning, and migration checks on every PR.
- Deployment provenance identifies the exact pushed commit and image digest.
