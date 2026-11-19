package storage

import "testing"

func TestDashboardTagsPreservePartnerMetadata(t *testing.T) {
	got := DashboardTags(map[string]any{
		"email": "a@example.com", "country": "US", "manager_uuid": "m",
		"internal_secret": "do-not-display",
	})
	if got["email"] != "a@example.com" || got["country"] != "US" || got["manager_uuid"] != "m" {
		t.Fatalf("allowed partner tags missing: %#v", got)
	}
	if got["internal_secret"] != "do-not-display" {
		t.Fatalf("partner tag was dropped: %#v", got)
	}
}
