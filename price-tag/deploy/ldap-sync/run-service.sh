#!/usr/bin/env bash
#
# Long-running LDAP directory sync for a VPN-connected host.
#
# It runs scripts/sync-ldap-cost-centers.sh once on start (configurable) and
# then every LDAP_SYNC_INTERVAL_SECONDS. A failed run is logged and retried on
# the next interval; it never exits the service loop.
#
set -euo pipefail

INTERVAL="${LDAP_SYNC_INTERVAL_SECONDS:-86400}"
RETRY="${LDAP_SYNC_RETRY_SECONDS:-900}"
RUN_ON_START="${LDAP_SYNC_RUN_ON_START:-1}"
APPLY="${LDAP_SYNC_APPLY:-1}"
FIELDS="${LDAP_SYNC_FIELDS:-directory}"
MIN_MATCHED="${LDAP_SYNC_MIN_MATCHED:-0}"
OUTPUT_DIR="${LDAP_SYNC_OUTPUT_DIR:-/var/lib/ldap-sync}"

if [[ ! "$INTERVAL" =~ ^[0-9]+$ ]] || [[ "$INTERVAL" -lt 60 ]]; then
  echo "ERROR: LDAP_SYNC_INTERVAL_SECONDS must be an integer >= 60 (got $INTERVAL)" >&2
  exit 1
fi
if [[ ! "$RETRY" =~ ^[0-9]+$ ]] || [[ "$RETRY" -lt 60 ]]; then
  echo "ERROR: LDAP_SYNC_RETRY_SECONDS must be an integer >= 60 (got $RETRY)" >&2
  exit 1
fi

if [[ -x /app/scripts/sync-ldap-cost-centers.sh ]]; then
  default_script=/app/scripts/sync-ldap-cost-centers.sh
else
  default_script="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/sync-ldap-cost-centers.sh"
fi
SCRIPT="${LDAP_SYNC_SCRIPT:-$default_script}"
[[ -x "$SCRIPT" ]] || {
  echo "ERROR: sync script is missing or not executable: $SCRIPT" >&2
  exit 1
}

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

run_once() {
  local args=(--output-dir "$OUTPUT_DIR" --fields "$FIELDS" --min-matched "$MIN_MATCHED")
  if [[ -n "${LDAP_PASSWORD_FILE:-}" ]]; then args+=(--password-file "$LDAP_PASSWORD_FILE"); fi
  if [[ -n "${LDAP_CA_CERT:-}" ]]; then args+=(--ca-file "$LDAP_CA_CERT"); fi
  if [[ -n "${DATABASE_URL_FILE:-}" ]]; then args+=(--database-url-file "$DATABASE_URL_FILE"); fi
  if [[ -n "${LDAP_USER_BASE:-}" ]]; then args+=(--ldap-base "$LDAP_USER_BASE"); fi
  if [[ -n "${LDAP_FILTER:-}" ]]; then args+=(--ldap-filter "$LDAP_FILTER"); fi
  if [[ "$APPLY" == "1" ]]; then
    args+=(--apply)
  else
    args+=(--dry-run)
  fi
  "$SCRIPT" "${args[@]}"
}

trap 'log "received shutdown signal; exiting"; exit 0' TERM INT

log "LDAP sync service starting (interval=${INTERVAL}s, apply=${APPLY}, fields=${FIELDS})"
if [[ "$RUN_ON_START" == "1" ]]; then
  if run_once; then
    log "sync run completed"
    delay="$INTERVAL"
  else
    log "sync run failed; will retry after ${RETRY}s"
    delay="$RETRY"
  fi
else
  log "run-on-start disabled; first run after ${INTERVAL}s"
  delay="$INTERVAL"
fi

while true; do
  sleep "$delay" &
  wait $! || true
  if run_once; then
    log "sync run completed"
    delay="$INTERVAL"
  else
    log "sync run failed; will retry after ${RETRY}s"
    delay="$RETRY"
  fi
done
