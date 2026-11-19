package handler

import (
	"net/http"
	"strings"

	"github.com/redhat-et/pricetag-metering/internal/storage"
)

// QuotaAdminHandler exposes the global safety-net policy to super-admins.
// The authorization wrapper is installed by cmd/main.go; keeping this handler
// small makes it impossible to accidentally expose the policy through a
// non-admin dashboard API.
type QuotaAdminHandler struct {
	store *storage.Store
}

func NewQuotaAdminHandler(store *storage.Store) *QuotaAdminHandler {
	return &QuotaAdminHandler{store: store}
}

type quotaPolicyUpdateRequest struct {
	DefaultMonthlyUSD      *float64  `json:"default_monthly_usd"`
	Enforced               *bool     `json:"enforced"`
	AllowedOverLimitModels *[]string `json:"allowed_over_limit_models"`
}

func quotaAdminActor(r *http.Request) string {
	if actor := strings.TrimSpace(r.Header.Get(realUserHeader)); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(r.Header.Get("X-Forwarded-User")); actor != "" {
		return actor
	}
	return "super-admin"
}

// HandlePolicy serves the global monthly safety-net policy.
// GET returns the current policy; PUT atomically updates any supplied fields.
func (h *QuotaAdminHandler) HandlePolicy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		policy, err := h.store.GetQuotaPolicy(r.Context())
		if err != nil {
			http.Error(w, "safety-net policy unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, policy)
	case http.MethodPut:
		var body quotaPolicyUpdateRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		policy, err := h.store.UpdateQuotaPolicy(r.Context(), quotaAdminActor(r), storage.QuotaPolicyUpdate{
			DefaultMonthlyUSD: body.DefaultMonthlyUSD,
			Enforced:          body.Enforced,
			Models:            body.AllowedOverLimitModels,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, policy)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
