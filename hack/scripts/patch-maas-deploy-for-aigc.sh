#!/usr/bin/env bash
# Patch synced MaaS deploy.sh so operator-mode e2e works without a local maas-controller/ tree.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DEPLOY_SH="${PROJECT_ROOT}/scripts/deploy.sh"
MARKER="ai-gateway-controller: operator mode without local maas-controller tree"

if [[ ! -f "${DEPLOY_SH}" ]]; then
  echo "ERROR: ${DEPLOY_SH} not found — run sync-maas-e2e-tests.sh first" >&2
  exit 1
fi

if grep -qF "${MARKER}" "${DEPLOY_SH}"; then
  echo "deploy.sh already patched for ai-gateway-controller"
  exit 0
fi

perl -0pi -e 's/  if \[\[ ! -d "\$controller_dir" \]\]; then\n    log_error "maas-controller directory not found at \$controller_dir — controller is required"\n    return 1\n  fi/  if [[ ! -d "$controller_dir" ]]; then\n    # '"${MARKER}"'\n    if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then\n      log_info "  maas-controller source tree not present; relying on operator-managed deployment"\n    else\n      log_error "maas-controller directory not found at $controller_dir — controller is required"\n      return 1\n    fi\n  fi/s' "${DEPLOY_SH}"

if ! grep -qF "${MARKER}" "${DEPLOY_SH}"; then
  echo "ERROR: failed to patch ${DEPLOY_SH}" >&2
  exit 1
fi

echo "Patched ${DEPLOY_SH}"
