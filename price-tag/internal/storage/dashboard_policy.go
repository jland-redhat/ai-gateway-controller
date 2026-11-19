package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Dashboard country policy.
//
// The dashboard is enabled per region; until a region has its own deployment,
// users whose directory country is on this list are shown a "coming soon" page
// instead of the dashboard. It gates dashboard views only: gateway keys and
// inference are untouched.
//
// The policy is a plain set: a row means "blocked", and removing the row
// unblocks the country. The seed runs once per database (guarded by
// dashboard_policy_meta) so an operator who unblocks every country does not
// get the defaults back on the next restart.
var dashboardPolicyMetaSeededKey = "initial_countries_seeded"

// DefaultDashboardBlockedCountries is the initial policy seeded on a database
// that has never had one. It is the Council of Europe member states (46).
// Operators change the list from Administration -> Dashboard policies.
var DefaultDashboardBlockedCountries = []string{
	"AL", "AD", "AM", "AT", "AZ", "BA", "BE", "BG", "CH", "CY", "CZ", "DE",
	"DK", "EE", "ES", "FI", "FR", "GB", "GE", "GR", "HR", "HU", "IE", "IS",
	"IT", "LI", "LT", "LU", "LV", "MC", "MD", "ME", "MK", "MT", "NL", "NO",
	"PL", "PT", "RO", "RS", "SE", "SI", "SK", "SM", "TR", "UA",
}

// migrateDashboardPolicy seeds the initial list exactly once. The table itself
// is created by the regular migrations slice; this only fills it.
func (s *Store) migrateDashboardPolicy(ctx context.Context) error {
	var seeded bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM dashboard_policy_meta WHERE key = $1)`,
		dashboardPolicyMetaSeededKey).Scan(&seeded); err != nil {
		return fmt.Errorf("dashboard policy meta: %w", err)
	}
	if seeded {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, country := range DefaultDashboardBlockedCountries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dashboard_blocked_countries (country, added_by)
			 VALUES ($1, 'initial-policy') ON CONFLICT (country) DO NOTHING`, country); err != nil {
			return fmt.Errorf("seed dashboard policy %s: %w", country, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dashboard_policy_meta (key, value) VALUES ($1, 'true')
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		dashboardPolicyMetaSeededKey); err != nil {
		return fmt.Errorf("dashboard policy seed marker: %w", err)
	}
	return tx.Commit()
}

// DashboardBlockedCountries returns the blocked ISO-3166-1 alpha-2 codes,
// sorted. A nil slice means no country is blocked.
func (s *Store) DashboardBlockedCountries(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT country FROM dashboard_blocked_countries ORDER BY country`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var country string
		if err := rows.Scan(&country); err != nil {
			return nil, err
		}
		out = append(out, strings.ToUpper(strings.TrimSpace(country)))
	}
	return out, rows.Err()
}

// DashboardCountryBlocked reports whether one code is on the policy list.
func (s *Store) DashboardCountryBlocked(ctx context.Context, country string) (bool, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return false, nil
	}
	var blocked bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM dashboard_blocked_countries WHERE country = $1)`,
		country).Scan(&blocked)
	return blocked, err
}

// AddDashboardBlockedCountry blocks a country. Returns false when it was
// already blocked (idempotent, no second audit row).
func (s *Store) AddDashboardBlockedCountry(ctx context.Context, actor, country string) (bool, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO dashboard_blocked_countries (country, added_by) VALUES ($1, $2)
		 ON CONFLICT (country) DO NOTHING`, country, actor)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if err := s.Audit(ctx, actor, "dashboard_policy.country_block", country, map[string]any{"country": country}); err != nil {
		return true, err
	}
	return true, nil
}

// RemoveDashboardBlockedCountry unblocks a country. Returns false when it was
// not on the list.
func (s *Store) RemoveDashboardBlockedCountry(ctx context.Context, actor, country string) (bool, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM dashboard_blocked_countries WHERE country = $1`, country)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	if err := s.Audit(ctx, actor, "dashboard_policy.country_unblock", country, map[string]any{"country": country}); err != nil {
		return true, err
	}
	return true, nil
}

