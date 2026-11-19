#!/usr/bin/env bash
#
# Hermetic shell tests for scripts/sync-ldap-cost-centers.sh.
#
# No LDAP and no PostgreSQL are contacted. The tests cover the review blockers:
# LDIF unfolding before base64 decode, manager DN resolution, scoped and valid
# manager stubs, zero-row SQL, changed-only updates, backup/rollback artifacts,
# and the local psql path that must not put the DSN/password in argv.
#
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/sync-ldap-cost-centers.sh"
[[ -x "$SCRIPT" ]] || { echo "missing script: $SCRIPT" >&2; exit 1; }

tmp="$(mktemp -d)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
file_mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }

desc='Cost Center With A Very Long Name That Wraps Across Lines'
b64="$(printf '%s' "$desc" | base64 | tr -d '\n')"
part1="${b64:0:22}"
part2="${b64:22}"

cat > "$tmp/sample.ldif" <<EOF
dn: uid=alice,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com
rhatUUID: 11111111-1111-1111-1111-111111111111
uid: alice
mail: alice@example.com
givenName: Alice
sn: Example
co: US
rhatCostCenter: CC-123
rhatCostCenterDesc:: $part1
 $part2
rhatOrganization: Platform Engineering
rhatJobTitle: Software Engineer
rhatJobCode: 12345
rhatGeo: NA
rhatOfficeLocation: Raleigh
rhatWorkerId: W123
rhatSupervisorWorkerId: W456
rhatEmployeeSubType: Regular
rhatPersonType: Employee
rhatHireDate: 2020-01-02
rhatTermDate:
rhatDeactivated: FALSE
manager: uid=bob,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com

dn: uid=bob,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com
rhatUUID: 22222222-2222-2222-2222-222222222222
uid: bob
mail: bob@example.com
givenName: Bob
sn: Boss

dn: uid=carol,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com
rhatUUID: 33333333-3333-3333-3333-333333333333
uid: carol
mail: carol@example.com
givenName: Carol
sn: Example
manager: uid=dave,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com

dn: uid=dave,cn=users,cn=accounts,dc=ipa,dc=redhat,dc=com
rhatUUID: 44444444-4444-4444-4444-444444444444
uid: dave
givenName: Dave
sn: NoMail
EOF

# --- dry run: folded base64, manager resolution, all fields -----------------
"$SCRIPT" --ldif "$tmp/sample.ldif" --fields all --output-dir "$tmp/out" --dry-run > "$tmp/dry.log" 2>&1 \
  || fail "dry run failed: $(cat "$tmp/dry.log")"
grep -q 'ldap_records=4' "$tmp/dry.log" || fail "wrong LDAP record count"
grep -q 'ldap_records_with_selected_data=4' "$tmp/dry.log" || fail "wrong usable row count"
grep -q 'ldap_manager_unresolved=0' "$tmp/dry.log" || fail "manager DN did not resolve"
grep -q 'ldap_manager_stub_candidates=1' "$tmp/dry.log" || fail "manager stub candidate count is wrong"
grep -q 'ldap_manager_stub_skipped=1' "$tmp/dry.log" || fail "manager stub without email was not skipped"

csv="$(find "$tmp/out" -name '*.csv' | head -1)"
main_sql="$(find "$tmp/out" -name '*.sql' ! -name '*.backup.sql' | head -1)"
[[ -n "$csv" && -n "$main_sql" ]] || fail "missing generated CSV or SQL"

grep -Fq "$desc" "$csv" || fail "folded base64 value was not decoded before unfolding"
grep -Fq 'Platform Engineering' "$csv" || fail "organization missing from CSV"
grep -Fq '22222222-2222-2222-2222-222222222222' "$csv" || fail "manager UUID missing from CSV"
grep -q 'manager_user_id' "$main_sql" || fail "manager_user_id is not updated"
grep -q '_ldap_manager_stubs' "$main_sql" || fail "missing manager stub provisioning"
grep -q 'u.manager_uuid = s.user_id' "$main_sql" || fail "manager stubs are not scoped to matched users"
grep -q "lower(x.tags->>'email') = lower(s.email)" "$main_sql" || fail "stub email uniqueness guard missing"
grep -q 'COALESCE((SELECT m.user_id FROM partner_users m WHERE m.user_id = u.manager_uuid AND m.active)' "$main_sql" || fail "inactive/missing manager would overwrite the existing link"
grep -q 'manager_stubs_created' "$main_sql" || fail "missing manager stub count"
grep -q 'IS DISTINCT FROM' "$main_sql" || fail "changed-only guard missing"
if grep -q 'updated_at' "$main_sql"; then fail "sync must not touch updated_at"; fi
grep -q 'SET LOCAL lock_timeout' "$main_sql" || fail "lock_timeout missing"

# --- default directory run: no email opt-in, so no stub rows ----------------
"$SCRIPT" --ldif "$tmp/sample.ldif" --output-dir "$tmp/out-directory" --dry-run > "$tmp/directory.log" 2>&1 \
  || fail "directory dry run failed: $(cat "$tmp/directory.log")"
grep -q 'ldap_manager_stubs_disabled=email_not_selected' "$tmp/directory.log" || fail "directory run did not report disabled stubs"
directory_sql="$(find "$tmp/out-directory" -name '*.sql' ! -name '*.backup.sql' | head -1)"
if grep -q '_ldap_manager_stubs' "$directory_sql"; then fail "directory run emitted manager stubs without email opt-in"; fi
grep -q 'manager_user_id' "$directory_sql" || fail "directory run stopped linking existing manager rows"

