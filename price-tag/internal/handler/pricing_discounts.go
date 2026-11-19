package handler

import (
	"net/http"

	"github.com/redhat-et/pricetag-metering/internal/pricing"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

type PricingDiscountHandler struct{ store *storage.Store }

func NewPricingDiscountHandler(store *storage.Store) *PricingDiscountHandler {
	return &PricingDiscountHandler{store: store}
}

func (h *PricingDiscountHandler) Handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		policy, err := h.store.GetPricingDiscountPolicy(r.Context())
		if err != nil {
			http.Error(w, "pricing discount policy unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, policy)
	case http.MethodPut:
		var policy pricing.DiscountPolicy
		if !decodeJSON(w, r, &policy) {
			return
		}
		actor := quotaAdminActor(r)
		if err := h.store.UpdatePricingDiscountPolicy(r.Context(), actor, policy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, policy)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
