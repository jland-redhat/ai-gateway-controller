# ADR: Adopt Praxis Policy Engine (PPE) for auth and rate limiting

- Status: Draft
- Date: 2026-11-17
- Owners: MaaS AI Gateway team
- Related: [DESIGN.md](../DESIGN.md), [praxis-dataplane-evolution.md](../praxis-dataplane-evolution.md)

## Summary

Replace the RHCL-injected Kuadrant auth stack (Authorino + Limitador wasm filter,
backed by maas-api) with the Praxis Policy Engine (PPE) embedded in the existing
praxis-extproc deployment. Target: one ExtProc (one Praxis instance per tenant
gateway) owns the whole request path — pre-auth payload preparation, authn/authz,
rate limiting, and payload processing — with policy supplied as a rendered
ConfigMap instead of Kuadrant AuthPolicy/RateLimitPolicy CRs.

**Version note:** there is no PPE 0.0.8 release. As of 2026-11-17 the latest
release is **v0.6.0** (2026-11-06, marked pre-release; API explicitly unstable
between minor versions). This ADR targets v0.6.0 capabilities.

## Context — what we run today

Gateway mode (current, "Praxis for 3.6" stage per
[praxis-dataplane-evolution.md](../praxis-dataplane-evolution.md)):

```
Client → Envoy (maas-default-gateway)
  → ext_proc ipp-pre   (payload-pre-processing: BBR chain, model extraction)
  → RHCL Kuadrant wasm (Authorino + Limitador → maas-api:
                        API-key validate, subscription select, rate limit,
                        inject x-maas-*, strip Authorization)
  → ext_proc ipp       (payload-processing: header guard, request_id,
                        stream_usage_inject, translation)
  → InferencePool EPP ext_proc (optional)
  → router → KServe Service
```

Ownership split:

| Concern | Owner today |
| --- | --- |
| EnvoyFilter with four RHCL wasm anchor variants, ExtProc clusters, route patches | ai-gateway-controller ([envoy-filter.yaml](../../config/manifests/praxis-extproc/overlays/odh/envoy-filter.yaml)) |
| praxis-extproc Deployments + `payload-processing-plugins` ConfigMap (pre-extproc.yaml / extproc.yaml filter chains) | ai-gateway-controller (`pkg/tenant/extproc.go`) |
| MaaS CR → Kuadrant CR translation (AuthPolicy, RateLimitPolicy, Limitador limits, API keys, subscriptions) | **maas-controller** (models-as-a-service repo) |
| maas-api (runtime auth service called by Authorino) | maas-controller |
| Consumer-portal AuthPolicies (OIDC session/anonymous) | ai-gateway-controller (`pkg/consumerportal`) |

