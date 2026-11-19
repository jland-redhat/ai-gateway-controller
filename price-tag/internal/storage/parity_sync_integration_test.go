package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestParitySyncLatchIntegration covers the blocker found in review: api-mode
// pods do not run the backfill, so the parity sync must latch rollupsReadyNow
// from rollup_meta before dashboards can serve rollups. Like the other live
// tests, it skips without DATABASE_URL and restores the shared meta rows.
func TestParitySyncLatchIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — parity latch needs PostgreSQL")
	}

	store, err := New(dsn, 0, PoolConfig{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	keys := []string{metaRollupsReady, metaParityHealthy, metaParityCheckedAt}
	type saved struct {
		value string
		found bool
	}
	previous := make(map[string]saved, len(keys))
	for _, key := range keys {
		value, found, lookupErr := store.metaLookup(ctx, key)
		if lookupErr != nil {
			t.Fatalf("snapshot %s: %v", key, lookupErr)
		}
		previous[key] = saved{value: value, found: found}
	}
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		for _, key := range keys {
			prev := previous[key]
			if prev.found {
				_ = store.metaSet(restoreCtx, key, prev.value)
			} else {
				_, _ = store.db.ExecContext(restoreCtx, `DELETE FROM rollup_meta WHERE key = $1`, key)
			}
		}
	})

	store.rollupsReadyNow.Store(false)
	store.parityHealthy.Store(false)

	if err := store.metaSet(ctx, metaRollupsReady, "true"); err != nil {
		t.Fatalf("seed rollups_ready: %v", err)
	}
	if err := store.metaSet(ctx, metaParityHealthy, "true"); err != nil {
		t.Fatalf("seed parity_healthy: %v", err)
	}
	if err := store.metaSet(ctx, metaParityCheckedAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed parity_checked_at: %v", err)
	}

	store.syncParityFromMeta(ctx, 15*time.Minute)

	if !store.rollupsReadyNow.Load() {
		t.Fatal("syncParityFromMeta did not latch rollupsReadyNow from rollup_meta")
	}
	if !store.parityHealthy.Load() {
		t.Fatal("syncParityFromMeta did not sync parityHealthy=true from rollup_meta")
	}
	if !store.parityConfirmed.Load() {
		t.Fatal("a fresh green verdict must latch parityConfirmed")
	}
}

// TestParitySyncStaleVerdictDoesNotLatchIntegration pins the boot-hydration
// bound: a green verdict older than maxAge must not confirm parity on an API
// pod that has never served rollups, or a restart after a long outage would
// serve an ancient rollup table. A pod that had already confirmed parity keeps
// serving under the documented serve-stale policy.
func TestParitySyncStaleVerdictDoesNotLatchIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — parity state sync needs PostgreSQL")
	}

	store, err := New(dsn, 0, PoolConfig{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	keys := []string{metaRollupsReady, metaParityHealthy, metaParityCheckedAt}
	type saved struct {
		value string
		found bool
	}
	previous := make(map[string]saved, len(keys))
	for _, key := range keys {
		value, found, lookupErr := store.metaLookup(ctx, key)
		if lookupErr != nil {
			t.Fatalf("snapshot %s: %v", key, lookupErr)
		}
		previous[key] = saved{value: value, found: found}
	}
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		for _, key := range keys {
			prev := previous[key]
			if prev.found {
				_ = store.metaSet(restoreCtx, key, prev.value)
			} else {
				_, _ = store.db.ExecContext(restoreCtx, `DELETE FROM rollup_meta WHERE key = $1`, key)
			}
		}
	})

	const maxAge = 15 * time.Minute
	if err := store.metaSet(ctx, metaRollupsReady, "true"); err != nil {
		t.Fatalf("seed rollups_ready: %v", err)
	}
	if err := store.metaSet(ctx, metaParityHealthy, "true"); err != nil {
		t.Fatalf("seed parity_healthy: %v", err)
	}
	if err := store.metaSet(ctx, metaParityCheckedAt, time.Now().UTC().Add(-2*maxAge).Format(time.RFC3339)); err != nil {
		t.Fatalf("seed stale parity_checked_at: %v", err)
	}

	store.useRollups.Store(true)
	store.rollupsReadyNow.Store(true)
	store.parityHealthy.Store(false)
	store.parityConfirmed.Store(false)
	store.servingRollups.Store(false)

	store.syncParityFromMeta(ctx, maxAge)

	if store.parityHealthy.Load() {
		t.Fatal("a stale verdict must not be reported as healthy")
	}
	if store.parityConfirmed.Load() {
		t.Fatal("a stale verdict must not latch parityConfirmed on a pod that never served rollups")
	}
	if store.rollupsLive() {
		t.Fatal("an unconfirmed pod must fall back to raw reads when the verdict is stale")
	}

	// A pod that confirmed parity earlier keeps serving the continuously
	// rebuilt rollups even after the verdict goes stale.
	store.parityConfirmed.Store(true)
	store.syncParityFromMeta(ctx, maxAge)
	if !store.parityConfirmed.Load() {
		t.Fatal("a stale verdict must not clear an existing confirmation")
	}
	if !store.rollupsLive() {
		t.Fatal("a confirmed pod must keep serving rollups under serve-stale")
	}
}

// TestParitySyncMissingMetaKeepsConfirmedRollupsServingIntegration pins the
// serve-stale decision for the missing-meta case too: health becomes unknown,
// but a deployment that already confirmed parity keeps reading the continuously
// rebuilt rollups. Only the explicit DASHBOARD_USE_ROLLUPS=false switch is a
// read-gate fallback.
func TestParitySyncMissingMetaKeepsConfirmedRollupsServingIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — parity state sync needs PostgreSQL")
	}

	store, err := New(dsn, 0, PoolConfig{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	keys := []string{metaRollupsReady, metaParityHealthy, metaParityCheckedAt}
	type saved struct {
		value string
		found bool
	}
	previous := make(map[string]saved, len(keys))
	for _, key := range keys {
		value, found, lookupErr := store.metaLookup(ctx, key)
		if lookupErr != nil {
			t.Fatalf("snapshot %s: %v", key, lookupErr)
		}
		previous[key] = saved{value: value, found: found}
	}
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer restoreCancel()
		for _, key := range keys {
			prev := previous[key]
			if prev.found {
				_ = store.metaSet(restoreCtx, key, prev.value)
			} else {
				_, _ = store.db.ExecContext(restoreCtx, `DELETE FROM rollup_meta WHERE key = $1`, key)
			}
		}
	})

	store.useRollups.Store(true)
	store.rollupsReadyNow.Store(true)
	store.parityHealthy.Store(true)
	store.parityConfirmed.Store(true)
	store.servingRollups.Store(false)
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM rollup_meta WHERE key IN ($1, $2)`, metaParityHealthy, metaParityCheckedAt); err != nil {
		t.Fatalf("delete parity meta: %v", err)
	}

	store.syncParityFromMeta(ctx, 15*time.Minute)

	if store.parityHealthy.Load() {
		t.Fatal("missing parity meta must clear the local green health flag")
	}
	if !store.rollupsLive() {
		t.Fatal("a confirmed deployment must keep serving rollups under serve-stale when meta disappears")
	}
}
