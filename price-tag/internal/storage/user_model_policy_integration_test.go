package storage

import (
	"fmt"
	"testing"
)

// Legacy username-keyed rows (written by the pre-UUID API) must still be read
// and enforced for a login that has no partner record. Cross-login
// aggregation now follows partner_user_logins rather than the retired
// person_identities table, so a bare legacy login is enforced on its own.
func TestUserModelAllowlistLegacyRowsStillEnforced(t *testing.T) {
	s, ctx := openTestStore(t)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO user_model_allowlists (username, models, updated_by) VALUES ('alice', '{model-a,model-b}', 'legacy')`); err != nil {
		t.Fatalf("seed legacy allowlist: %v", err)
	}

	if decision, err := s.GetMonthlyUsage(ctx, "alice", "model-a", false); err != nil || !decision.ModelAllowed || !decision.HasAccess {
		t.Fatalf("allowed model decision = %#v, err %v", decision, err)
	}
	if decision, err := s.GetMonthlyUsage(ctx, "alice", "model-c", false); err != nil || decision.ModelAllowed || decision.HasAccess {
		t.Fatalf("disallowed model decision = %#v, err %v", decision, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_model_allowlists WHERE username = 'alice'`); err != nil {
		t.Fatalf("clear allowlist: %v", err)
	}
	s.invalidateQuotaCache()
	if decision, err := s.GetMonthlyUsage(ctx, "alice", "model-c", false); err != nil || !decision.ModelAllowed || !decision.HasAccess {
		t.Fatalf("baseline access after clearing policy = %#v, err %v", decision, err)
	}
}

func TestNormalizeUserModelAllowlistRejectsWildcardsAndBoundsList(t *testing.T) {
	if _, err := normalizeUserModelAllowlist([]string{"model-*"}); err == nil {
		t.Fatal("expected wildcard model ID to be rejected")
	}
	tooMany := make([]string, userModelAllowlistMax+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("model-%d", i)
	}
	if _, err := normalizeUserModelAllowlist(tooMany); err == nil {
		t.Fatal("expected oversized allowlist to be rejected")
	}
}
