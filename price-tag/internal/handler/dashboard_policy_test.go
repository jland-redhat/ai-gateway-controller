package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

func TestValidISOCountry(t *testing.T) {
	for _, code := range []string{"FR", "DE", "US", "GB", "TR"} {
		if !validISOCountry(code) {
			t.Errorf("%s should be a valid ISO-3166-1 alpha-2 code", code)
		}
	}
	for _, code := range []string{"", "F", "FRA", "fr", "XX", "1A", "F1"} {
		if validISOCountry(code) {
			t.Errorf("%q must not validate as an ISO-3166-1 alpha-2 code", code)
		}
	}
}

func TestISOCountryCodesSortedAndComplete(t *testing.T) {
	codes := isoCountryCodes()
	if len(codes) < 200 {
		t.Fatalf("expected the full ISO list, got %d codes", len(codes))
	}
	for i := 1; i < len(codes); i++ {
		if codes[i-1] > codes[i] {
			t.Fatalf("codes are not sorted at %d: %q > %q", i, codes[i-1], codes[i])
		}
	}
	found := map[string]bool{}
	for _, code := range codes {
		found[code] = true
	}
	for _, want := range []string{"FR", "DE", "US", "AQ"} {
		if !found[want] {
			t.Errorf("ISO list missing %s", want)
		}
	}
}

func TestDashboardGateDenyAPIAndPage(t *testing.T) {
	gate := &DashboardCountryGate{page: []byte("<html>coming soon</html>")}

	rec := httptest.NewRecorder()
	gate.deny(rec, httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("API status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("API content-type = %q, want JSON", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "dashboard_unavailable_region") {
		t.Errorf("API body = %q, want the machine-readable error", body)
	}

	rec = httptest.NewRecorder()
	gate.deny(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("page status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("page content-type = %q, want HTML", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "coming soon") {
		t.Errorf("page body = %q, want the blocked page", body)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}

	// A non-GET navigation-shaped request is not a page render: it must not
	// return the HTML page to an API client.
	rec = httptest.NewRecorder()
	gate.deny(rec, httptest.NewRequest(http.MethodPost, "/account", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("POST content-type = %q, want JSON", ct)
	}
}

// snapshot is the cache read path: it must serve fresh policy, serve the last
// good set during the failure backoff, and force a refresh once stale. It does
// no I/O, which is what keeps a slow database from serializing every request.
func TestGateSnapshotCachesSuccessAndFailure(t *testing.T) {
	fresh := &DashboardCountryGate{ttl: time.Minute, failTTL: time.Minute,
		blocked: map[string]struct{}{"FR": {}}, loadedAt: time.Now()}
	if set, ok := fresh.snapshot(); !ok || len(set) != 1 {
		t.Fatalf("fresh policy not served from cache (ok=%v len=%d)", ok, len(set))
	}

	backoff := &DashboardCountryGate{ttl: time.Minute, failTTL: time.Minute, failAt: time.Now()}
	if _, ok := backoff.snapshot(); !ok {
		t.Fatal("a failed refresh must be cached for failTTL, not retried per request")
	}

	stale := &DashboardCountryGate{ttl: time.Minute, failTTL: time.Minute,
		loadedAt: time.Now().Add(-2 * time.Minute)}
	if _, ok := stale.snapshot(); ok {
		t.Fatal("stale policy must be refreshed")
	}

	expiredFail := &DashboardCountryGate{ttl: time.Minute, failTTL: time.Minute,
		failAt: time.Now().Add(-2 * time.Minute)}
	if _, ok := expiredFail.snapshot(); ok {
		t.Fatal("an expired failure backoff must retry")
	}
}

// The impersonation control must stay reachable while a super-admin is
// viewing a blocked person; every other session route stays gated.
func TestGateExemptPath(t *testing.T) {
	if !gateExemptPath("/admin/impersonate") {
		t.Fatal("/admin/impersonate must bypass the gate or an impersonating super-admin is locked in")
	}
	for _, path := range []string{"/dashboard", "/manager", "/api/v1/whoami", "/admin", "/admin/impersonate/other"} {
		if gateExemptPath(path) {
			t.Errorf("%s must stay gated", path)
		}
	}
}

// orgTotals is the manager's inclusive total: blocked people are not in the
// table, but their usage counts here. Super-admins see the rows, so nothing is
// reported as hidden for them.
func TestOrgTotalsIncludeBlockedPeople(t *testing.T) {
	rows := []storage.OrgUsageRow{
		{Username: "m@example.com", Requests: 2, TotalTokens: 100, CostUSD: 0.10, QuotaUSD: 5, QuotaSpentUSD: 1},
		{Username: "a@example.com", Requests: 1, TotalTokens: 50, CostUSD: 0.05, QuotaUSD: 5, QuotaSpentUSD: 2},
		{Username: "b@example.com", Requests: 1, TotalTokens: 25, CostUSD: 0.0105, QuotaUSD: 5, QuotaSpentUSD: 3, OverLimit: true, Blocked: true},
	}
	got := orgTotals(rows, false)
	if got.People != 3 || got.WithKey != 3 || got.BlockedPeople != 1 {
		t.Fatalf("people/withkey/blocked = %d/%d/%d, want 3/3/1", got.People, got.WithKey, got.BlockedPeople)
	}
	if got.Requests != 4 || got.TotalTokens != 175 {
		t.Fatalf("requests/tokens = %d/%d, want 4/175", got.Requests, got.TotalTokens)
	}
	if got.CostUSD != 0.16 {
		t.Fatalf("cost = %v, want 0.16 (blocked person included)", got.CostUSD)
	}
	if got.OverLimit != 1 {
		t.Fatalf("over_limit = %d, want 1", got.OverLimit)
	}
	if super := orgTotals(rows, true); super.BlockedPeople != 0 {
		t.Fatalf("super-admin totals report blocked_people = %d, want 0", super.BlockedPeople)
	}
}

func TestDashboardPolicyHandlerRejectsInvalidCountry(t *testing.T) {
	h := NewDashboardPolicyHandler(nil, config.Config{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/dashboard-policies",
		strings.NewReader(`{"country":"XX"}`))
	rec := httptest.NewRecorder()
	h.Handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown country", rec.Code)
	}
}

// The gate is mounted inside RequireAuth. A deployment without the policy
// store (or without the module) must pass every request through untouched.
func TestDashboardGatePassesThroughWithoutStore(t *testing.T) {
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }

	(&DashboardCountryGate{}).Wrap(next)(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if !called {
		t.Fatal("nil-store gate must pass through")
	}
}