PPE ([praxis-proxy/policy](https://github.com/praxis-proxy/policy), v0.6.0) is a
Rust **library**, not a server. A host process embeds it, loads one static YAML
policy document (`engine_settings`, `global`, `plugins`, `groups`, `routes`,
`secrets`), and fires hooks (`identity.resolve`, `ratelimit.pre/post_identity`,
`http.request/response`, CMF LLM/tool hooks). It has no ext_proc code of its
own — the host is the Praxis proxy, which is what our praxis-extproc image
already runs. There is no admin API, no Kubernetes watching, no xDS; dynamic
policy is entirely the host's job.

## 1. Gap analysis: PPE v0.6.0 vs the RHCL/Kuadrant stack

### What PPE covers

| Need | PPE v0.6.0 answer | Verdict |
| --- | --- | --- |
| API-key authn | `identity/api-key`: hash-indexed file or **HTTP directory** (bounded cache), header/cookie/query extraction, deny codes | ✅ maas-api can serve as the HTTP key directory, or keys render to a file |
| JWT authn | `identity/jwt`: JWKS, issuers/audiences, claim mapping presets, DPoP (RFC 9449) | ✅ |
| Policy/subscription checks | APL DSL (`require`/`deny`/`allow`/`run`), pluggable Cedar / CEL / OPA PDPs, PDP decision cache | ✅ subscription/entitlement rules expressible in CEL/OPA |
| Rate limiting | `ratelimit.pre/post_identity` hooks + experimental in-process `ratelimit/limitador` plugin (429 with limit/remaining/retry_after) | ⚠️ see gaps |
| Token quota (LLM budgets) | experimental `quota` plugin (Limitador-backed per-principal budget, pre-call check + post-call usage report) | ⚠️ experimental |
| Upstream credential swap | `delegator/oauth` (RFC 8693 token exchange), `delegator/vault` | ✅ replaces part of Authorino's credential injection |
| Identity → header injection | `assertions:` block maps `subject.id`, `claim.*` to upstream request headers | ✅ x-maas-* equivalent |
| Response headers/status policy | `http.response` hook, strip/replace upstream headers | ✅ |
| Audit | Per-decision audit events, OCSF sink with DSSE + hash chaining | ✅ (better than today) |
| Kuadrant parity | Upstream proposal `00133_kuadrant-authpolicy-attribute-mapping.md` maps every Kuadrant well-known attribute; `authpolicy-transpiler` (Kuadrant AuthPolicy → PPE YAML) exists in praxis-proxy/demos | ✅ strong signal this migration is intended |

### Gaps (blockers or open work)

1. **Rate-limit state is per-instance** — moved to MAJOR GAPS below.
2. **No dynamic policy surface.** Static YAML only. No admin API, no k8s
   watching; secrets mutation on reload is refused (restart required). Today
   maas-controller reconciles AuthPolicies continuously. Needs: host-side
   config hot-reload (Praxis already reloads server config) driven by a
   controller that re-renders the ConfigMap.
3. **HTTP request bodies are not a first-class APL input** on `http:` routes
   (LLM/tool entity bodies are). Body work stays in Praxis filter chains —
   fine for us (BBR chain already lives there), but any policy keyed on raw
   HTTP bodies must become a filter or an LLM-entity route.
4. **Response bodies are not modeled at all** in PPE (headers/status only).
   Our response-body work is payload processing (stream_usage_inject, SSE) and
   stays in the ext_proc filter chain — not a blocker, just a boundary to
   respect.
5. **No OIDC Authorization Code / browser flows.** Identity is JWT or API key.
   This affects only the consumer portal (oauth2-proxy session + anonymous
   AuthPolicies). It does **not** affect MaaS external OIDC (overlay 0032):
   that path is bearer-JWT validation only (JWKS fetch, issuer/audience,
   `azp == clientId`), which `identity/jwt` covers — connecting an IDP for API
   consumers keeps working. Proposal: leave consumer portal on Kuadrant for now
   (out of scope), or model its JWT checks in `identity/jwt` later.
6. **maas-api coupling.** Today Authorino calls maas-api for key validation
   *and* subscription selection. PPE splits these: keys via
   `identity/api-key` HTTP directory; subscription/entitlement logic must be
   re-expressed (CEL/OPA against directory data, or maas-api kept as a
   metadata/PIP endpoint — external metadata PIP is tracked upstream in
   issue #184, foundation landed 0.6.0).
7. **PPE `http:` routes cannot match on headers** — moved to MAJOR GAPS below.
8. **Maturity.** All releases pre-release; "public API will move between minor
   versions". APL has no `now()`/arithmetic (no rolling freshness windows).
   Mitigation: pin exact PPE/praxis-extproc builds (we already do via image
   digest).

### Parity note: current direct-JWT path lacks an audience check (PPE is stricter)

The consumer-portal `direct-jwt` rule
([authpolicies.yaml](../../config/manifests/consumer-portal/base/authpolicies.yaml))
configures only `issuerUrl` and `ttl` — no `audience`. Authorino therefore
accepts **any** token signed by the trusted issuer, regardless of which client
the token was minted for; the in-file comment on the `client-bound` rule
acknowledges this ("the direct-jwt rule authenticates any signed token").
The `client-bound` azp check does not close it: its `patterns` list is an OR,
so the `iss == issuer` pattern alone satisfies the rule and a wrong-`azp`
token still passes. PPE's `identity/jwt` makes `audiences` part of each
`trusted_issuers` entry (fail-closed), so migrating this surface to PPE
*fixes* the audience-binding hole rather than regressing it. Not a PPE gap;
recorded here so the parity test suite asserts `aud`/`azp` rejection under
both engines and the current rule is not reproduced verbatim in any
hand-written PPE policy. Revocation remains absent from both stacks — the
`ttl: 300` window is the only mitigation on either side.

### Identity capability matrix: Authorino vs PPE v0.6.0

Authorino's identity section is broader than OIDC. Per Authorino docs vs the
PPE `builtins` catalog (`identity/jwt`, `identity/api-key` only):

| Capability | Authorino | PPE v0.6.0 | Migration impact |
| --- | --- | --- | --- |
| JWT/OIDC validation (JWKS, issuer) | ✅ `oidc` identity | ✅ `identity/jwt` | none |
| Audience check | ⚠️ optional (`audiences` may be omitted) | ⚠️ `audiences` on every `trusted_issuers` entry — **examples always set it; docs do not state it is optional. Needs verification.** | **potential blocker** — see below |
| Credential location | authorizationHeader, cookie, customHeader, queryString | header, cookie, query_param (needs `read_query` capability) | none — equivalent |
| Claim mapping | JSONPath-ish per-attribute overrides | claim-mapper presets / inline `claim_map`, fixed destination set | parity check on destination fields |
| OAuth2 token introspection (opaque tokens) | ✅ `oauth2` identity (introspection endpoint + credentials) | ❌ not present | only matters if a tenant sends opaque access tokens; our surfaces are JWT or API key |
| Kubernetes TokenReview | ✅ `kubernetes` identity | ❌ | **MAJOR GAP 1** (overlay 0032 chain) |
| mTLS / HMAC identities | ✅ | ❌ | not used by MaaS surfaces today |
| Anonymous identity | ✅ `anonymous: {}` | ❌ no identity plugin, but expressible: no subject resolves, policy uses `require(authenticated)` or `allow` | semantics differ — consumer portal session policy needs rework if moved |
| DPoP (RFC 9449) | ❌ | ✅ `identity/jwt` (header credential + `read_headers` required) | PPE ahead |
| Multi-identity priority chains | ✅ `priority:` with fall-through | ✅ host composes resolvers; overlay 0032 ordering must be preserved host-side | verify composition order matches first-match-wins |
| JWKS refresh/TTL | ✅ `ttl` (default 0 = no re-fetch) | not documented | verify JWKS rotation behavior before GA |

**Audience migration risk (needs verification).** Authorino treats `audiences`
as optional — our consumer-portal `direct-jwt` rule omits it entirely, and
external tenants configuring OIDC via overlay 0032 may do the same. If PPE
requires `audiences` on every trusted-issuer entry (or rejects tokens whose
`aud` does not match), two failure modes appear on migration: (a) the
transpiled policy fails config load for tenants that never configured an
audience, and (b) tokens that carry no `aud` claim (or a different audience
value) stop passing where they passed under Authorino. Neither is fixable
host-side. Required: confirm with the Praxis team whether `audiences` is
optional in `identity/jwt`; if required, the `authpolicy-transpiler` must
derive an audience default and the migration guide must call out the
behavior change. This intentionally tightens security (see parity note
above) but is a breaking change for tokens minted without `aud`.

## MAJOR GAPS (required changes in PPE for 3.7)

Gaps that need implementation work in PPE itself — not workarounds we can do
host-side. These are the asks for the Praxis team; without them the
corresponding 3.6 authentication surfaces cannot move to PPE.

1. **Kubernetes TokenReview identity is missing.** Today Authorino's
   `kubernetes` (TokenReview) identity validates OpenShift ServiceAccount
   bearer tokens as the third method in the overlay 0032 priority chain
   (API key, then OIDC JWT, then TokenReview); maas-controller renders it
   into the tenant AuthPolicy. PPE v0.6.0 has only `identity/api-key` and
   `identity/jwt` — there is no way to validate an SA token. Required:
   an `identity/tokenreview` (kube TokenReview) plugin in PPE that
   authenticates the bearer token against the Kubernetes API and maps
   user/groups into the same identity attributes APL rules consume, and
   that composes with the other two identities in the overlay 0032
   priority order (first match wins, no fall-through error).
   Implementation constraint: the single post-auth ExtProc ServiceAccount
   deliberately holds no API permissions
   ([extproc.go](../../pkg/tenant/extproc.go) metrics_auth comment) — the
   TokenReview client must run under a separate narrowly-scoped identity
   (TokenReview-only RBAC), not by broadening the existing SA.
   The minting side needs no change: SA tokens are minted by OpenShift
   itself, and the MaaS API-key minting flow (`POST /v1/api-keys`,
   revocation via maas-api) survives as the PPE HTTP key directory.
   Without this plugin, kube-token clients stay on the V2/Kuadrant path.
   Upstream tracking: **already filed** —
   [praxis-proxy/policy#213](https://github.com/praxis-proxy/policy/issues/213)
   ("Support Kubernetes TokenReview for authentication", open); companion
   SubjectAccessReview ask in
   [#214](https://github.com/praxis-proxy/policy/issues/214); also on the
   AuthPolicy-subset checklist
   ([#195](https://github.com/praxis-proxy/policy/issues/195)).

2. **`http:` routes cannot match on headers, so route-scoped (per-model)
   rate-limit policies have no PPE equivalent.** Today per-model token rate
   limits work by attaching a Kuadrant TokenRateLimitPolicy to a specific
   HTTPRoute matched on the `X-Gateway-Model-Name` header: the pre-auth
   ExtProc chain extracts the model name from the request body into that
   header, and Envoy re-resolves the route on the header match, so the
   TRLP for that model applies ([reconciler.go](../../pkg/controller/reconciler.go)
   renders the header-match rules). *Conceptually the model-name-to-route
   resolution belongs to model serving and should stay separate from rate
   limiting* — but on PPE that separation is currently not expressible:
   per the upstream routing docs
   ([http-routing.md](https://github.com/praxis-proxy/policy/blob/main/docs/content/http-routing.md)),
   an `http:` route map takes exactly `path`, `path_prefix`, or `method`
   and "anything other than `path`, `path_prefix`, and `method` in that map
   is a load error" — there is not even a header (or query) matching, so we cannot
   build an equivalent policy keyed on the model header. Required: header
   matching on `http:` routes (or an equivalent scoping mechanism for
   `ratelimit`/`quota` rules keyed on request headers). Fallbacks if PPE
   cannot deliver: key limits on identity + the parsed body `model` on
   LLM-entity routes, or keep per-model token rate limits on Kuadrant TRLP
   during migration. Upstream tracking: **no dedicated issue found**. The
   HTTP route selector shipped without header matching
   ([#40](https://github.com/praxis-proxy/policy/issues/40), closed), and the
   broader Kuadrant-parity epics
   ([#100](https://github.com/praxis-proxy/policy/issues/100),
   [#195](https://github.com/praxis-proxy/policy/issues/195)) do not call it
   out specifically. Needs a new issue.

3. **Rate-limit counters cannot be shared and cannot reuse the existing
   Redis/Limitador.** Today the RHCL Limitador wasm filter calls the
   Limitador service, which keeps counters in Redis — global across
   replicas, durable across restarts. PPE v0.6.0 has no equivalent: per the
   upstream rate-limiting docs, the `ratelimit/limitador` plugin "counts
   requests with an in-process Limitador limiter", "counters are local to
   each plugin instance", "replicas do not share counters", and there is no
   backend/redis/valkey/remote-endpoint option — so pointing PPE at the
   exact same Redis instance (or the running Limitador service) is **not
   possible** for request rate limits. Required: a remote/shared counter
   backend for the rate-limit path — either a `ratelimit` plugin variant
   that is a Limitador gRPC client (pointing at the existing Limitador
   deployment and its Redis), or a Valkey/Redis-backed counter store with
   the same semantics. Migration plan for the counters themselves:
   * **Request rate limits** (windowed counters): no data migration
     needed — cut a tenant over at a window boundary; worst case one
     window of over-admission per counter. The blocker is the missing
     shared backend, not counter data.
   * **Token quotas / budgets**: the experimental `quota/limitador`
     plugin *does* talk to a remote Limitador service whose counters
     "survive restarts and be shared by replicas". If its Limitador
     client API matches the RHCL-deployed Limitador version, point it at
     the same Limitador service and Redis — budget balances carry over
     across the V2→PPE cutover with zero migration. Needs verification:
     Limitador API version compatibility and whether `quota/limitador`
     covers non-LLM (`http:`) traffic or only `cmf.llm_input/output`
     hooks. Until the shared rate-limit backend ships, multi-replica
     gateways either stay single-replica for PPE tenants or keep
     request rate limits on Kuadrant TRLP.
   Upstream tracking: **partially covered.** The enabling work is filed:
   [#127](https://github.com/praxis-proxy/policy/issues/127) (host-managed
   backend stores injected into PPE plugins — explicitly motivated by the
   Limitador-in-PPE Redis/Valkey case) and
   [#203](https://github.com/praxis-proxy/policy/issues/203) (align
   Limitador's Redis client with PPE's, framed as "relevant when Redis
   storage is enabled"), both under the
   [#143](https://github.com/praxis-proxy/policy/issues/143) epic
   (Limitador integration); graduation tracked in
   [#308](https://github.com/praxis-proxy/policy/issues/308). **Not filed:**
   a `ratelimit` plugin variant that is a Limitador *gRPC client* pointing
   at the existing standalone Limitador deployment — #127/#203 assume an
   embedded store, not the remote service. If we want the "reuse the running
   Limitador as-is" option, that needs a new issue.

4. **`identity/jwt` audience semantics may break tenants that configured
   Authorino OIDC without `audiences`.** Authorino makes the audience check
   optional; every documented PPE `trusted_issuers` example sets `audiences`,
   and the docs do not state it can be omitted. If it is required (or if
   tokens without a matching `aud` claim are rejected), tenants migrating
   from AuthPolicies that omit `audiences` get either a config-load failure
   or suddenly-rejected tokens. Needs verification against PPE source; if
   confirmed, required: an optional-audiences mode (or a documented
   derivation rule the `authpolicy-transpiler` can apply), plus a migration
   note that the check is intentionally stricter. See the identity capability
   matrix and parity note in section 1.
   Upstream tracking: **no dedicated issue found.** Adjacent open issues:
   [#290](https://github.com/praxis-proxy/policy/issues/290) (OIDC
   Discovery for JWT auth) and
   [#129](https://github.com/praxis-proxy/policy/issues/129) (browser OIDC
   flows) — neither addresses audience optionality. Needs a new issue after
   source verification.

## 2. Proposed architecture

### Target shape

```
Client → Envoy (maas-default-gateway)
  → ext_proc payload-processing (single praxis instance per tenant)
      Praxis filter chain:
        1. pre-auth BBR chain          (today's pre-extproc.yaml, unchanged)
        2. PPE: identity.resolve       (API-key via maas-api HTTP directory)
        3. PPE: ratelimit hooks        (shared-store Limitador plugin)
        4. PPE: authorization          (APL/CEL subscription rules,
                                        assertions → x-maas-* headers,
                                        Authorization strip)
        5. payload chain               (today's extproc.yaml, unchanged)
  → InferencePool EPP ext_proc (optional, unchanged)
  → router → KServe Service
```

- **One ExtProc, one Praxis instance.** The two Deployments
  (`payload-pre-processing`, `payload-processing`) collapse into one. The
  pre-auth/post-auth split becomes ordering *inside* the Praxis filter chain +
  PPE hook phases (`ratelimit.pre_identity` fires before identity; authz after).
  Envoy filter chain simplifies to a single `ext_proc` filter — no more RHCL
  wasm anchors (four Istio-version variants die), no priority-10 ordering
  dance against Kuadrant's EnvoyFilter.
- **Fail-closed stays**: `failure_mode_allow: false`, request body BUFFERED,
  response body STREAMED as today.
- **maas-api fate (open question):** either (a) reduced to an API-key +
  subscription-data HTTP directory that PPE reads, or (b) eventually absorbed
  into rendered policy + a directory endpoint. Recommend (a) first — smallest
  step, keeps runtime data path identical.

### Does the Kuadrant translation become a ConfigMap? Yes.

PPE has no CRDs. The MaaSSubscription/AuthPolicy/APIKey → Kuadrant
AuthPolicy/RateLimitPolicy translation in maas-controller must become a
**PPE policy YAML rendered into a ConfigMap**, exactly like
`payload-processing-plugins` already works for filter chains:

- One ConfigMap per tenant, e.g. `payload-processing-policy`, keys:
  `extproc.yaml` (server + filter chains, as today) and `policy.yaml`
  (PPE document: identity directories, routes, APL/CEDAR/CEL, rate limits).
- Controller stamps config sha256 on the Deployment (existing
  `extproc-config-sha256` mechanism) and rolls pods on change — that is the
  "dynamic policy" story until PPE grows hot-reload.
- **Division of labor question (decision needed):** who renders `policy.yaml`?
  Options:
  1. maas-controller renders it (owns the MaaS CR semantics, keeps today's
     ownership split) and ai-gateway-controller only mounts/rolls it;
  2. ai-gateway-controller renders it from data maas-controller publishes
     (inverts the dependency);
  3. reuse the upstream `authpolicy-transpiler` as a library so maas-controller
     keeps emitting Kuadrant-shaped intermediate objects and a transpile step
     converts to PPE YAML.
  Recommend **option 3** for migration safety: translation logic stays
  reviewable and a diff of transpiled output gives us the PPE-vs-Kuadrant
  parity audit for free.

### Migration / coexistence

Per-tenant, annotation-gated, same pattern as the IPP→praxis payload swap
(`maas.opendatahub.io/payload-processing-type`):

- New annotation value, e.g. `policy-engine: ppe` (default stays `rhcl`).
- EnvoyFilter rendering picks either the single-filter PPE chain or the
  current ipp-pre/wasm/ipp chain per tenant gateway.
- Rollout order: dev tenant → EA → GA, mirroring RHOAIENG-98846 gates.
- Rollback = flip annotation back; Kuadrant/RHCL stays installed.

## 3. Work breakdown by repository

### praxis-proxy/policy (upstream, contribute)

- Shared-counter rate-limit backend (remote Limitador or Valkey) for the
  `ratelimit/limitador` plugin; graduate it out of `experimental-`.
- Stable reload semantics for policy (today: engine-swap; secrets still force
  restart).
- External metadata PIP (issue #184) if maas-api stays a live data source.
- Graduate `quota` plugin if token budgets move to PPE.
- Keep `00133` Kuadrant attribute mapping current.
- Header matching on `http:` routes (or equivalent model-scoped rate-limit
  keying), so per-model token limits survive the loss of HTTPRoute-attached
  TokenRateLimitPolicy.

### praxis-proxy/praxis + opendatahub-io/praxis-extproc

- Single-instance ext_proc mode: merge pre-auth + post-auth phases in one
  filter chain; fire PPE `ratelimit.pre_identity`, `identity.resolve`, authz,
  `ratelimit.post_identity` at the right chain positions.
- Embed PPE with `builtins` + needed features (`api-key`, `jwt`, PDP of
  choice); wire an `HttpTransport` for the maas-api directory fetch.
- Load `policy.yaml` from the mounted ConfigMap alongside `extproc.yaml`.
- Verify fail-closed behavior, buffered/streamed body handling, SSE.

### models-as-a-service / maas-controller

- New renderer: MaaSSubscription/AuthPolicy/APIKey → PPE policy YAML into the
  per-tenant ConfigMap (via authpolicy-transpiler if option 3 chosen).
- Keep Kuadrant renderer for `policy-engine: rhcl` tenants (coexistence).
- Reduce maas-api to API-key/subscription HTTP directory (phase 1); decide
  long-term home of subscription-selection logic.
- Rate-limit descriptors → PPE rate-limit config; define what the shared
  counter store is.

### ai-gateway-controller (this repo)

- EnvoyFilter variant: single `ext_proc` filter, drop RHCL wasm anchors and
  the Kuadrant priority ordering; keep maas-api-route ExtProcPerRoute disables
  (directory calls must not loop through the gateway — actually directory
  calls go pod→maas-api directly via cluster; confirm no recursion).
- Collapse two Deployments into one; extend ConfigMap mechanism to
  `policy.yaml` + sha256 roll.
- `policy-engine` annotation plumbing in `pkg/tenant` alongside
  `payload-processing-type`; managed=false / force-migration gates honored.
- Consumer portal: keep on Kuadrant AuthPolicies (out of scope), document.

### e2e / test

- `POLICY_ENGINE=ppe` path in `test/e2e/scripts/deploy-platform.sh` and the
  prow script (skip RHCL install, install nothing or only the shared RL store).
- Parity test suite: for a matrix of MaaS CRs, assert same outcomes
  (401/403/429 semantics, x-maas-* headers, Authorization strip) under rhcl
  and ppe — the transpiler diff doubles as the config-level check.

## Decision needed

1. Who renders PPE policy YAML (option 1/2/3 above) — recommend 3.
2. Rate-limit shared store: remote Limitador vs Valkey vs single-replica
   acceptance for EA.
3. maas-api long-term: directory-only, absorbed, or stays as PIP.
4. Timeline coupling to the "Standard mode" (post-3.6, Praxis as the gateway)
   draft — PPE adoption in Gateway mode now is a stepping stone; confirm we
   are not blocked on that draft settling.

## POC: early-praxis-policy-engine-ppe (branch `early-praxis-policy-engine-ppe`)

Goal: prove the standard MaaS validation flow
([validation guide](https://opendatahub-io.github.io/models-as-a-service/latest/install/validation/))
passes against a PPE-backed tenant gateway, before any of the MAJOR GAPS are
fixed upstream.

**Host wiring is already solved upstream.** praxis-extproc main ships a
`policy-engine` feature: the Praxis Policy Engine is exposed as a `policy`
*filter* in the ext_proc filter chain (praxis-ai v0.9.0 → praxis-filter
v0.7.4, `crates/filter/src/builtins/http/security/policy/` in
praxis-proxy/praxis). Filter config is flat:

```yaml
filters:
  - filter: policy
    config_path: /etc/praxis/policy.yaml   # the PPE document
```

So the POC needs **no upstream Rust changes**: load a hand-written PPE
policy document through the existing ConfigMap mechanism and insert one
`policy` filter entry in the rendered chain.

### POC scope (what is built)

- One dev tenant, annotation-gated (e.g. `policy-engine: ppe`, default
  stays `rhcl`), reusing the `payload-processing-type` annotation pattern.
- Hand-written `policy.yaml` in the per-tenant ConfigMap (no
  maas-controller renderer, no authpolicy-transpiler — deferred):
  - `identity/api-key` with maas-api as the HTTP key directory
    (`trusted_private_endpoints` for the in-cluster maas-api host — the
    policy filter is private-address-restricted by default);
  - subscription/entitlement checks in CEL/OPA keyed on directory data;
  - `assertions` → `x-maas-*` upstream headers, Authorization strip;
  - `ratelimit/limitador` in-process, keyed on subscription (same keying
    as today: the whole subscription — all API keys and owners bound to
    it — shares one counter; this is current-stack behavior, not a POC
    approximation);
  - `allow`-through route for `/maas-api` paths (see carve-out below).
- EnvoyFilter variant for PPE tenants: drop the RHCL wasm anchors, keep
  the existing ext_proc chain, keep the maas-api-route ExtProcPerRoute
  disables.
- Deployment: single replica (in-process rate-limit counters are
  correct but not shared).

### POC carve-outs (what is deliberately NOT built)

| Major gap | POC dodge |
| --- | --- |
| 1. TokenReview | `/maas-api/*` routes bypass PPE policy (`allow` route +
  existing ExtProcPerRoute disable). maas-api keeps validating OpenShift
  tokens itself, so API-key **minting is real, unchanged** — not faked.
  Only SA-token *inference* auth is missing, and the validation flow
  never exercises it. |
| 2. header matching | Per-model TRLP absent. The validation flow's rate
  limit is subscription-keyed, not per-model — not exercised. |
| 3. shared counters | Single-replica gateway; in-process limitador. |
| 4. audiences | Not exercised — the flow uses API keys, no OIDC. |

Verification item: confirm maas-api self-validates the bearer token on
`POST /v1/api-keys` today (it is the service Authorino calls and holds
k8s API access), so the carve-out needs no extra auth on that route.

### Validation criteria

Run the manual validation steps 1–7 (endpoint, API-key mint, model list,
inference, 401 without token, 429 after ~4 requests) plus
`scripts/validate-deployment.sh` against the PPE tenant. Parity
expectations: 401/403/429 semantics and `x-maas-*` headers match the
rhcl tenant.

### POC results: what does NOT work, and why (honest ledger)

The validation flow passes, but only because of deliberate carve-outs.
Everything below is **not enforced by PPE** in the POC. Each entry says
why and links upstream tracking where it exists.

1. **MaaSAuthPolicy (model access control) is not enforced by PPE at
   all.** The PPE document only does `require(authenticated)` on
   inference paths — no per-model, per-user access rules. Enforcement
   still happens through the legacy path: maas-controller renders
   MaaSAuthPolicy into Kuadrant AuthPolicies and the RHCL wasm (left in
   the chain, shadow mode) enforces it. Translating MaaSAuthPolicy
   semantics into PPE policy is the deferred renderer/transpiler work
   (gap 6 above). Upstream: the AuthPolicy-capability epics
   [#100](https://github.com/praxis-proxy/policy/issues/100) and
   [#195](https://github.com/praxis-proxy/policy/issues/195); the
   `authpolicy-transpiler` exists only in praxis-proxy/demos.
2. **MaaSSubscription feeds nothing in PPE.** Two touchpoints, neither
   in the policy: key minting binds a key to a subscription inside
   maas-api's DB, and `GET /v1/models` scoping is maas-api itself.
   Entitlement checks in PPE (can this subject use this model/quota?)
   are absent. Upstream: the external-metadata PIP work is the
   enabling surface — spike closed
   ([#215](https://github.com/praxis-proxy/policy/issues/215)),
   implementation epic
   [#184](https://github.com/praxis-proxy/policy/issues/184) with the
   `metadata-http` issue series
   [#246](https://github.com/praxis-proxy/policy/issues/246)–[#261](https://github.com/praxis-proxy/policy/issues/261).
3. **The rate limit is per-user, not per-subscription — a deviation
   from the RHCL stack.** The original binding
   (`subscription: claim.subscription`, reading the subscription field
   of maas-api's validate response) failed with
   `ratelimit.missing_attribute`: unconsumed api-key record fields do
   not surface as `claim.*` attributes for limitador bindings. Rekeyed
   to `subject.id`. RHCL Limitador descriptors key on the subscription
   (all keys and owners of one subscription share a counter); the POC's
   PPE counter is per-user. No upstream issue yet — the
   record-claims-to-binding attribute gap needs one.
4. **Request rate limiting only — NO token rate limiting.** The POC's
   429 is a windowed request counter (`ratelimit/limitador`,
   experimental). The product's token budgets (Kuadrant
   TokenRateLimitPolicy, per-model token metering) have **no PPE
   equivalent in the POC**: per-model keying needs header matching on
   `http:` routes (MAJOR GAP 2, no upstream issue) and the token-quota
   path is the experimental `quota` plugin. Upstream:
   [#308](https://github.com/praxis-proxy/policy/issues/308) (graduate
   ratelimit/limitador for token-based limiting),
   [#200](https://github.com/praxis-proxy/policy/issues/200) (Kuadrant
   TRLP → Praxis TRL gap investigation),
   [#336](https://github.com/praxis-proxy/policy/issues/336)
   (reservation-based admission), and the tokenomics enhancement
   proposal 00121 in praxis-proxy/enhancements.
5. **Counters are per-replica** (MAJOR GAP 3): single-replica gateway
   required; multi-replica would over-admit. Upstream: #127 / #203 /
   #143 as tracked in MAJOR GAPS.
6. **TokenReview missing** (MAJOR GAP 1): SA-token inference auth
   impossible on PPE. Upstream: #213.
7. **OIDC/JWT path never exercised**: the flow uses API keys only; the
   audience-optionality migration risk (MAJOR GAP 4) is untested.
   No upstream issue.
8. **RHCL still runs in shadow.** Both engines enforce; a PPE-only run
   requires deleting the tenant's Kuadrant AuthPolicy/RateLimitPolicy
   CRs after scaling maas-controller to 0 (POC-only; it recreates them).
   The 401/403/429 outcomes observed are produced by PPE (its deny
   reasons surface), but model-access 403s in shadow mode could come
   from either engine.
9. **praxis 0.8.0 compatibility shims live in our render path**: the
   vendored BBR chain fails 0.8 validation without an explicit
   `max_body_bytes` on the llmisvc resolver, and the policy filter's
   nested pipeline trips the unbounded-StreamBuffer check unless
   `insecure_options.allow_unbounded_body` is set (clamped to the
   server cap). Both are patchable upstream; no issues filed. Related:
   the policy filter warns it builds without the runtime's sub-request
   connector and uses a second connection pool — also unfiled.
10. **FIPS builds cannot carry the policy filter at all** (pure-Rust
    crypto in PPE's plugins): the product image excludes
    `policy-engine` by design. A FIPS-compatible PPE build is a
    productization blocker with no upstream issue yet.
11. **`x-maas-*` upstream headers are not injected by PPE.** The
    post-auth header guard strips caller-supplied `x-maas-*`, but the
    identity → header injection PPE would need (assertions →
    x-maas-username, x-maas-subscription, …) is not rendered in the
    POC policy; today those headers come from the RHCL wasm in shadow
    mode. PPE's `assertions:` block supports it; the renderer work is
    the same deferred item as #1.

### Repos touched

- **ai-gateway-controller** (branch `early-praxis-policy-engine-ppe`):
  annotation plumbing, EnvoyFilter variant, `policy.yaml` ConfigMap
  rendering, hand-written tenant policy.
- **models-as-a-service**: install only (full platform install for the
  cluster); no code changes required for the POC.
- **praxis-extproc / praxis-proxy**: no changes required — `policy`
  filter ships in the default feature set. Image must be a build
  carrying `policy-engine` (check the pinned digest).
