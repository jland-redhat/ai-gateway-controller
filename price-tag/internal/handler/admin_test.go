package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/maasapi"
)

// platformGroups is the group scope for every maas-api key call. A missing
// k8s client must surface as an error (the handlers degrade to an empty
// list / 503 with a log line) — it must NOT yield an empty slice, because
// the client's guard then rejects the call with a message about groups that
// would point at the wrong layer during an incident.
func TestPlatformGroupsNoKubernetes(t *testing.T) {
	h := NewAdminHandler(nil, nil, config.Config{})
	_, err := h.platformGroups(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes-not-configured error, got %v", err)
	}
}

func TestLegacyCreateKeyRejectsUnapprovedGroup(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	cfg := config.Config{
		UserHeader:   "X-Forwarded-User",
		GroupsHeader: "X-Forwarded-Groups",
	}
	h := NewAdminHandler(nil, maasapi.NewClient(server.URL, "test-tenant"), cfg)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/keys", strings.NewReader(`{"group":"team-b","keyName":"atlas"}`))
	req.Header.Set(cfg.UserHeader, "alice@example.com")
	req.Header.Set(cfg.GroupsHeader, `["team-a"]`)
	rec := httptest.NewRecorder()

	h.HandleKeys(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if called {
		t.Fatal("unapproved group reached maas-api instead of being rejected")
	}
}
