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
#   PRAXIS_EXTPROC_IMAGE            — praxis dataplane image (default from params.env)
#   REMOVE_MAAS_IPP                 — default true; delete maas-controller legacy IPP
#                                     and hand ext_proc to ai-gateway-controller
#   PRAXIS_INSTALL_TIMEOUT          — default 180s
#   SCALE_DOWN_PAYLOAD_PROCESSING   — deprecated alias for REMOVE_MAAS_IPP

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
DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_CONTROLLER_DEPLOYMENT="${MAAS_CONTROLLER_DEPLOYMENT:-maas-controller}"
MAAS_CONTROLLER_PRIOR_REPLICAS=""
MAAS_CONTROLLER_PAUSED=false
MAAS_CONTROLLER_RESUME_REPLICAS="${MAAS_CONTROLLER_RESUME_REPLICAS:-1}"

REMOVE_MAAS_IPP="${REMOVE_MAAS_IPP:-${SCALE_DOWN_PAYLOAD_PROCESSING:-true}}"
PRAXIS_INSTALL_TIMEOUT="${PRAXIS_INSTALL_TIMEOUT:-180}"
MANAGED_FALSE_ANNOTATION="${MANAGED_FALSE_ANNOTATION:-opendatahub.io/managed=false}"

_default_praxis_image() {
  local params="${PROJECT_ROOT}/config/self/default/params.env"
  if [[ -f "${params}" ]]; then
    grep -E '^praxis-extproc-image=' "${params}" | cut -d= -f2- || true
  fi
}
PRAXIS_EXTPROC_IMAGE="${PRAXIS_EXTPROC_IMAGE:-$(_default_praxis_image)}"
PRAXIS_EXTPROC_IMAGE="${PRAXIS_EXTPROC_IMAGE:-quay.io/opendatahub/odh-praxis-extproc:odh-stable}"

# Default-tenant IPP object names in the gateway namespace (maas-controller + praxis share these).
IPP_NAMES=(
  payload-processing
  payload-pre-processing
)

if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE}" ]]; then
  echo "ERROR: AI_GATEWAY_CONTROLLER_IMAGE is required" >&2
  exit 1
fi

_on_deploy_exit() {
  local code=$?
  if [[ $code -ne 0 && -n "${MAAS_CONTROLLER_PRIOR_REPLICAS:-}" && "${MAAS_CONTROLLER_PAUSED:-}" == "true" ]]; then
    echo "Deploy failed; attempting to resume maas-controller ..."
    _resume_maas_controller || true
  fi
}
trap _on_deploy_exit EXIT

_pause_maas_controller() {
  local replicas
  replicas="$(oc get deployment "${MAAS_CONTROLLER_DEPLOYMENT}" -n "${DEPLOYMENT_NAMESPACE}" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")"
  if [[ -z "${replicas}" ]]; then
    echo "WARN: ${MAAS_CONTROLLER_DEPLOYMENT} not found in ${DEPLOYMENT_NAMESPACE}; skipping pause"
    MAAS_CONTROLLER_PRIOR_REPLICAS=""
    return 0
  fi
  MAAS_CONTROLLER_PRIOR_REPLICAS="${replicas}"
  if [[ "${replicas}" == "0" ]]; then
    MAAS_CONTROLLER_PRIOR_REPLICAS="${MAAS_CONTROLLER_RESUME_REPLICAS}"
    echo "maas-controller already scaled to 0; will resume to ${MAAS_CONTROLLER_PRIOR_REPLICAS} after handoff"
    return 0
  fi
  echo "Pausing maas-controller (scale ${replicas} -> 0) to avoid IPP reconcile during handoff ..."
  oc scale deployment "${MAAS_CONTROLLER_DEPLOYMENT}" -n "${DEPLOYMENT_NAMESPACE}" --replicas=0
  oc rollout status deployment/"${MAAS_CONTROLLER_DEPLOYMENT}" -n "${DEPLOYMENT_NAMESPACE}" --timeout=180s
  MAAS_CONTROLLER_PAUSED=true
}

