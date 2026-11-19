package storage

import (
	"strings"
	"testing"
)

// TestDashboardCountryOfNormalizesTags covers the normalization the gate and
// the SQL policy join share. Unknown, empty and malformed values must read as
// "no country", which the gate treats as allowed.
func TestDashboardCountryOfNormalizesTags(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]any
		want string
	}{
		{"upper", map[string]any{"country": "FR"}, "FR"},
		{"lower", map[string]any{"country": " fr "}, "FR"},
		{"missing", map[string]any{"email": "a@b"}, ""},
		{"empty", map[string]any{"country": "  "}, ""},
		{"too long", map[string]any{"country": "FRA"}, ""},
		{"not a string", map[string]any{"country": 42}, ""},
		{"nil tags", nil, ""},
	}
	for _, tc := range cases {
		if got := DashboardCountryOf(tc.tags); got != tc.want {
			t.Errorf("%s: DashboardCountryOf(%v) = %q, want %q", tc.name, tc.tags, got, tc.want)
		}
	}
}

// TestDefaultDashboardBlockedCountriesIsSane guards the seed list: unique,
// upper-case alpha-2 codes. A typo here would silently fail to match anyone.
func TestDefaultDashboardBlockedCountriesIsSane(t *testing.T) {
	seen := map[string]bool{}
	for _, country := range DefaultDashboardBlockedCountries {
		if DashboardCountryOf(map[string]any{"country": country}) != country {
			t.Errorf("invalid seed code %q", country)
		}
		if seen[country] {
			t.Errorf("duplicate seed code %q", country)
		}
		seen[country] = true
	}
	if !seen["FR"] || !seen["DE"] {
		t.Errorf("expected the Council of Europe seed to include FR and DE")
	}
	if seen["US"] {
		t.Errorf("US must not be in the seed list")
	}
}

// TestDashboardCountryPolicyLifecycle exercises the policy against a real
// Postgres: the seeded list, impact counting, block/unblock, and the drill-down
// guard helper.
func TestDashboardCountryPolicyLifecycle(t *testing.T) {
	s, ctx := openTestStore(t)

	blocked, err := s.DashboardBlockedCountries(ctx)
	if err != nil {
		t.Fatalf("list seeded policy: %v", err)
	}
	if len(blocked) == 0 {
		t.Fatalf("expected the initial policy to be seeded")
	}
	isBlocked, err := s.DashboardCountryBlocked(ctx, "FR")
	if err != nil || !isBlocked {
		t.Fatalf("FR should be blocked by the seed (blocked=%v err=%v)", isBlocked, err)
	}
	if isBlocked, err = s.DashboardCountryBlocked(ctx, "US"); err != nil || isBlocked {
		t.Fatalf("US must not be blocked (blocked=%v err=%v)", isBlocked, err)
	}

	const (
		frUser  = "9b8f6a12-0000-4000-8000-000000000f01"
		usUser  = "9b8f6a12-0000-4000-8000-000000000f02"
		frLogin = "fr-user@example.com"
		usLogin = "us-user@example.com"
	)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO partner_users (user_id, tags, created_by, updated_by) VALUES ($1, $2, 'policy-test', 'policy-test')`,
		frUser, `{"email":"fr-user@example.com","country":"fr","first_name":"Fr"}`); err != nil {
		t.Fatalf("insert FR user: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO partner_users (user_id, tags, created_by, updated_by) VALUES ($1, $2, 'policy-test', 'policy-test')`,
		usUser, `{"email":"us-user@example.com","country":"US","first_name":"Us"}`); err != nil {
		t.Fatalf("insert US user: %v", err)
	}
	for _, login := range []struct{ username, userID string }{{frLogin, frUser}, {usLogin, usUser}} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO partner_user_logins (username, user_id, is_current) VALUES ($1, $2, true)`,
			login.username, login.userID); err != nil {
			t.Fatalf("insert login %s: %v", login.username, err)
		}
	}

	impacted, people, err := s.DashboardPolicyImpact(ctx)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if impacted != 1 || people != 2 {
		t.Fatalf("impact = %d of %d, want 1 of 2", impacted, people)
	}

	// Drill-down guard: the blocked person is not addressable by a manager.
	if blocked, err := s.PartnerUserCountryBlocked(ctx, frUser); err != nil || !blocked {
		t.Fatalf("FR user should be blocked (blocked=%v err=%v)", blocked, err)
	}
	if blocked, err := s.PartnerUserCountryBlocked(ctx, usUser); err != nil || blocked {
		t.Fatalf("US user should not be blocked (blocked=%v err=%v)", blocked, err)
	}

	// Unblock, re-block: the gate's cache invalidation depends on these
	// returning whether they changed anything.
	removed, err := s.RemoveDashboardBlockedCountry(ctx, "tester", "fr")
	if err != nil || !removed {
		t.Fatalf("remove FR: removed=%v err=%v", removed, err)
	}
	if removed, err = s.RemoveDashboardBlockedCountry(ctx, "tester", "FR"); err != nil || removed {
		t.Fatalf("second remove of FR must be a no-op (removed=%v err=%v)", removed, err)
	}
	if blocked, err := s.DashboardCountryBlocked(ctx, "FR"); err != nil || blocked {
		t.Fatalf("FR should be unblocked (blocked=%v err=%v)", blocked, err)
	}
	added, err := s.AddDashboardBlockedCountry(ctx, "tester", "fr")
	if err != nil || !added {
		t.Fatalf("add FR: added=%v err=%v", added, err)
	}
	if added, err = s.AddDashboardBlockedCountry(ctx, "tester", "FR"); err != nil || added {
		t.Fatalf("second add of FR must be a no-op (added=%v err=%v)", added, err)
	}

	// The seed is one-shot: clearing the list must survive a restart instead
	// of re-blocking the whole default list on the next migrate.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dashboard_blocked_countries`); err != nil {
		t.Fatalf("clear policy: %v", err)
	}
	if err := s.migrateDashboardPolicy(ctx); err != nil {
		t.Fatalf("re-run seed: %v", err)
	}
	blocked, err = s.DashboardBlockedCountries(ctx)
	if err != nil {
		t.Fatalf("list after reseed: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("seed must not run twice, got %v", blocked)
	}
}

// TestDashboardPolicyMigrationPresent keeps the schema honest: the policy
// tables must be created by the regular migration path.
func TestDashboardPolicyMigrationPresent(t *testing.T) {
	joined := ""
	for _, stmt := range migrations {
		joined += stmt + "\n"
	}
	for _, want := range []string{"dashboard_blocked_countries", "dashboard_policy_meta"} {
		if !strings.Contains(joined, want) {
			t.Errorf("migrations missing %s", want)
		}
	}
}
