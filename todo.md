# ai-gateway-controller E2E / Konflux — TODO

## Done in this repo

- [x] MaaS e2e sync + operator-mode deploy (no local `maas-controller/` tree)
- [x] Default MaaS images: `quay.io/opendatahub/maas-api:latest` and `maas-controller:latest` (main Konflux pushes)
- [x] PR image: `AI_GATEWAY_CONTROLLER_IMAGE` from Konflux snapshot → `deploy-ai-gateway-controller.sh`

## Done in odh-konflux-central fork

[jland-redhat/odh-konflux-central](https://github.com/jland-redhat/odh-konflux-central) `main` (synced with upstream):

- [x] `integration-tests/ai-gateway-controller/pr-group-testing-pipeline.yaml`
- [x] `gitops/integration-testing-prerequisites.yaml` — `ai-gateway-controller-group`

- [ ] Open PR to `opendatahub-io/odh-konflux-central` and merge
- [ ] After merge: point `.tekton/ai-gateway-controller-group-test.yaml` at upstream konflux-central

## Konflux cluster setup (manual)

- [ ] `ai-gateway-controller-group` component under `group-testing`
- [ ] `konflux-integration-runner` pull access to `quay.io/opendatahub/odh-ai-gateway-controller`
- [ ] PAC `git_auth_secret` in `open-data-hub-tenant`

## Image policy

| Component | Image source |
|-----------|----------------|
| `maas-api` | `quay.io/opendatahub/maas-api:latest` (MaaS `main` push) |
| `maas-controller` | `quay.io/opendatahub/maas-controller:latest` (MaaS `main` push) |
| `ai-gateway-controller` | PR snapshot digest from `odh-ai-gateway-controller-ci` |

Override MaaS tags with `MAAS_IMAGE_TAG` or explicit `MAAS_*_IMAGE` env vars.

## Known gaps

- Operator has no `RELATED_IMAGE_ODH_AI_GATEWAY_CONTROLLER_IMAGE` — CI installs controller via kustomize and scales down `payload-processing`
- After `sync-maas-e2e-tests.sh`, `patch-maas-deploy-for-aigc.sh` re-applies deploy.sh operator-mode fix

## Local run

```bash
./hack/scripts/sync-maas-e2e-tests.sh
AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
  ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh
```
