# ai-gateway-controller E2E — dynamic MaaS checkout

Tests and deploy tooling come from [models-as-a-service](https://github.com/opendatahub-io/models-as-a-service) at runtime (`test/e2e/scripts/fetch-maas-e2e.sh`), pinned by `test/maas-e2e.lock`.

```bash
# Refresh pin to latest main:
MAAS_UPDATE_LOCK=true bash test/e2e/scripts/fetch-maas-e2e.sh

AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
  ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh
```

---

## Excluded tests (not in `run_e2e_tests.sh` allowlist)

Re-enable when the **Requirement to re-introduce** is met. Update the allowlist in `test/e2e/scripts/run_e2e_tests.sh`.

| Test module | Why excluded | Requirement to re-introduce |
|-------------|--------------|------------------------------|
| `test_external_models.py` | ai-gateway-controller has no ExternalModel reconciler yet | Implement ExternalModel/ExternalProvider in aigc; Konflux egress fixtures for simulator endpoints |
| `test_external_oidc.py` | External OIDC gateway path not in aigc CI scope | Partner/OIDC gateway deployments + Keycloak fixtures in prow; `EXTERNAL_OIDC=true` path |
| `test_x_api_key_auth.py` | Depends on IPP ExternalModel (`apiFormat=messages`) identity source | Praxis/IPP ExternalModel wiring in aigc; gateway AuthPolicy `api-keys-x-api-key` source |
| `test_networkpolicy.py` | NetworkPolicy assertions assume full MaaS networkpolicy bundle | Confirm aigc deploy applies same NP manifests; no CNI conflicts on Konflux clusters |
| `test_model_identity_conflict.py` | Cross-CR identity conflict scenarios need full controller surface | Validate against aigc + praxis dataplane; may need MaaS upstream fixes |
| `test_tenant_auto_resolve.py` | Tenant auto-resolve depends on MaaS controller features not validated on aigc yet | Confirm `maas-controller` + aigc AITenant wiring; run on dedicated cluster |
| `test_crd_watch_resilience.py` | Deletes KServe CRD / restarts controller — destructive serial test | Safe on ephemeral CI only; add serial pass gating + KServe module present |
| `test_authpolicy_generation_stability.py` | Long stability window; sensitive to parallel AuthPolicy churn | Run serial-only or increase isolation; confirm no aigc-specific AuthPolicy regressions |

---

## Upstream MaaS changes needed (or carry aigc patches)

These were fixed in the **vendored** branch (`ci/maas-e2e-konflux-group-test`) and are **not** in upstream MaaS `main` yet. Merge upstream or keep `patch-maas-deploy-for-aigc.sh` / aigc-only scripts.

| Area | Issue | Fix (upstream or aigc) |
|------|--------|-------------------------|
| **Praxis default dataplane** | `test_per_tenant_ipp_isolation` expects Go IPP log markers (`handlers/server.go`, `x-request-id`) | Upstream: skip log check when deployment is `odh-praxis-extproc` (Rust quiet at INFO); rely on HTTP 200 |
| **Praxis image / BBR** | Default tenant uses praxis; needs `llmisvc_model_provider_resolver` (praxis-proxy/ai#699) | Published image pin in prow/Tekton until Konflux builds praxis with #699; promote to `odh-praxis-extproc` |
| **`_poll_status`** | Parallel workers churn `maas-gateway-auth` → empty 401/403 flakes | Upstream: re-check gateway AuthPolicy on transient empty 401/403 during poll |
| **Duplicate subscription headers** | `test_duplicate_subscription_headers_ignored` warmup 403 under load | Upstream: 8s post-mint delay + 90s warmup poll (see `test_negative_security.py`) |
| **`deploy.sh`** | Kustomize e2e has no `maas-controller/` source tree | **aigc:** `patch-maas-deploy-for-aigc.sh` after fetch (keep until upstream accepts `deployment/` only) |
| **`deploy-models.sh`** | Waits for all Kuadrant AuthPolicies (flakes when aigc adds policies) | **aigc:** patch scopes wait to `managed-by=maas-controller` label |
| **`validate-deployment.sh`** | BBR model URL is gateway-root; path-based HTTPRoute needs path prefix | Upstream/prow: path-based inference URL when `/v1/models` returns host-only URL |
| **`prow_run_*` prerequisites** | Empty `PRAXIS_EXTPROC_IMAGE` + `set -e` silent exit | **aigc-only** in `prow_run_ai_gateway_controller_test.sh` |
| **Must-gather** | CI artifacts for HTTPRoute/LLMIS debugging | **aigc:** Tekton `must-gather` step + optional `aigc-artifacts.sh` (`E2E_COLLECT_MUST_GATHER=true`) |
| **Webhook handoff** | Pausing `maas-controller` breaks AITenant webhook during praxis install | **aigc-only** in `deploy-ai-gateway-controller.sh` (annotate → pause → delete IPP → **resume** → apply aigc) |

---

## ai-gateway-controller repo gaps (not MaaS)

- [ ] `RELATED_IMAGE_ODH_AI_GATEWAY_CONTROLLER_IMAGE` — operator still installs via kustomize in CI
- [ ] Merge [odh-konflux-central](https://github.com/jland-redhat/odh-konflux-central) group-test pipeline upstream
- [ ] TEMP praxis pin (`quay.io/maas/odh-praxis-extproc:pr699-76cb977`) — revert when Konflux `odh-praxis-extproc-ci` includes #699
- [ ] Point `.tekton/ai-gateway-controller-group-test.yaml` at upstream konflux-central after merge
