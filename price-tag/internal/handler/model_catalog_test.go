package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelCatalogRequiresGETAndConfiguredAdapter(t *testing.T) {
	h := NewModelCatalogHandler(nil)
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		rec := httptest.NewRecorder()
		h.Handle(rec, httptest.NewRequest(method, "/api/v1/models", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.Handle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET without adapter status = %d, want 503", rec.Code)
	}
}
