#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
SCRIPT="$ROOT/test/e2e/scripts/deploy-ai-gateway-controller.sh"

wait_body=$(sed -n '/^_wait_for_praxis_extproc()/,/^_enable_praxis_on_default_tenant()/p' "$SCRIPT")
image_body=$(sed -n '/^assert_praxis_extproc_image()/,/^echo "Deploying ai-gateway-controller image:/p' "$SCRIPT")
gateway_payload_pattern="payload-processing -n \"\${GATEWAY_NAMESPACE}\""
gateway_pre_pattern="payload-pre-processing -n \"\${GATEWAY_NAMESPACE}\""
tenant_payload_pattern="payload-processing -n \"\${tenant_namespace}\""

grep -Fq "$gateway_payload_pattern" <<<"$wait_body"
grep -Fq "$gateway_pre_pattern" <<<"$wait_body"
grep -Fq 'shared praxis-extproc ready' <<<"$wait_body"
grep -Fq '/etc/praxis/extproc.yaml' <<<"$wait_body"
grep -Fq '/etc/praxis/pre-extproc.yaml' <<<"$wait_body"

if grep -Fq "$tenant_payload_pattern" <<<"$wait_body"; then
  echo 'FAIL: shared payload-processing wait still targets tenant_namespace' >&2
  exit 1
fi
if grep -Fq 'payload-processing-external-model' <<<"$wait_body"; then
  echo 'FAIL: shared readiness helper requires the optional ExternalModel workload' >&2
  exit 1
fi

grep -Fq "$gateway_payload_pattern" <<<"$image_body"
if grep -Fq "$tenant_payload_pattern" <<<"$image_body"; then
  echo 'FAIL: shared image assertion still targets tenant_namespace' >&2
  exit 1
fi

# Handoff must pause aigc around select+IPP delete so status clear cannot race
# reclaim to steady (MaaS default-praxis + annotation reconcile).
main_tail=$(sed -n '/^echo "Deploying ai-gateway-controller image:/,$p' "$SCRIPT")
for needle in \
  '_pause_ai_gateway_controller' \
  '_select_praxis_on_default_tenant' \
  '_delete_legacy_ipp_in_gateway_namespace' \
  '_clear_praxis_cleanup_on_default_tenant' \
  '_resume_ai_gateway_controller'
do
  grep -Fq "$needle" <<<"$main_tail" || {
    echo "FAIL: handoff sequence missing ${needle}" >&2
    exit 1
  }
done
pause_line=$(grep -nF '_pause_ai_gateway_controller' <<<"$main_tail" | head -1 | cut -d: -f1)
select_line=$(grep -nF '_select_praxis_on_default_tenant' <<<"$main_tail" | head -1 | cut -d: -f1)
resume_line=$(grep -nF '_resume_ai_gateway_controller' <<<"$main_tail" | head -1 | cut -d: -f1)
(( pause_line < select_line && select_line < resume_line )) || {
  echo 'FAIL: pause must wrap select before resume' >&2
  exit 1
}
grep -Fq 'status}" == "steady"' "$SCRIPT" || {
  echo 'FAIL: select must reject steady after clear while allowing cleanup-complete' >&2
  exit 1
}

echo 'deploy-ai-gateway-controller shared ExtProc wait: PASS'
