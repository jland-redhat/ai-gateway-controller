package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

func testCfg() config.Config {
	return config.Config{
		UserHeader:   "X-Forwarded-User",
		GroupsHeader: "X-Forwarded-Groups",
		AdminUsers:   []string{"boss"},
		DefaultGroup: "default",
	}
}

// The pre-existing behaviour of IsAdmin for callers WITH an identity is
// unchanged by this feature (admin allowlist only) — and with the flipped
// default, an anonymous caller is nobody, not an admin.
func TestIsAdmin_SecurityDefaults(t *testing.T) {
	cfg := testCfg()

	mk := func(user string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if user != "" {
			r.Header.Set(cfg.UserHeader, user)
		}
		return r
	}
	if !IsAdmin(cfg, mk("boss")) {
		t.Error("allowlisted admin must be admin")
	}
	if IsAdmin(cfg, mk("someone")) {
		t.Error("random user must not be admin")
	}
	if IsAdmin(cfg, mk("")) {
		t.Error("anonymous caller must NOT be admin once org visibility exists")
	}
	anonAdmin := testCfg()
	anonAdmin.AllowUnauthenticatedAdmin = true
	if !IsAdmin(anonAdmin, mk("")) {
		t.Error("explicit opt-in must still work for local dev")
	}
}

// ApplyScope is the single gate every dashboard query passes through.
// These cases run without a database on purpose: the branches they cover
// (admin passthrough, anonymous rejection) must not touch the store at all.
func TestApplyScope_DBFreeBranches(t *testing.T) {
	cfg := testCfg()

	// Admin: filter passes through byte-for-byte — existing behaviour.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(cfg.UserHeader, "boss")
	w := httptest.NewRecorder()
	got, ok := ApplyScope(w, r, nil, cfg, "alice,bob")
	if !ok || got != "alice,bob" {
		t.Errorf("admin passthrough broken: got %q ok=%v", got, ok)
	}

	// Anonymous non-admin: rejected, and a nil store proves nothing was
	// queried (a store call would panic).
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	if _, ok := ApplyScope(w, r, nil, cfg, ""); ok {
		t.Error("anonymous caller must be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous rejection should be 401, got %d", w.Code)
	}
}

// caller must resolve the swapped identity for scope math during view-as.
func TestCaller_ImpersonationScope(t *testing.T) {
	cfg := testCfg()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set(cfg.UserHeader, "viewed-user")
	r.Header.Set(realUserHeader, "boss")
	if c := caller(r, cfg); c != "viewed-user" {
		t.Errorf("caller must be the swapped identity for scope math, got %q", c)
	}
}

// The three-way split the role feature depends on: super-admin implies
// admin (so the operators can still see the usage page their console links
// to), but admin never implies super-admin (the many admins who only want
// usage must not reach the console or routing).
func TestSuperAdminImpliesAdmin(t *testing.T) {
	cfg := config.Config{
		UserHeader:      "X-Forwarded-User",
		AdminUsers:      []string{"plainadmin"},
		SuperAdminUsers: []string{"operator"},
	}
	mk := func(user string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if user != "" {
			r.Header.Set(cfg.UserHeader, user)
		}
		return r
	}
	op := mk("operator")
	if !IsSuperAdmin(cfg, op) {
		t.Error("listed operator must be super-admin")
	}
	if !IsAdmin(cfg, op) {
		t.Error("super-admin must imply admin (usage view stays reachable)")
	}
	pa := mk("plainadmin")
	if IsSuperAdmin(cfg, pa) {
		t.Error("a plain admin must NOT be super-admin")
	}
	if !IsAdmin(cfg, pa) {
		t.Error("plain admin must still be admin")
	}
	if IsAdmin(cfg, mk("")) || IsSuperAdmin(cfg, mk("")) {
		t.Error("anonymous caller is neither admin nor super-admin")
	}
}