# --- zero usable rows: valid SQL, no dangling VALUES ------------------------
: > "$tmp/empty.ldif"
"$SCRIPT" --ldif "$tmp/empty.ldif" --output-dir "$tmp/out-empty" --dry-run > "$tmp/empty.log" 2>&1 \
  || fail "empty dry run failed: $(cat "$tmp/empty.log")"
empty_sql="$(find "$tmp/out-empty" -name '*.sql' ! -name '*.backup.sql' | head -1)"
grep -q 'updated_partner_users=0' "$empty_sql" || fail "empty SQL lacks zero count"
if grep -q 'INSERT INTO _ldap_sync' "$empty_sql"; then fail "empty SQL emitted an INSERT"; fi
if grep -Eq 'VALUES[[:space:]]*;' "$empty_sql"; then fail "empty SQL emitted dangling VALUES"; fi

# --- local apply: no DSN/password in psql argv ------------------------------
mkdir -p "$tmp/bin"
cat > "$tmp/bin/psql" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PSQL_ARGS_LOG"
env | grep '^PG' | sort >> "$PSQL_ENV_LOG"
file=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == "-f" ]]; then file="$arg"; fi
  prev="$arg"
done
if [[ "$file" == *backup.sql ]]; then
  echo 'kind,user_id,tags,manager_user_id'
  echo 'user,11111111-1111-1111-1111-111111111111,"{""email"": ""old@example.com""}","22222222-2222-2222-2222-222222222222"'
  echo 'stub,33333333-3333-3333-3333-333333333333,,'
else
  echo 'updated_partner_users=1'
fi
SH
chmod +x "$tmp/bin/psql"

PATH="$tmp/bin:$PATH" \
PSQL_ARGS_LOG="$tmp/args.log" \
PSQL_ENV_LOG="$tmp/env.log" \
DATABASE_URL='postgres://alice:s3cr3t@db.example.com:5432/pricetag?sslmode=require' \
  "$SCRIPT" --ldif "$tmp/sample.ldif" --output-dir "$tmp/out-apply" --apply > "$tmp/apply.log" 2>&1 \
  || fail "apply failed: $(cat "$tmp/apply.log")"

if grep -Eq 's3cr3t|postgres://' "$tmp/args.log"; then
  fail "DSN or password leaked into psql argv"
fi
grep -q 'PGSERVICEFILE=' "$tmp/env.log" || fail "PGSERVICEFILE was not set for psql"
grep -q 'PGPASSFILE=' "$tmp/env.log" || fail "PGPASSFILE was not set for psql"

backup_csv="$(find "$tmp/out-apply" -name '*.backup.csv' | head -1)"
rollback_sql="$(find "$tmp/out-apply" -name '*.rollback.sql' | head -1)"
[[ -n "$backup_csv" && -n "$rollback_sql" ]] || fail "backup/rollback artifact missing"
[[ "$(file_mode "$backup_csv")" == "600" ]] || fail "backup CSV is not mode 600"
[[ "$(file_mode "$rollback_sql")" == "600" ]] || fail "rollback SQL is not mode 600"
grep -q 'UPDATE partner_users' "$rollback_sql" || fail "rollback SQL does not restore partner_users"
grep -q 'DELETE FROM partner_users' "$rollback_sql" || fail "rollback SQL does not remove created manager stubs"

# --- kube mode still reads the DSN from the configured Secret ---------------
cat > "$tmp/bin/kubectl" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${KUBECTL_LOG:?}"
if [[ " $* " == *" get secret "* ]]; then
  printf '%s' 'cG9zdGdyZXM6Ly91c2VyOnMzY3IzdEBkYi5leGFtcGxlLmNvbTo1NDMyL3ByaWNldGFn'
  exit 0
fi
if [[ " $* " == *" mktemp "* ]]; then
  echo /tmp/fake-ldap-remote
  exit 0
fi
if [[ " $* " == *" psql "* ]]; then
  file=""
  prev=""
  for arg in "$@"; do
    if [[ "$prev" == "-f" ]]; then file="$arg"; fi
    prev="$arg"
  done
  if [[ "$file" == *backup.sql ]]; then
    echo 'kind,user_id,tags,manager_user_id'
  else
    echo 'updated_partner_users=1'
  fi
  exit 0
fi
exit 0
SH
chmod +x "$tmp/bin/kubectl"

PATH="$tmp/bin:$PATH" \
KUBECTL_LOG="$tmp/kubectl.log" \
DATABASE_URL='' \
DATABASE_URL_FILE='' \
  "$SCRIPT" --ldif "$tmp/sample.ldif" --output-dir "$tmp/out-kube" --apply \
    --kubectl "$tmp/bin/kubectl" --kube-context ctx --namespace ns \
    --pg-pod pod --pg-secret sec --pg-secret-key key > "$tmp/kube.log" 2>&1 \
  || fail "kube apply failed: $(cat "$tmp/kube.log")"
grep -q 'get secret sec' "$tmp/kubectl.log" || fail "kube mode did not read the DSN Secret"
if grep -Eq 's3cr3t|postgres://' "$tmp/kubectl.log"; then
  fail "DSN leaked into kubectl argv"
fi
grep -q 'updated_partner_users=1' "$tmp/kube.log" || fail "kube apply did not run the SQL"

echo "ok: LDAP sync shell tests"
