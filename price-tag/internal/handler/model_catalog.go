package handler

import (
	"log/slog"
	"net/http"

	"github.com/redhat-et/pricetag-metering/internal/k8s"
)

// ModelCatalogHandler serves the global EnMaaS catalog, not a per-user access
// decision. Route/AuthPolicy authenticates the caller; the catalog intentionally
// does not require a MaaS inference key.
type ModelCatalogHandler struct {
	client *k8s.Client
}

func NewModelCatalogHandler(client *k8s.Client) *ModelCatalogHandler {
	return &ModelCatalogHandler{client: client}
}

type modelCatalogResponse struct {
	Object string              `json:"object"`
	Data   []modelCatalogEntry `json:"data"`
}

type modelCatalogEntry struct {
	ID           string            `json:"id"`
	OwnedBy      string            `json:"owned_by,omitempty"`
	ProviderRefs []k8s.ProviderRef `json:"provider_refs,omitempty"`
}

func (h *ModelCatalogHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if h.client == nil {
		http.Error(w, "model catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	models, err := h.client.ListModels(r.Context())
	if err != nil {
		slog.Error("global model catalog lookup failed", "error", err)
		http.Error(w, "model catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	data := make([]modelCatalogEntry, 0, len(models))
	for _, model := range models {
		data = append(data, modelCatalogEntry{
			ID: model.Name, OwnedBy: model.Provider, ProviderRefs: model.ProviderRefs,
		})
	}
	writeJSON(w, modelCatalogResponse{Object: "list", Data: data})
}
