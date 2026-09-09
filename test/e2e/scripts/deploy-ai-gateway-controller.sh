#!/usr/bin/env bash
# Deploy (or upgrade) ai-gateway-controller on an existing MaaS cluster for e2e.
#
# Required:
#   AI_GATEWAY_CONTROLLER_IMAGE — manager image to run (Konflux PR image in CI)
#
# Optional:
#   AI_GATEWAY_CONTROLLER_NAMESPACE — default opendatahub
#   GATEWAY_NAMESPACE               — default openshift-ingress
#   GATEWAY_NAME                    — default maas-default-gateway
#   SCALE_DOWN_PAYLOAD_PROCESSING   — default true; scale maas-controller-managed
#                                     payload-processing to 0 to avoid fighting praxis-extproc

set -euo pipefail

_find_project_root() {
  local dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  while [[ "$dir" != "/" && ! -e "$dir/.git" ]]; do dir="$(dirname "$dir")"; done
  [[ -e "$dir/.git" ]] && printf '%s\n' "$dir" || return 1
}

PROJECT_ROOT="$(_find_project_root)"
AI_GATEWAY_CONTROLLER_IMAGE="${AI_GATEWAY_CONTROLLER_IMAGE:-}"
AI_GATEWAY_CONTROLLER_NAMESPACE="${AI_GATEWAY_CONTROLLER_NAMESPACE:-opendatahub}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
SCALE_DOWN_PAYLOAD_PROCESSING="${SCALE_DOWN_PAYLOAD_PROCESSING:-true}"
DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"

if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE}" ]]; then
  echo "ERROR: AI_GATEWAY_CONTROLLER_IMAGE is required" >&2
  exit 1
fi

echo "Deploying ai-gateway-controller image: ${AI_GATEWAY_CONTROLLER_IMAGE}"
echo "  namespace: ${AI_GATEWAY_CONTROLLER_NAMESPACE}"
echo "  gateway: ${GATEWAY_NAMESPACE}/${GATEWAY_NAME}"

if [[ "${SCALE_DOWN_PAYLOAD_PROCESSING}" == "true" ]]; then
  echo "Scaling down payload-processing deployments managed by maas-controller ..."
  while IFS= read -r dep; do
    [[ -z "${dep}" ]] && continue
    ns="${dep%%/*}"
    name="${dep#*/}"
    echo "  scaling ${ns}/${name} to 0"
    oc scale deployment "${name}" -n "${ns}" --replicas=0 2>/dev/null || true
  done < <(oc get deployment -A -l app.kubernetes.io/component=payload-processing \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  oc scale deployment payload-processing -n "${DEPLOYMENT_NAMESPACE}" --replicas=0 2>/dev/null || true
fi

work_dir="$(mktemp -d -t aigc-kustomize.XXXXXXXXXX)"
trap 'rm -rf "${work_dir}"' EXIT

cp -a "${PROJECT_ROOT}/config/self" "${work_dir}/self"
sed -i "s|^ai-gateway-controller-image=.*|ai-gateway-controller-image=${AI_GATEWAY_CONTROLLER_IMAGE}|" \
  "${work_dir}/self/default/params.env"

if command -v kustomize >/dev/null 2>&1; then
  kustomize build "${work_dir}/self/default"
elif kubectl kustomize "${work_dir}/self/default" >/dev/null 2>&1; then
  kubectl kustomize "${work_dir}/self/default"
else
  echo "ERROR: kustomize or kubectl kustomize required" >&2
  exit 1
fi | \
  sed \
    -e "s|value: maas-default-gateway|value: ${GATEWAY_NAME}|g" \
    -e "s|value: openshift-ingress|value: ${GATEWAY_NAMESPACE}|g" \
  | oc apply -f -

oc rollout status deployment/ai-gateway-controller \
  -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}" --timeout=180s

echo "ai-gateway-controller rollout complete"
