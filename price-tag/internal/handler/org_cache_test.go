package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
)

func testOrgCacheConfig() config.Config {
	return config.Config{
		DashboardCacheEnabled:    true,
		DashboardCacheTTLSeconds: 60,
		UserHeader:               "X-Forwarded-User",
	}
}

func TestOrgCacheSeparatesCallersScopesAndHonoursBypass(t *testing.T) {
	c := newOrgResponseCache(testOrgCacheConfig())
	calls := 0
	seenRoots := []string{}
	next := func(w http.ResponseWriter, r *http.Request) {
		calls++
		root, _ := orgScopeFromRequest(r)
		seenRoots = append(seenRoots, root)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root":"` + root + `"}`))
	}

	scopes := map[string]string{"alice": "root-a", "bob": "root-b"}
	denied := map[string]bool{}
	resolve := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		user := r.Header.Get("X-Forwarded-User")
		if denied[user] {
			http.Error(w, "root outside your visibility scope", http.StatusForbidden)
			return "", false
		}
		return scopes[user], true
	}
	h := c.wrap(next, resolve)

	do := func(user, cacheControl string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/org/usage?range=7d", nil)
		req.Header.Set("X-Forwarded-User", user)
		if cacheControl != "" {
			req.Header.Set("Cache-Control", cacheControl)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	if rec := do("alice", ""); rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rec.Code)
	}
	if rec := do("alice", ""); rec.Code != http.StatusOK {
		t.Fatalf("cached call status = %d", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("same caller+scope+query must hit the cache: calls = %d", calls)
	}
	do("bob", "")
	if calls != 2 {
		t.Fatalf("a different caller must not share an entry: calls = %d", calls)
	}
	do("alice", "no-cache")
	if calls != 3 {
		t.Fatalf("Cache-Control: no-cache must bypass the cache: calls = %d", calls)
	}

	// A role/subtree change must not reuse the old entry: the resolver runs
	// before the lookup and the resolved root is part of the key.
	scopes["alice"] = "root-c"
	if rec := do("alice", ""); rec.Code != http.StatusOK || calls != 4 {
		t.Fatalf("changed scope must miss the cache: status=%d calls=%d", rec.Code, calls)
	}
	if seenRoots[len(seenRoots)-1] != "root-c" {
		t.Fatalf("handler saw root %q, want root-c", seenRoots[len(seenRoots)-1])
	}
	// And a revoked caller is rejected even though an entry exists.
	denied["alice"] = true
	if rec := do("alice", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked caller got %d, want 403", rec.Code)
	}
	if calls != 4 {
		t.Fatalf("revoked caller reached the handler: calls = %d", calls)
	}
}

func TestOrgCacheDoesNotStoreOversizeResponses(t *testing.T) {
	c := newOrgResponseCache(testOrgCacheConfig())
	calls := 0
	h := c.wrap(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(strings.Repeat("x", orgCacheMaxBodyBytes+1)))
	}, func(http.ResponseWriter, *http.Request) (string, bool) { return "root-a", true })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/org/usage?range=7d", nil)
	req.Header.Set("X-Forwarded-User", "alice")
	h(httptest.NewRecorder(), req)
	h(httptest.NewRecorder(), req)
	if calls != 2 {
		t.Fatalf("oversize responses must not be cached: calls = %d", calls)
	}
}

func TestOrgCacheDoesNotStoreErrors(t *testing.T) {
	c := newOrgResponseCache(testOrgCacheConfig())
	calls := 0
	h := c.wrap(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}, func(http.ResponseWriter, *http.Request) (string, bool) { return "root-a", true })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/org/charts?range=7d", nil)
	req.Header.Set("X-Forwarded-User", "alice")
	h(httptest.NewRecorder(), req)
	h(httptest.NewRecorder(), req)
	if calls != 2 {
		t.Fatalf("non-200 responses must never be cached: calls = %d", calls)
	}
}
