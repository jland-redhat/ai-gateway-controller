package handler

import (
	"net/http"
	"strconv"

	"github.com/redhat-et/pricetag-metering/internal/storage"
)

type ModelPolicyAdminHandler struct {
	store *storage.Store
}

func NewModelPolicyAdminHandler(store *storage.Store) *ModelPolicyAdminHandler {
	return &ModelPolicyAdminHandler{store: store}
}

type modelPolicyAdminPage struct {
	Policies []storage.PartnerUserModelPolicyRow `json:"policies"`
	Total    int                                 `json:"total"`
	HasMore  bool                                `json:"has_more"`
}

// HandleList returns the users currently restricted by the external
// model-policy API. This is intentionally read-only in the first version.
func (h *ModelPolicyAdminHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit, offset := 50, 0
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
	}
	policies, total, hasMore, err := h.store.ListPartnerUserModelPolicies(r.Context(), limit, offset)
	if err != nil {
		http.Error(w, "model policy list failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, modelPolicyAdminPage{Policies: policies, Total: total, HasMore: hasMore})
}
