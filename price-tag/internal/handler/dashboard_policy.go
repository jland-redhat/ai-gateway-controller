package handler

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

// DashboardPolicyHandler is the super-admin surface for the dashboard country
// policy. It lists the blocked ISO-3166-1 alpha-2 codes, adds and removes
// them, and previews how many active directory users a change affects.
//
// The route is mounted behind RequireAuth + RequirePartnerSuperAdmin, and the
// dashboard gate exempts super-admins, so policy changes can never lock the
// operator out of the tab that manages them.
type DashboardPolicyHandler struct {
	store *storage.Store
	cfg   config.Config
	gate  *DashboardCountryGate
	// onChange runs after a successful policy change so cached dashboard
	// responses (manager tables and totals) apply it immediately.
	onChange func()
}

// NewDashboardPolicyHandler wires the policy API. gate is optional; when set,
// a policy change invalidates the gate's cached country set immediately.
func NewDashboardPolicyHandler(store *storage.Store, cfg config.Config, gate *DashboardCountryGate) *DashboardPolicyHandler {
	return &DashboardPolicyHandler{store: store, cfg: cfg, gate: gate}
}

// SetOnChange registers a post-change hook (the manager-response cache).
func (h *DashboardPolicyHandler) SetOnChange(fn func()) {
	h.onChange = fn
}

func (h *DashboardPolicyHandler) changed() {
	if h.onChange != nil {
		h.onChange()
	}
}

type dashboardPolicyResponse struct {
	Blocked  []string `json:"blocked"`
	ISO      []string `json:"iso"`
	Impacted int      `json:"impacted"`
	People   int      `json:"people"`
}

func (h *DashboardPolicyHandler) response(w http.ResponseWriter, r *http.Request) {
	blocked, err := h.store.DashboardBlockedCountries(r.Context())
	if err != nil {
		slog.Error("dashboard policy list failed", "error", err)
		http.Error(w, "policy query failed", http.StatusInternalServerError)
		return
	}
	impacted, people, err := h.store.DashboardPolicyImpact(r.Context())
	if err != nil {
		slog.Error("dashboard policy impact failed", "error", err)
		http.Error(w, "policy query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, dashboardPolicyResponse{
		Blocked:  storage.SortedCountries(blocked),
		ISO:      isoCountryCodes(),
		Impacted: impacted,
		People:   people,
	})
}

// Handle serves GET (read the policy) and POST (block a country).
func (h *DashboardPolicyHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.response(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Country string `json:"country"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	country := strings.ToUpper(strings.TrimSpace(body.Country))
	if !validISOCountry(country) {
		http.Error(w, "country must be an ISO 3166-1 alpha-2 code", http.StatusBadRequest)
		return
	}
	added, err := h.store.AddDashboardBlockedCountry(r.Context(), caller(r, h.cfg), country)
	if err != nil {
		slog.Error("dashboard policy add failed", "country", country, "error", err)
		http.Error(w, "policy update failed", http.StatusInternalServerError)
		return
	}
	if added && h.gate != nil {
		h.gate.Invalidate()
	}
	if added {
		h.changed()
	}
	h.response(w, r)
}

// HandleCountry serves DELETE /api/v1/admin/dashboard-policies/{CC}.
func (h *DashboardPolicyHandler) HandleCountry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	country := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/dashboard-policies/")))
	if !validISOCountry(country) {
		http.Error(w, "country must be an ISO 3166-1 alpha-2 code", http.StatusBadRequest)
		return
	}
	removed, err := h.store.RemoveDashboardBlockedCountry(r.Context(), caller(r, h.cfg), country)
	if err != nil {
		slog.Error("dashboard policy remove failed", "country", country, "error", err)
		http.Error(w, "policy update failed", http.StatusInternalServerError)
		return
	}
	if removed && h.gate != nil {
		h.gate.Invalidate()
	}
	if removed {
		h.changed()
	}
	h.response(w, r)
}

// validISOCountry accepts any ISO-3166-1 alpha-2 code the platform knows. The
// directory and the IdP carry alpha-2 codes, so that is the policy vocabulary.
func validISOCountry(country string) bool {
	if len(country) != 2 {
		return false
	}
	_, ok := iso2ToISO3[country]
	return ok
}

// isoCountryCodes returns every selectable alpha-2 code, sorted, so the admin
// tab can offer the full list instead of a hard-coded subset.
func isoCountryCodes() []string {
	out := make([]string, 0, len(iso2ToISO3))
	for code := range iso2ToISO3 {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}
