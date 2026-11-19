# Legacy org/table removal (PR A)

This change removes the service's dependence on the legacy organization tables
so a later PR can drop them safely:

- `people`
- `person_identities`
- `user_profiles`
- `quota_requests`, `quota_grants`
- `visibility_grants`, `key_invites`, `org_import_batches`

## Principle

`partner_users` is the single identity source. Manager hierarchy is derived
dynamically from `partner_users.manager_user_id`; a user created before their
manager appears as a root until the manager joins, after which the tree
resolves automatically. Group-based behavior is removed (historical
`usage_events.group_name` retained for reporting only). Quota enforcement and
the approval workflow are disabled in phase one.

## Migration checklist

- [x] Partner-user manager scope resolver (`PartnerManagerScope`).
- [x] Login manager redirect uses partner manager scope.
- [x] Org tree / scope / usage / charts / person APIs read partner users only
      (`partner_org.go` storage, `partner_org.go` handler).
- [x] Remove group filters and group column from dashboard.
- [x] Remove quota request/approval code paths (types, handler, storage, migrations).
- [x] Display names resolved from partner tags (`displayNameExpr`, `GetUserProfile`).
- [x] Usage-report and quota-decision login aliases via `PartnerLoginsForUsername`.
- [x] Model-policy login aliases via `PartnerLoginsForUsername`.
- [x] Remove `syncPartnerUserProfiles` (user_profiles write on partner create/update).
- [x] Remove legacy Admin People table/handlers; admin console shows only
      Partner/Atlas Users + Keys tabs.
- [x] Remove Users & Groups, Platform, Quotas admin tabs.
- [x] Remove dead org/quota/userprofiles handler files + integration tests.
- [x] `storage/org.go` trimmed to audit log + SlugNorm only.
- [x] Cycle-safe manager assignment guard (`UpdatePartnerUserAccess`).
- [x] Make `partner_users.role` canonical for normal admin/super-admin
      authorization; retain `ADMIN_USERS`/`SUPERADMIN_USERS` only as a
      store-outage break-glass fallback.
- [ ] Pre-drop guard: fail startup migration if any legacy reference remains (PR B).

## Tables to drop (PR B)

The follow-up PR drops the legacy tables only after this PR is deployed and the
service no longer references them. The drop migration uses `DROP TABLE IF EXISTS`
and verifies there are no remaining foreign keys before executing.

Tables: `people`, `person_identities`, `user_profiles`, `quota_requests`,
`quota_grants`, `visibility_grants`, `key_invites`, `org_import_batches`.

The `CREATE TABLE IF NOT EXISTS` migrations for these tables were removed in
PR A (they would harmlessly re-create empty tables on fresh databases, but
nothing references them).

## Legacy code retained as dead JS

The following HTML JS functions remain but are never called (their UI elements
were removed). They are harmless and will be cleaned up in PR B:

- `loadPeople`, `renderPeople`, `openAddUserModal` — legacy People table
- `loadQuotaPolicy`, `loadQuotaOverrides` — Quotas tab
- `loadPolicies`, `loadSubscriptions` — Platform tab
- `loadPendingUsers`, `loadGroups`, `saveProfiles` — Users & Groups tab
- Manager page `loadInbox` (quota-requests fetch) — silently catches 404
- Dashboard `renderQuota` — quota banner is CSS-hidden (`display:none!important`)
