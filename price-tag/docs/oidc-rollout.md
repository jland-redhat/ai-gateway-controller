# PriceTag dashboard OIDC rollout

OIDC login is disabled by default. Do not enable it in the production
deployment until the identity provider and the partner directory have been
validated in a non-production environment.

## Required settings

Inject these as deployment configuration; never commit the client secret:

```text
OIDC_ENABLED=true
OIDC_ISSUER_URL=<exact issuer URL from IAM>
OIDC_CLIENT_ID=enmaas-pricetag-dashboard
OIDC_CLIENT_SECRET=<Secret-backed value>
OIDC_REDIRECT_URL=https://dashboard.enmaas.devshift.net/oauth/callback
OIDC_SCOPES=openid profile email groups
OIDC_POST_LOGOUT_REDIRECT_URL=https://dashboard.enmaas.devshift.net/login
SESSION_SECRET=<long random Secret-backed value>
```

`OIDC_POST_LOGOUT_REDIRECT_URL` is optional, must be registered with the
provider, and is only used when the provider advertises an
`end_session_endpoint`. If it is empty the provider shows its own signed-out
page after the RP-initiated logout.

`SESSION_SECRET` signs both the PriceTag session cookie and the OIDC state
cookie. Without it the process generates a random per-start secret, which
logs every user out on restart and breaks the callback on multi-replica
deployments where each pod would hold a different secret. With OIDC enabled,
startup fails if `SESSION_SECRET` is unset. Rotate it by updating the Secret
and rolling all replicas together: every existing session and in-flight OIDC
state cookie becomes invalid, so users sign in again.

The callback URL must exactly match the URL registered with IAM. The issuer
must be the exact `iss` value from the provider discovery document; do not
substitute the dashboard URL or a provider host guessed from the client ID.

## Login behavior

`GET /login` continues to provide API-key login. When OIDC is enabled it also
shows a Red Hat SSO button. The button starts Authorization Code + PKCE at
`/oauth/login`, and the provider returns to `/oauth/callback`.

The callback validates state, nonce, PKCE, issuer, audience, signature, and
token expiry. It requires `sub`, `rhatUUID`, and an email or
`preferred_username`. OIDC groups are copied into the signed session for
display/context only; they do not grant PriceTag roles. The persisted
directory `groups` tag is informational and may lag the IdP; authorization
uses the partner role and the live platform group list, not that tag. The
session is built from the current token, so groups omitted by the IdP do not
inherit stale values from the directory.

The callback resolves the provisioned user by the verified `rhatUUID`, not by
`email` or `preferred_username`; those claims are profile attributes and may
change without breaking login. `email` is not treated as a second identity.
After the profile merge, the callback re-resolves the current partner login for
the verified `rhatUUID` and writes that canonical login to the session.
`email_verified` is not required;
the trust assumption is that the IdP asserted the verified `rhatUUID` and the
partner directory already maps that UUID to an active user.

Logout clears the PriceTag cookie and, when the provider advertises an
`end_session_endpoint`, redirects to RP-initiated logout. If the provider has
no end-session endpoint, logout is local only and the IdP session may still be
alive.

The user must already exist as an active partner user. The partner user's
stable UUID must equal the OIDC `rhatUUID`; login does not auto-provision or
elevate accounts. LDAP synchronization remains responsible for creating and
updating the partner directory, including manager links. One deliberate
exception: an existing directory row may have no login mapping yet (the LDAP
sync creates manager placeholders that way). The first successful SSO login
creates that missing `partner_user_logins` row from the merged email; it never
creates the `partner_users` row itself.

### Session groups are bounded by the session lifetime

The signed session lives for seven days. Its group list is captured from the
IdP at login and is **not** revalidated per request. This section states
exactly what the code enforces, so the guarantee is not overstated.

Self-service API key create and revoke do two separate things:

1. They intersect the session groups with the cluster's live
   `MaaSSubscription` group list. A group removed from the subscription stops
   working immediately, even for an existing session.
2. They do **not** re-check that the user is still a member of that group in
   the IdP or in OpenShift. Membership is whatever the IdP asserted at login.

Consequences, stated plainly:

- If a user is removed from an IdP group but that group is still listed on the
  subscription, the user can keep minting and revoking keys scoped to it until
  the session expires (at most seven days). This is stale authorization, not
  merely display context.
- If the Kubernetes client is unavailable, or the subscription lookup errors or
  returns an empty list, the code falls back to the session groups rather than
  failing closed. That legacy fallback is intentional for availability and is
  the reason a missing platform list is not treated as an authorization check.
