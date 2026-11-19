package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
)

func TestLogoutRedirectsToProviderEndSessionWhenAdvertised(t *testing.T) {
	t.Setenv("SESSION_SECRET", "test-session-secret")
	h := NewAuthHandler(config.Config{OIDC: config.OIDC{
		ClientID:              "client-id",
		RedirectURL:           "https://dashboard.example/oauth/callback",
		PostLogoutRedirectURL: "https://dashboard.example/login",
	}})
	h.oidc = &oidcProvider{endSessionURL: "https://idp.example/logout?existing=1"}

	response := httptest.NewRecorder()
	h.HandleLogout(response, httptest.NewRequest(http.MethodGet, "/logout", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "idp.example" || location.Path != "/logout" {
		t.Fatalf("logout location = %q, want provider end-session endpoint", location)
	}
	query := location.Query()
	if query.Get("client_id") != "client-id" {
		t.Fatalf("client_id = %q", query.Get("client_id"))
	}
	if query.Get("post_logout_redirect_uri") != "https://dashboard.example/login" {
		t.Fatalf("post_logout_redirect_uri = %q", query.Get("post_logout_redirect_uri"))
	}
	if query.Get("existing") != "1" {
		t.Fatal("provider's existing end-session query was dropped")
	}

	cookies := response.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != cookieName || cookies[0].MaxAge >= 0 {
		t.Fatalf("session cookie was not cleared: %#v", cookies)
	}
}

func TestLogoutStaysLocalWhenProviderHasNoEndSession(t *testing.T) {
	t.Setenv("SESSION_SECRET", "test-session-secret")
	h := NewAuthHandler(config.Config{})
	response := httptest.NewRecorder()
	h.HandleLogout(response, httptest.NewRequest(http.MethodGet, "/logout", nil))
	if got := response.Header().Get("Location"); got != "/login" {
		t.Fatalf("logout location = %q, want /login", got)
	}
}
