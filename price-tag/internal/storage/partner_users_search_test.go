package storage

import "testing"

// TestListPartnerUsersPageSearchAndPaging covers the search filter, the total
// count, page boundaries, and that an empty search returns the full set. It
// needs a Postgres (DATABASE_URL); otherwise it skips.
func TestListPartnerUsersPageSearchAndPaging(t *testing.T) {
	s, ctx := openTestStore(t)

	// Seed a deterministic set: 5 "Flash" users plus one unrelated user.
	people := []struct {
		id, email, first, last string
	}{
		{"123e4567-e89b-12d3-a456-000000000001", "flash.one@example.com", "Flash", "One"},
		{"123e4567-e89b-12d3-a456-000000000002", "flash.two@example.com", "Flash", "Two"},
		{"123e4567-e89b-12d3-a456-000000000003", "flash.three@example.com", "Flash", "Three"},
		{"123e4567-e89b-12d3-a456-000000000004", "flash.four@example.com", "Flash", "Four"},
		{"123e4567-e89b-12d3-a456-000000000005", "flash.five@example.com", "Flash", "Five"},
		{"123e4567-e89b-12d3-a456-0000000000ff", "zoe@example.com", "Zoe", "Zebra"},
	}
	for _, p := range people {
		if _, err := s.CreatePartnerUser(ctx, "partner-m2m", p.id, map[string]any{
			"email": p.email, "first_name": p.first, "last_name": p.last,
		}); err != nil {
			t.Fatalf("CreatePartnerUser %s: %v", p.email, err)
		}
	}

	// Empty search returns everyone.
	users, total, hasMore, err := s.ListPartnerUsersPage(ctx, "", 50, 0)
	if err != nil {
		t.Fatalf("ListPartnerUsersPage empty: %v", err)
	}
	if total != 6 || len(users) != 6 || hasMore {
		t.Fatalf("empty search = total %d, len %d, hasMore %v; want 6/6/false", total, len(users), hasMore)
	}

	// Case-insensitive name search narrows to the 5 Flash users.
	users, total, hasMore, err = s.ListPartnerUsersPage(ctx, "FLASH", 50, 0)
	if err != nil {
		t.Fatalf("ListPartnerUsersPage search: %v", err)
	}
	if total != 5 || len(users) != 5 || hasMore {
		t.Fatalf("name search = total %d, len %d, hasMore %v; want 5/5/false", total, len(users), hasMore)
	}

	// Email substring search.
	_, total, _, err = s.ListPartnerUsersPage(ctx, "zoe@", 50, 0)
	if err != nil || total != 1 {
		t.Fatalf("email search total = %d, err %v; want 1", total, err)
	}

	// Partial user-id search.
	_, total, _, err = s.ListPartnerUsersPage(ctx, "0000000000ff", 50, 0)
	if err != nil || total != 1 {
		t.Fatalf("user-id search total = %d, err %v; want 1", total, err)
	}

	// Page boundaries over the search result: 2 per page across the 5 Flash users.
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		users, total, hasMore, err = s.ListPartnerUsersPage(ctx, "flash", 2, page*2)
		if err != nil {
			t.Fatalf("paged search page %d: %v", page, err)
		}
		if total != 5 {
			t.Fatalf("paged search total page %d = %d; want 5", page, total)
		}
		for _, u := range users {
			seen[u.UserID] = true
		}
		wantMore := page < 2
		if hasMore != wantMore {
			t.Fatalf("page %d hasMore = %v; want %v", page, hasMore, wantMore)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged search covered %d distinct users; want 5", len(seen))
	}

	// No-match search is empty, not an error.
	users, total, hasMore, err = s.ListPartnerUsersPage(ctx, "no-such-person", 50, 0)
	if err != nil || total != 0 || len(users) != 0 || hasMore {
		t.Fatalf("no-match = total %d, len %d, hasMore %v, err %v; want 0/0/false/nil", total, len(users), hasMore, err)
	}

	// Oversized limit is clamped server-side (no error, returns the page).
	if _, _, _, err := s.ListPartnerUsersPage(ctx, "", 100000, 0); err != nil {
		t.Fatalf("oversized limit should be clamped, got err %v", err)
	}
}