// DashboardPolicyImpact aggregates active directory users by the blocked state
// of their country. It powers the impact preview in the admin tab (how many
// people a change would affect) and nothing else.
func (s *Store) DashboardPolicyImpact(ctx context.Context) (blockedUsers int, totalUsers int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE d.country IS NOT NULL),
			COUNT(*)
		FROM partner_users p
		LEFT JOIN dashboard_blocked_countries d
			ON d.country = UPPER(BTRIM(COALESCE(p.tags->>'country', '')))
		WHERE p.active`).Scan(&blockedUsers, &totalUsers)
	return blockedUsers, totalUsers, err
}

// PartnerUserCountryBlocked reports whether one partner user (by stable ID) is
// covered by the dashboard country policy. It is used to refuse a manager's
// drill-down into a hidden person.
func (s *Store) PartnerUserCountryBlocked(ctx context.Context, userID string) (bool, error) {
	var blocked bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM partner_users p
			JOIN dashboard_blocked_countries d
				ON d.country = UPPER(BTRIM(COALESCE(p.tags->>'country', '')))
			WHERE p.user_id = $1::uuid
		)`, userID).Scan(&blocked)
	return blocked, err
}

// DashboardBlockedUsernamesInSubtree returns the logins of every blocked
// person in the subtree (empty root = whole organisation). The manager charts
// use it to fold those people into one "hidden by policy" slice while keeping
// their spend in the pie total.
func (s *Store) DashboardBlockedUsernamesInSubtree(ctx context.Context, rootUserID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(user_id, depth, path) AS (
			SELECT p.user_id, 0, ARRAY[p.user_id]
			FROM partner_users p
			WHERE p.active AND ($1 = '' OR p.user_id = $1::uuid)
			UNION ALL
			SELECT p.user_id, st.depth + 1, st.path || p.user_id
			FROM partner_users p JOIN subtree st ON p.manager_user_id = st.user_id
			WHERE p.active AND st.depth < 1000 AND NOT p.user_id = ANY(st.path)
		)
		SELECT DISTINCT l.username
		FROM subtree st
		JOIN partner_users pu ON pu.user_id = st.user_id
		JOIN partner_user_logins l ON l.user_id = st.user_id
		JOIN dashboard_blocked_countries d
			ON d.country = UPPER(BTRIM(COALESCE(pu.tags->>'country', '')))`, rootUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, err
		}
		out[username] = struct{}{}
	}
	return out, rows.Err()
}

// DashboardCountryOf returns the normalized directory country for a partner
// user, or "" when the directory has none. It is used by the dashboard gate;
// the country is normalised the same way the SQL policy join does.
func DashboardCountryOf(tags map[string]any) string {
	if tags == nil {
		return ""
	}
	raw, ok := tags["country"]
	if !ok {
		return ""
	}
	country, ok := raw.(string)
	if !ok {
		return ""
	}
	country = strings.ToUpper(strings.TrimSpace(country))
	if len(country) != 2 {
		return ""
	}
	return country
}

// NormalizeDashboardCountries makes a sorted, upper-cased set for callers that
// compare against the policy list in memory.
func NormalizeDashboardCountries(countries []string) map[string]struct{} {
	out := make(map[string]struct{}, len(countries))
	for _, country := range countries {
		country = strings.ToUpper(strings.TrimSpace(country))
		if country != "" {
			out[country] = struct{}{}
		}
	}
	return out
}

// SortedCountries is a small helper for deterministic output in tests and
// handlers that echo the policy back to the browser.
func SortedCountries(countries []string) []string {
	out := append([]string(nil), countries...)
	for i, country := range out {
		out[i] = strings.ToUpper(strings.TrimSpace(country))
	}
	sort.Strings(out)
	return out
}
