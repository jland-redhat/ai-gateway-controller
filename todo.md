# ai-gateway-controller E2E / Konflux — TODO

## Done in this repo

- [x] `hack/scripts/sync-maas-e2e-tests.sh`
- [x] `test/e2e/scripts/prow_run_ai_gateway_controller_test.sh`
- [x] `test/e2e/scripts/run_e2e_tests.sh` (no external-model tests)
- [x] `test/e2e/scripts/deploy-ai-gateway-controller.sh`
- [x] `.tekton/ai-gateway-controller-group-test.yaml`
- [x] `.tekton/odh-ai-gateway-controller-pull-request.yaml` — `enable-group-testing: "true"`

## Done in odh-konflux-central fork

[jland-redhat/odh-konflux-central](https://github.com/jland-redhat/odh-konflux-central) `main`:

- [x] `integration-tests/ai-gateway-controller/pr-group-testing-pipeline.yaml`
- [x] `pipelineruns/ai-gateway-controller/ai-gateway-controller-group-test.yaml`
- [x] `gitops/integration-testing-prerequisites.yaml` — `ai-gateway-controller-group`

- [ ] Open PR to `opendatahub-io/odh-konflux-central` and merge
- [ ] After merge: point `.tekton/ai-gateway-controller-group-test.yaml` at upstream konflux-central

## Konflux cluster setup (manual)

- [ ] `ai-gateway-controller-group` component under `group-testing` app
- [ ] `konflux-integration-runner` can pull `quay.io/opendatahub/odh-ai-gateway-controller`
- [ ] PAC `git_auth_secret` in `open-data-hub-tenant`

## Known gaps

- Operator has no `RELATED_IMAGE_ODH_AI_GATEWAY_CONTROLLER_IMAGE` yet — CI installs controller directly and scales down `payload-processing`
- Group test snapshots only `odh-ai-gateway-controller-ci`; MaaS platform uses stable deploy defaults

## Local run

```bash
./hack/scripts/sync-maas-e2e-tests.sh
AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
  ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh
```