_resume_maas_controller() {
  [[ -n "${MAAS_CONTROLLER_PRIOR_REPLICAS:-}" ]] || return 0
  if [[ "${MAAS_CONTROLLER_PRIOR_REPLICAS}" == "0" ]]; then
    return 0
  fi
  echo "Resuming maas-controller (scale -> ${MAAS_CONTROLLER_PRIOR_REPLICAS}) ..."
  oc scale deployment "${MAAS_CONTROLLER_DEPLOYMENT}" -n "${DEPLOYMENT_NAMESPACE}" \
    --replicas="${MAAS_CONTROLLER_PRIOR_REPLICAS}"
  oc rollout status deployment/"${MAAS_CONTROLLER_DEPLOYMENT}" -n "${DEPLOYMENT_NAMESPACE}" --timeout=180s || true
}

_delete_legacy_ipp_in_gateway_namespace() {
  echo "Removing maas-controller legacy IPP in ${GATEWAY_NAMESPACE} ..."
  local name
  for name in "${IPP_NAMES[@]}"; do
    oc delete deployment,service,destinationrule "${name}" -n "${GATEWAY_NAMESPACE}" --ignore-not-found --wait=false 2>/dev/null || true
  done
  oc delete envoyfilter payload-processing -n "${GATEWAY_NAMESPACE}" --ignore-not-found --wait=false 2>/dev/null || true
  oc delete networkpolicy payload-processing -n "${GATEWAY_NAMESPACE}" --ignore-not-found --wait=false 2>/dev/null || true
  oc delete hpa -n "${GATEWAY_NAMESPACE}" -l app.kubernetes.io/name=payload-processing --ignore-not-found 2>/dev/null || true

  local deadline=$((SECONDS + 120))
  while [[ $SECONDS -lt $deadline ]]; do
    if ! oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" &>/dev/null \
      && ! oc get deployment payload-pre-processing -n "${GATEWAY_NAMESPACE}" &>/dev/null; then
      echo "Legacy IPP Deployments removed from ${GATEWAY_NAMESPACE}"
      return 0
    fi
    sleep 2
  done
  echo "ERROR: legacy IPP Deployments still present in ${GATEWAY_NAMESPACE} after delete" >&2
  oc get deploy -n "${GATEWAY_NAMESPACE}" | grep payload || true
  return 1
}

_apply_ai_gateway_controller() {
  local work_dir
  work_dir="$(mktemp -d -t aigc-kustomize.XXXXXXXXXX)"

  cp -a "${PROJECT_ROOT}/config/self" "${work_dir}/self"
  sed -i "s|^ai-gateway-controller-image=.*|ai-gateway-controller-image=${AI_GATEWAY_CONTROLLER_IMAGE}|" \
    "${work_dir}/self/default/params.env"
  sed -i "s|^praxis-extproc-image=.*|praxis-extproc-image=${PRAXIS_EXTPROC_IMAGE}|" \
    "${work_dir}/self/default/params.env"

  if command -v kustomize >/dev/null 2>&1; then
    kustomize build "${work_dir}/self/default"
  elif kubectl kustomize "${work_dir}/self/default" >/dev/null 2>&1; then
    kubectl kustomize "${work_dir}/self/default"
  else
    rm -rf "${work_dir}"
    echo "ERROR: kustomize or kubectl kustomize required" >&2
    return 1
  fi | \
    sed \
      -e "s|value: maas-default-gateway|value: ${GATEWAY_NAME}|g" \
      -e "s|value: openshift-ingress|value: ${GATEWAY_NAMESPACE}|g" \
    | oc apply -f -

  rm -rf "${work_dir}"

  oc delete configmap ai-gateway-controller-parameters -n default --ignore-not-found 2>/dev/null || true

  oc rollout status deployment/ai-gateway-controller \
    -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}" --timeout=180s

  echo "Triggering immediate praxis-extproc install ..."
  oc rollout restart deployment/ai-gateway-controller -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}"
  oc rollout status deployment/ai-gateway-controller \
    -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}" --timeout=180s
}

