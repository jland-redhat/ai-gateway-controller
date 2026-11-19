package storage

import (
	"errors"
	"testing"
)

// Partner manager scope is derived from partner_users.manager_user_id. These
// tests run against a real Postgres (openTestStore skips without DATABASE_URL).
func TestPartnerManagerScope(t *testing.T) {
	s, ctx := openTestStore(t)

	const boss = "11111111-1111-4111-8111-111111111111"
	const mgr = "22222222-2222-4222-8222-222222222222"
	const alice = "33333333-3333-4333-8333-333333333333"

	mk := func(id, email, first string) {
		if _, err := s.CreatePartnerUser(ctx, "test", id, map[string]any{
			"email": email, "first_name": first, "last_name": "Example", "manager_uuid": nil,
		}); err != nil {
			t.Fatalf("create %s: %v", email, err)
		}
	}
	mk(boss, "boss@example.com", "Boss")
	mk(mgr, "mgr@example.com", "Manager")
	mk(alice, "alice@example.com", "Alice")

	link := func(id, manager string) {
		if _, err := s.UpdatePartnerUserAccess(ctx, "test", id, PartnerRoleUser, &manager); err != nil {
			t.Fatalf("link %s->%s: %v", id, manager, err)
		}
	}
	link(mgr, boss)
	link(alice, mgr)

	// Boss sees the whole chain and is a manager.
	scope, isMgr, err := s.PartnerManagerScope(ctx, "boss@example.com")
	if err != nil || !isMgr {
		t.Fatalf("boss scope err=%v isMgr=%v", err, isMgr)
	}
	for _, want := range []string{"boss@example.com", "mgr@example.com", "alice@example.com"} {
		if !contains(scope, want) {
			t.Fatalf("boss scope missing %s: %v", want, scope)
		}
	}

	// A leaf sees only itself and is not a manager.
	leaf, isMgr, err := s.PartnerManagerScope(ctx, "alice@example.com")
	if err != nil || isMgr || len(leaf) != 1 || leaf[0] != "alice@example.com" {
		t.Fatalf("alice scope=%v isMgr=%v err=%v", leaf, isMgr, err)
	}

	// Unknown username resolves to just itself.
	unknown, isMgr, err := s.PartnerManagerScope(ctx, "nobody@example.com")
	if err != nil || isMgr || len(unknown) != 1 || unknown[0] != "nobody@example.com" {
		t.Fatalf("unknown scope=%v isMgr=%v err=%v", unknown, isMgr, err)
	}

	// A manager cycle must be rejected, keeping the recursive scope bounded.
	if _, err := s.UpdatePartnerUserAccess(ctx, "test", boss, PartnerRoleUser, strPtr(alice)); !errors.Is(err, ErrInvalidPartnerUser) {
		t.Fatalf("cycle assignment must be rejected, got %v", err)
	}
	if scope, _, err := s.PartnerManagerScope(ctx, "boss@example.com"); err != nil {
		t.Fatalf("scope after rejected cycle must still resolve: %v", err)
	} else if !contains(scope, "alice@example.com") {
		t.Fatalf("scope unexpectedly lost descendants: %v", scope)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func strPtr(s string) *string { return &s }
