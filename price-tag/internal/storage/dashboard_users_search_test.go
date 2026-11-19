package storage

import (
	"testing"
	"time"
)

// TestGetDashboardUsersSearch verifies the usage Users view honors a
// case-insensitive search over username and display name, scoped within the
// existing filters. It needs a Postgres (DATABASE_URL); otherwise it skips.
func TestGetDashboardUsersSearch(t *testing.T) {
	s, ctx := openTestStore(t)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := from.Add(48 * time.Hour)

	// Two users with usage in the window. One has a partner display name.
	id := "123e4567-e89b-12d3-a456-4266140000aa"
	if _, err := s.CreatePartnerUser(ctx, "partner-m2m", id, map[string]any{
		"email": "alice@example.com", "first_name": "Alice", "last_name": "Wonder",
	}); err != nil {
		t.Fatalf("CreatePartnerUser: %v", err)
	}
	for _, ev := range []struct {
		id, username string
	}{
		{"u-alice", "alice@example.com"},
		{"u-bob", "bob@example.com"},
	} {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO usage_events (event_id,timestamp,username,model,prompt_tokens,completion_tokens,total_tokens)
			VALUES ($1,$2,$3,'model-a',10,5,15)`, ev.id, from.Add(time.Hour), ev.username); err != nil {
			t.Fatalf("insert usage %s: %v", ev.id, err)
		}
	}

	// No search → both users.
	all, err := s.GetDashboardUsers(ctx, from, until, "", "", "", "", "", 100, 0, "", "", true)
	if err != nil {
		t.Fatalf("GetDashboardUsers all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("no search returned %d users; want 2", len(all))
	}

	// Search by username substring (case-insensitive).
	bob, err := s.GetDashboardUsers(ctx, from, until, "", "", "", "", "", 100, 0, "", "BOB", true)
	if err != nil {
		t.Fatalf("GetDashboardUsers bob: %v", err)
	}
	if len(bob) != 1 || bob[0].Username != "bob@example.com" {
		t.Fatalf("username search = %#v; want only bob", bob)
	}

	// Search by display name (partner first/last), not present in username.
	wonder, err := s.GetDashboardUsers(ctx, from, until, "", "", "", "", "", 100, 0, "", "wonder", true)
	if err != nil {
		t.Fatalf("GetDashboardUsers wonder: %v", err)
	}
	if len(wonder) != 1 || wonder[0].Username != "alice@example.com" {
		t.Fatalf("display-name search = %#v; want only alice", wonder)
	}

	// No match → empty, no error.
	none, err := s.GetDashboardUsers(ctx, from, until, "", "", "", "", "", 100, 0, "", "nobody", true)
	if err != nil || len(none) != 0 {
		t.Fatalf("no-match search = %#v, err %v; want empty", none, err)
	}
}