- Partner deactivation has the same bound: `partner_users.active` is evaluated
  during login, and `RequireAuth` validates the signed cookie only. A
  deactivated partner keeps an existing session until it expires and then
  cannot log in again.

This is an explicit, accepted risk for this change: revocation is bounded by
the seven-day session lifetime instead of taking effect immediately. Before
session groups are used for any other authorization decision, or before the
session lifetime is extended, per-request membership and `partner_users.active`
revalidation must be added and must fail closed when the lookup is unavailable.

## Safe rollout sequence

1. Obtain the exact IAM discovery/issuer URL and confirm the claims.
2. Deploy the code with `OIDC_ENABLED=false`.
3. Build and test against a non-production PriceTag environment.
4. Create the `/oauth/login` and `/oauth/callback` routes.
5. Add the client secret and a shared `SESSION_SECRET` through the deployment
   secret manager.
6. Enable OIDC only in the approved environment.
7. Verify ordinary user, manager, inactive/unprovisioned user, and admin cases.
8. Keep API-key login during the migration and define rollback as
   `OIDC_ENABLED=false`.

No production cluster mutation is part of this change.

## Local real-SSO testing

The provider must have the exact local callback registered. The production
callback cannot be reused for a laptop callback unless IAM explicitly adds it.
Use a separate development/stage client and secret whenever possible.

An HTTPS tunnel is the safest way to test the real provider while keeping the
application local:

1. Start PriceTag locally on port `8080` with a local database containing the
   test partner user.
2. Start an approved HTTPS tunnel, for example a temporary Cloudflare Tunnel
   or ngrok endpoint, forwarding to `http://127.0.0.1:8080`.
3. Ask IAM to register the tunnel callback exactly, for example:
   `https://<temporary-host>/oauth/callback`.
4. Configure local-only values:

   ```text
   OIDC_ENABLED=true
   OIDC_ISSUER_URL=<exact IAM issuer>
   OIDC_CLIENT_ID=<development client>
   OIDC_CLIENT_SECRET=<local secret file value>
   OIDC_REDIRECT_URL=https://<temporary-host>/oauth/callback
   ```

5. Open `https://<temporary-host>/login` in a browser. The provider returns
   through the tunnel to the local callback.

For a local-only HTTP mock-provider test, `SESSION_COOKIE_SECURE=false` may be
used. Never use that setting on a shared, staged, or production deployment.

The HTTPS tunnel is important because the session and OIDC state cookies are
`Secure`. Do not map the production dashboard hostname to `127.0.0.1` in
`/etc/hosts`, and do not use the production client secret for an unapproved
local callback. The local database must also contain the test user's active
partner record with a matching `rhatUUID`; production data or databases must
not be used for this test.

## Claims and directory storage

The approved IAM registration currently requests these OIDC claims:

```text
sub
rhatUUID
preferred_username / username
email
given_name
family_name
groups
```

PriceTag consumes `sub`, `rhatUUID`, email/username, and groups at the login
boundary. `rhatUUID` must match the active partner user's stable UUID; groups
are carried in the signed session but do not grant PriceTag roles.

After the `rhatUUID` check succeeds, the callback merges every returned profile
claim into the partner user's tags through the normal PATCH path. This keeps
the directory fresh between LDAP syncs and makes the usage dashboard's
country/state and cost-center views work from one source:

```text
email, first_name, last_name, preferred_username, oidc_sub, rhat_uuid,
country, state, region, manager_rhat_uuid, cost_center,
cost_center_description, employee_type, organization, termination_date,
oidc_active, groups
```

`groups` is stored as a string array (the directory also permits `*_groups`
arrays for the local dev personas). Values omitted by the IdP leave the
existing tag untouched. Login never provisions or elevates a partner user from
claims alone, and never authorizes one from claims alone; it may only claim an
existing active directory row by creating its missing login mapping.

For a controlled Stage/dogfood test, `OIDC_CLAIMS_DEBUG=true` renders a
diagnostic page at the callback containing the decoded claim values and no raw
token. Keep this disabled in Production.

`OIDC_TEST_LOGIN=true` shows the mock-IdP persona picker, including personas
used for local demos. It additionally requires a loopback request host
(`localhost`, `127.0.0.1`, or `::1`), so enabling it on a shared stage hostname
does not expose it. Keep both development flags `false` in Production.

Country, state, city, office location, cost center, and manager data are not
promised by every SSO registration, but the Stage registration currently
returns them. They continue to sync from LDAP as the authoritative source; the
login-time merge only fills the partner tags with the values the IdP actually
returned. `manager_user_id` remains the relational reporting field;
`manager_rhat_uuid` is the raw claim value for traceability.