_wait_for_praxis_extproc() {
  echo "Waiting for praxis-extproc (${PRAXIS_EXTPROC_IMAGE}) in ${GATEWAY_NAMESPACE} (timeout: ${PRAXIS_INSTALL_TIMEOUT}s) ..."
  local deadline=$((SECONDS + PRAXIS_INSTALL_TIMEOUT))
  while [[ $SECONDS -lt $deadline ]]; do
    local image args ready
    if ! oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" &>/dev/null; then
      echo "  Waiting... deployment/payload-processing not created yet"
      sleep 5
      continue
    fi
    image="$(oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
    args="$(oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)"
    ready="$(oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")"

    if [[ "${image}" == "${PRAXIS_EXTPROC_IMAGE}" ]] \
      && [[ "${args}" == *"/etc/praxis/extproc.yaml"* ]] \
      && [[ "${ready:-0}" -ge 1 ]]; then
      echo "✅ praxis-extproc ready: ${GATEWAY_NAMESPACE}/payload-processing image=${image}"
      return 0
    fi
    # Manager may still be applying; also accept any odh-praxis-extproc tag once rolled out.
    if [[ "${image}" == *"odh-praxis-extproc"* ]] \
      && [[ "${args}" == *"/etc/praxis/extproc.yaml"* ]] \
      && [[ "${ready:-0}" -ge 1 ]]; then
      echo "✅ praxis-extproc ready: ${GATEWAY_NAMESPACE}/payload-processing image=${image}"
      return 0
    fi
    echo "  Waiting... image=${image:-<none>} ready=${ready:-0} args=${args:-<none>}"
    sleep 5
  done

  echo "ERROR: praxis-extproc not ready after ${PRAXIS_INSTALL_TIMEOUT}s" >&2
  oc get deployment -n "${GATEWAY_NAMESPACE}" | grep payload || true
  oc logs deployment/ai-gateway-controller -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}" --tail=30 2>&1 || true
  return 1
}

_protect_praxis_from_maas_reconcile() {
  echo "Annotating praxis IPP resources ${MANAGED_FALSE_ANNOTATION} so maas-controller skips them ..."
  local name kind
  for name in "${IPP_NAMES[@]}"; do
    for kind in deployment service destinationrule; do
      oc annotate "${kind}" "${name}" -n "${GATEWAY_NAMESPACE}" \
        "${MANAGED_FALSE_ANNOTATION}" --overwrite 2>/dev/null || true
    done
  done
  oc annotate envoyfilter payload-processing -n "${GATEWAY_NAMESPACE}" \
    "${MANAGED_FALSE_ANNOTATION}" --overwrite 2>/dev/null || true
  oc annotate networkpolicy payload-processing -n "${GATEWAY_NAMESPACE}" \
    "${MANAGED_FALSE_ANNOTATION}" --overwrite 2>/dev/null || true
}

assert_praxis_extproc_image() {
  local image
  image="$(oc get deployment payload-processing -n "${GATEWAY_NAMESPACE}" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')"
  if [[ "${image}" != "${PRAXIS_EXTPROC_IMAGE}" ]]; then
    echo "ERROR: expected payload-processing image ${PRAXIS_EXTPROC_IMAGE}, got ${image}" >&2
    return 1
  fi
  if [[ "${image}" == *"odh-ai-gateway-payload-processing"* ]]; then
    echo "ERROR: legacy IPP image still in use: ${image}" >&2
    return 1
  fi
  echo "Verified ext_proc dataplane image: ${image}"
}

echo "Deploying ai-gateway-controller image: ${AI_GATEWAY_CONTROLLER_IMAGE}"
echo "  praxis-extproc image: ${PRAXIS_EXTPROC_IMAGE}"
echo "  namespace: ${AI_GATEWAY_CONTROLLER_NAMESPACE}"
echo "  gateway: ${GATEWAY_NAMESPACE}/${GATEWAY_NAME}"
echo "  remove maas IPP: ${REMOVE_MAAS_IPP}"

if [[ "${REMOVE_MAAS_IPP}" == "true" ]]; then
  _pause_maas_controller
  _delete_legacy_ipp_in_gateway_namespace
fi

_apply_ai_gateway_controller

if [[ "${REMOVE_MAAS_IPP}" == "true" ]]; then
  _wait_for_praxis_extproc
  _protect_praxis_from_maas_reconcile
  _resume_maas_controller
fi

assert_praxis_extproc_image
echo "ai-gateway-controller rollout complete (praxis-extproc handoff done)"
