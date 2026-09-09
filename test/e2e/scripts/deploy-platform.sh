#!/bin/bash
# ai-gateway-controller variant: deploy MaaS via operator + pinned container images.
# Does not require maas-controller/ or maas-api/ source trees in this repo.

set -euo pipefail

if [[ -z "${PROJECT_ROOT:-}" ]]; then
    _dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    PROJECT_ROOT="$(cd "$_dir/../../.." && pwd)"
fi
[[ "$(type -t find_project_root 2>/dev/null)" == "function" ]] || source "$PROJECT_ROOT/scripts/deployment-helpers.sh"
[[ "$(type -t apply_default_oidc_for_keycloak 2>/dev/null)" == "function" ]] || source "$PROJECT_ROOT/test/e2e/scripts/auth_utils.sh"
# shellcheck disable=SC1091
source "$PROJECT_ROOT/test/e2e/scripts/maas-image-defaults.sh"

DEPLOY_MODE="${DEPLOY_MODE:-operator}"
INSECURE_HTTP="${INSECURE_HTTP:-false}"
EXTERNAL_OIDC="${EXTERNAL_OIDC:-false}"
SKIP_AUTH_CHECK="${SKIP_AUTH_CHECK:-true}"
export POLICY_ENGINE="${POLICY_ENGINE:-rhcl}"
export INGRESS_MODE="${INGRESS_MODE:-clusterip}"
AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-$(resolve_authorino_namespace "${POLICY_ENGINE}")}"
export AUTHORINO_NAMESPACE

deploy_maas_platform() {
    echo "Deploying MaaS platform (operator mode, remote images only)..."
    echo "  DEPLOY_MODE=${DEPLOY_MODE}"
    echo "  MAAS_API_IMAGE=${MAAS_API_IMAGE}"
    echo "  MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE}"

    echo "Installing cert-manager and LeaderWorkerSet operators..."
    if ! bash "$PROJECT_ROOT/.github/hack/install-cert-manager-and-lws.sh"; then
        echo "ERROR: cert-manager/LWS installation failed"
        exit 1
    fi

    export DB_SSLMODE="${DB_SSLMODE:-disable}"
    local deploy_cmd=(
        "$PROJECT_ROOT/scripts/deploy.sh"
        --deployment-mode "${DEPLOY_MODE}"
        --policy-engine "${POLICY_ENGINE}"
    )
    [[ -n "${OPERATOR_CATALOG:-}" ]] && deploy_cmd+=(--operator-catalog "${OPERATOR_CATALOG}")
    [[ -n "${OPERATOR_IMAGE:-}" ]] && deploy_cmd+=(--operator-image "${OPERATOR_IMAGE}")
    [[ "$INSECURE_HTTP" == "true" ]] && deploy_cmd+=(--disable-tls-backend)

    if ! "${deploy_cmd[@]}"; then
        echo "ERROR: MaaS platform deployment failed"
        exit 1
    fi

    if [[ "${SKIP_AUTH_CHECK:-true}" == "true" ]]; then
        echo "WARNING: Skipping Authorino readiness check (SKIP_AUTH_CHECK=true)"
    else
        echo "Waiting for Authorino (namespace: ${AUTHORINO_NAMESPACE})..."
        if ! wait_authorino_ready "$AUTHORINO_NAMESPACE" "$AUTHORINO_TIMEOUT"; then
            echo "ERROR: Authorino did not become ready (timeout: ${AUTHORINO_TIMEOUT}s)"
            exit 1
        fi
    fi

    echo "MaaS platform deployment completed"
}

deploy_maas_platform
