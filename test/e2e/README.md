# ai-gateway-controller E2E

MaaS pytest suite is **not vendored** in this repo. `prow_run_ai_gateway_controller_test.sh` fetches [models-as-a-service](https://github.com/opendatahub-io/models-as-a-service) into `test/maas-e2e/` (gitignored), pinned by `test/maas-e2e.lock`.

**Orchestration (this repo):** `test/e2e/scripts/`  
**Tests + fixtures (MaaS checkout):** `test/maas-e2e/test/e2e/`

See [TODO.md](TODO.md) for excluded tests and upstream gaps.

```bash
AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-pr \
  ./test/e2e/scripts/prow_run_ai_gateway_controller_test.sh
```
