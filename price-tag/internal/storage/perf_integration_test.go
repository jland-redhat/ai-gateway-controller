package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests exercise the exact SQL added by the performance follow-up
// against a real Postgres. openTestStore skips without DATABASE_URL, so the
// ordinary unit-test run stays hermetic.
func TestEnsureUsageGroupIndexIntegration(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.ensureUsageGroupIndexWithLock(ctx); err != nil {
		t.Fatalf("ensure usage group index: %v", err)
	}
	for _, index := range []string{usageGroupActivityIndex, usageUserActivityIndex} {
		var valid bool
		err := s.db.QueryRowContext(ctx, `
			SELECT i.indisvalid AND i.indisready
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = $1 AND n.nspname = current_schema()`, index).Scan(&valid)
		if err != nil {
			t.Fatalf("inspect index %s: %v", index, err)
		}
		if !valid {
			t.Fatalf("%s is not valid/ready", index)
		}
	}
	// A second pass must be a no-op (and must not drop the valid indexes).
	if err := s.ensureUsageGroupIndexWithLock(ctx); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
}

// TestRecentIndexPlansIntegration pins the exact predicate/ordering shape the
// /recent queries use. A future edit to either the index expression or the SQL
// fails here even when the small integration table would normally seq-scan.
func TestRecentIndexPlansIntegration(t *testing.T) {
	s, ctx := openTestStore(t)
	if err := s.ensureUsageGroupIndexWithLock(ctx); err != nil {
		t.Fatalf("ensure activity indexes: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	explain := func(query string, args ...any) string {
		t.Helper()
		var plan string
		if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) `+query, args...).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		return plan
	}
	groupPlan := explain(`
		SELECT e.timestamp, e.username
		FROM usage_events e
		WHERE ($1 = '' OR COALESCE(NULLIF(e.group_name, ''), 'unknown') = $1)
		ORDER BY e.timestamp DESC
		LIMIT 20`, "perf-plan-group")
	if !strings.Contains(groupPlan, usageGroupActivityIndex) {
		t.Fatalf("group /recent plan does not use %s: %s", usageGroupActivityIndex, groupPlan)
	}
	userPlan := explain(`
		SELECT e.timestamp, e.username
		FROM usage_events e
		WHERE e.username = $1
		ORDER BY e.timestamp DESC
		LIMIT 20`, "perf-plan-user@example.com")
	if !strings.Contains(userPlan, usageUserActivityIndex) {
		t.Fatalf("user /recent plan does not use %s: %s", usageUserActivityIndex, userPlan)
	}
}

func TestGetRecentEventsFilteredWindowIntegration(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Now().UTC()
	oldUser := fmt.Sprintf("perf-old-%d@example.com", now.UnixNano())
	newUser := fmt.Sprintf("perf-new-%d@example.com", now.UnixNano())
	const group = "perf-window-group"

	insert := func(username string, ts time.Time, eventID string) {
		t.Helper()
		if err := s.InsertEvent(ctx, UsageEvent{
			EventID:      eventID,
			Timestamp:    ts,
			Username:     username,
			GroupName:    group,
			Model:        "perf-window-model",
			Provider:     "perf-window-provider",
			Source:       "perf-integration-test",
			PromptTokens: 1,
			TotalTokens:  1,
		}); err != nil {
			t.Fatalf("insert %s: %v", username, err)
		}
	}
	insert(oldUser, now.Add(-40*24*time.Hour), oldUser+"-event")
	insert(newUser, now.Add(-time.Hour), newUser+"-event")

	filtered, err := s.GetRecentEvents(ctx, 20, group, "", "")
	if err != nil {
		t.Fatalf("filtered recent: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Username != newUser {
		t.Fatalf("filtered recent = %+v, want only the recent row", filtered)
	}

	// The unfiltered feed intentionally keeps its pure LIMIT behaviour.
	unfiltered, err := s.GetRecentEvents(ctx, 20, "", "", "")
	if err != nil {
		t.Fatalf("unfiltered recent: %v", err)
	}
	if len(unfiltered) != 2 {
		t.Fatalf("unfiltered recent returned %d rows, want 2", len(unfiltered))
	}
}

func TestGetPersonModelUsageRollupMatchesRawIntegration(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	until := now.Add(time.Hour)
	user := fmt.Sprintf("perf-person-%d@example.com", now.UnixNano())
	const model = "perf-person-model"
	const provider = "perf-person-provider"

	// Write live rollups alongside the raw events so both sources exist.
	s.SetLiveRollups(true)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%s-%d", user, i)
		if err := s.InsertEvent(ctx, UsageEvent{
			EventID:             id,
			Timestamp:           now.Add(-time.Duration(i) * time.Minute),
			Username:            user,
			GroupName:           "perf-person-group",
			Model:               model,
			Provider:            provider,
			Source:              "perf-integration-test",
			PromptTokens:        100,
			CompletionTokens:    50,
			TotalTokens:         150,
			CachedInputTokens:   20,
			CacheCreationTokens: 10,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	raw, err := s.GetPersonModelUsage(ctx, []string{user}, since, until, "ref-model")
	if err != nil {
		t.Fatalf("raw person usage: %v", err)
	}

	s.rollupsReadyNow.Store(true)
	s.parityConfirmed.Store(true)
	s.UseRollups(true)
	rolled, err := s.GetPersonModelUsage(ctx, []string{user}, since, until, "ref-model")
	if err != nil {
		t.Fatalf("rollup person usage: %v", err)
	}

	if len(raw) != 1 || len(rolled) != 1 {
		t.Fatalf("rows raw=%d rollup=%d, want 1 each", len(raw), len(rolled))
	}
	r, g := raw[0], rolled[0]
	if g.Model != r.Model || g.Provider != r.Provider {
		t.Fatalf("identity raw=%s/%s rollup=%s/%s", r.Model, r.Provider, g.Model, g.Provider)
	}
	if g.Requests != 3 || g.PromptTokens != 300 || g.CompletionTokens != 150 ||
		g.TotalTokens != 450 || g.CachedTokens != 60 || g.CostUSD != r.CostUSD {
		t.Fatalf("rollup aggregate %+v does not match raw requests/tokens/cost %+v", g, r)
	}
}