// RequireSuperAdmin sends a plain admin (who can see usage) to the usage
// dashboard, and a non-admin to their own account — the fallback matches
// what each caller is actually allowed to look at.
func TestRequireSuperAdmin_Fallbacks(t *testing.T) {
	cfg := config.Config{
		UserHeader:      "X-Forwarded-User",
		AdminUsers:      []string{"plainadmin"},
		SuperAdminUsers: []string{"operator"},
	}
	plainAdminLoc := func(user string) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/routing", nil)
		r.Header.Set(cfg.UserHeader, user)
		RequireSuperAdmin(cfg, func(http.ResponseWriter, *http.Request) {
			t.Fatalf("%s must not reach super-admin handler", user)
		})(w, r)
		return w.Header().Get("Location")
	}
	if loc := plainAdminLoc("plainadmin"); loc != "/dashboard" {
		t.Errorf("plain admin redirected to %q, want /dashboard", loc)
	}
	if loc := plainAdminLoc("someone"); loc != "/me" {
		t.Errorf("non-admin redirected to %q, want /me", loc)
	}
	// Super-admin passes through.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/routing", nil)
	r.Header.Set(cfg.UserHeader, "operator")
	reached := false
	RequireSuperAdmin(cfg, func(http.ResponseWriter, *http.Request) { reached = true })(w, r)
	if !reached {
		t.Error("operator must reach the super-admin handler")
	}
}

func TestRequireAdminDashboard(t *testing.T) {
	cfg := config.Config{UserHeader: "X-Forwarded-User", AdminUsers: []string{"admin"}}
	called := false
	page := RequireAdminDashboard(cfg,
		func(http.ResponseWriter, *http.Request) { called = true },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("coming soon"))
		},
	)

	regular := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	regular.Header.Set(cfg.UserHeader, "user")
	w := httptest.NewRecorder()
	page(w, regular)
	if called || w.Code != http.StatusOK || w.Body.String() != "coming soon" {
		t.Fatalf("regular user dashboard response: called=%v status=%d body=%q", called, w.Code, w.Body.String())
	}

	admin := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	admin.Header.Set(cfg.UserHeader, "admin")
	w = httptest.NewRecorder()
	page(w, admin)
	if !called {
		t.Fatal("admin must reach dashboard")
	}
}

func TestRequireAdminAPI(t *testing.T) {
	cfg := config.Config{UserHeader: "X-Forwarded-User", AdminUsers: []string{"admin"}}
	api := RequireAdminAPI(cfg, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	regular := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/users", nil)
	regular.Header.Set(cfg.UserHeader, "user")
	w := httptest.NewRecorder()
	api(w, regular)
	if w.Code != http.StatusForbidden {
		t.Fatalf("regular user API status = %d, want 403", w.Code)
	}

	admin := httptest.NewRequest(http.MethodGet, "/api/v1/dashboard/users", nil)
	admin.Header.Set(cfg.UserHeader, "admin")
	w = httptest.NewRecorder()
	api(w, admin)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin API status = %d, want 204", w.Code)
	}
}

func TestHandleRoles(t *testing.T) {
	admins, supers := partnerRoleLists([]storage.PartnerUser{
		{Role: storage.PartnerRoleAdmin, Tags: map[string]any{"email": "bob@example.com"}},
		{Role: storage.PartnerRoleAdmin, Tags: map[string]any{"email": "alice@example.com"}},
		{Role: storage.PartnerRoleSuperAdmin, Tags: map[string]any{"email": "operator@example.com"}},
		{Role: storage.PartnerRoleUser, Tags: map[string]any{"email": "user@example.com"}},
	})
	if len(admins) != 2 || admins[0] != "alice@example.com" || admins[1] != "bob@example.com" {
		t.Errorf("admins = %v, want [alice@example.com bob@example.com]", admins)
	}
	if len(supers) != 1 || supers[0] != "operator@example.com" {
		t.Errorf("superAdmins = %v, want [operator@example.com]", supers)
	}
}
