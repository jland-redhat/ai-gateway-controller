package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
)

func TestRequireAuthClearsSpoofedIdentityHeaders(t *testing.T) {
	t.Setenv("SESSION_SECRET", "test-session-secret")
	cfg := config.Config{UserHeader: "X-Forwarded-User", GroupsHeader: "X-Forwarded-Groups"}
	h := NewAuthHandler(cfg)

	tests := []struct {
		name       string
		groups     []string
		wantGroups string
	}{
		{name: "empty session groups", groups: nil, wantGroups: ""},
		{name: "session groups replace spoofed header", groups: []string{"GE", `evil","admin`}, wantGroups: `["GE","evil\",\"admin"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cookieResponse := httptest.NewRecorder()
			h.setCookie(cookieResponse, "alice@example.com", tc.groups, "")

			request := httptest.NewRequest(http.MethodGet, "/api/v1/me/keys", nil)
			request.AddCookie(cookieResponse.Result().Cookies()[0])
			request.Header.Set(cfg.UserHeader, "spoofed@example.com")
			request.Header.Set(cfg.GroupsHeader, `["super_admin"]`)
			request.Header.Set(realUserHeader, "spoofed-admin@example.com")

			response := httptest.NewRecorder()
			h.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get(cfg.UserHeader); got != "alice@example.com" {
					t.Errorf("user header = %q, want signed-session user", got)
				}
				if got := r.Header.Get(cfg.GroupsHeader); got != tc.wantGroups {
					t.Errorf("groups header = %q, want %q", got, tc.wantGroups)
				}
				if got := r.Header.Get(realUserHeader); got != "" {
					t.Errorf("real-user header = %q, want cleared", got)
				}
				w.WriteHeader(http.StatusNoContent)
			})(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
			}
		})
	}
}

func TestRequireAuthGroupsHeaderIsValidJSON(t *testing.T) {
	t.Setenv("SESSION_SECRET", "test-session-secret")
	cfg := config.Config{UserHeader: "X-Forwarded-User", GroupsHeader: "X-Forwarded-Groups"}
	h := NewAuthHandler(cfg)
	groups := []string{`a","b`, "GE"}

	cookieResponse := httptest.NewRecorder()
	h.setCookie(cookieResponse, "alice@example.com", groups, "")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookieResponse.Result().Cookies()[0])

	h.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		var decoded []string
		if err := json.Unmarshal([]byte(r.Header.Get(cfg.GroupsHeader)), &decoded); err != nil {
			t.Fatalf("groups header is not valid JSON: %v", err)
		}
		if len(decoded) != len(groups) || decoded[0] != groups[0] || decoded[1] != groups[1] {
			t.Fatalf("decoded groups = %#v, want %#v", decoded, groups)
		}
		w.WriteHeader(http.StatusNoContent)
	})(httptest.NewRecorder(), request)
}
