package handler

import (
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/dashboard"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

type PartnerOrgHandler struct {
	store *storage.Store
	cfg   config.Config
	cache *orgResponseCache
}

func NewPartnerOrgHandler(store *storage.Store, cfg config.Config) *PartnerOrgHandler {
	return &PartnerOrgHandler{store: store, cfg: cfg, cache: newOrgResponseCache(cfg)}
}

// Cached wraps a manager read in the short-lived, per-caller response cache.
// The wrapper resolves scopeRoot (the actual authorization gate) before every
// lookup, so a hit never outlives a revoked role or subtree.
func (h *PartnerOrgHandler) Cached(next http.HandlerFunc) http.HandlerFunc {
	return h.cache.wrap(next, h.scopeRoot)
}

// InvalidateCache drops cached manager responses. The dashboard country policy
// calls it after a change so hidden rows and inclusive totals apply at once.
func (h *PartnerOrgHandler) InvalidateCache() {
	if h == nil {
		return
	}
	h.cache.InvalidateAll()
}

// resolvedScope reuses the cache wrapper's authorization result when present,
// otherwise performs the normal check (cache disabled or uncached route).
func (h *PartnerOrgHandler) resolvedScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	if root, ok := orgScopeFromRequest(r); ok {
		return root, true
	}
	return h.scopeRoot(w, r)
}

// ServeManager serves the manager.html page from the embedded dashboard FS.
func (h *PartnerOrgHandler) ServeManager(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(dashboard.FS, "manager.html")
	if err != nil {
		http.Error(w, "manager page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// scopeResponse is the JSON envelope for HandleScope.
type scopeResponse struct {
	Username     string   `json:"username"`
	Scope        string   `json:"scope"`
	IsAdmin      bool     `json:"isAdmin"`
	IsSuperAdmin bool     `json:"isSuperAdmin"`
	IsManager    bool     `json:"isManager"`
	ScopeSize    int      `json:"scopeSize"`
	Roots        []string `json:"roots"`
	PersonSlug   string   `json:"personSlug"`
}

// HandleScope reports the caller's authorization scope so the manager page
// can decide which UI surface to render (self / manager / admin).
func (h *PartnerOrgHandler) HandleScope(w http.ResponseWriter, r *http.Request) {
	me := caller(r, h.cfg)
	if me == "" {
		http.Error(w, "who are you?", http.StatusUnauthorized)
		return
	}

	resp := scopeResponse{
		Username: me,
		Roots:    []string{},
	}

	if IsPartnerSuperAdmin(r.Context(), h.cfg, h.store, r) {
		resp.Scope = "admin"
		resp.IsAdmin = true
		resp.IsSuperAdmin = true
		writeJSON(w, resp)
		return
	}

	pu, puErr := h.store.PartnerUserByMaaSUsername(r.Context(), me)
	list, isMgr, mgrErr := h.store.PartnerManagerScope(r.Context(), me)
	if mgrErr != nil {
		slog.Error("partner manager scope failed", "user", me, "error", mgrErr)
	}

	resp.IsAdmin = IsPartnerAdmin(r.Context(), h.cfg, h.store, r)

	if isMgr {
		resp.Scope = "manager"
		resp.IsManager = true
		resp.ScopeSize = len(list)
		if puErr == nil {
			resp.PersonSlug = pu.UserID
		}
		writeJSON(w, resp)
		return
	}

	resp.Scope = "self"
	resp.ScopeSize = len(list)
	if puErr == nil {
		resp.PersonSlug = pu.UserID
	}
	writeJSON(w, resp)
}

// scopeRoot resolves the ?root= query parameter into a partner user_id that
// the caller is allowed to view. Super-admins may pass any root or leave it
// empty (whole org). Everyone else defaults to their own partner user_id; a
// non-empty root that differs from their own must pass PartnerInSubtree.
// Returns the resolved root and true, or writes an error response and returns
// false.
func (h *PartnerOrgHandler) scopeRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	me := caller(r, h.cfg)
	if me == "" {
		http.Error(w, "who are you?", http.StatusUnauthorized)
		return "", false
	}

	root := strings.TrimSpace(r.URL.Query().Get("root"))

	if IsPartnerSuperAdmin(r.Context(), h.cfg, h.store, r) {
		return root, true
	}

	pu, err := h.store.PartnerUserByMaaSUsername(r.Context(), me)
	if err != nil {
		if errors.Is(err, storage.ErrPartnerUserNotFound) {
			http.Error(w, "no partner identity", http.StatusForbidden)
			return "", false
		}
		slog.Error("partner user lookup failed", "user", me, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}

	myRoot := pu.UserID
	if root == "" || root == myRoot {
		return myRoot, true
	}

	ok, err := h.store.PartnerInSubtree(r.Context(), myRoot, root)
	if err != nil {
		slog.Error("subtree check failed", "user", me, "root", root, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return "", false
	}
	if !ok {
		http.Error(w, "root outside your visibility scope", http.StatusForbidden)
		return "", false
	}
	return root, true
}

// orgTotalsResponse is the server-side total for a manager's subtree. It is
// computed over every person in scope, including the rows the country policy
// hides from the table, so the page's totals never shrink when a region is
// blocked. BlockedPeople is what the table note reports.
type orgTotalsResponse struct {
	People        int     `json:"people"`
	WithKey       int     `json:"with_key"`
	BlockedPeople int     `json:"blocked_people"`
	Requests      int     `json:"requests"`
	TotalTokens   int64   `json:"total_tokens"`
	CostUSD       float64 `json:"cost_usd"`
	QuotaUSD      float64 `json:"quota_usd"`
	QuotaSpentUSD float64 `json:"quota_spent_usd"`
	OverLimit     int     `json:"over_limit"`
}

// orgUsageResponse is one manager read: the rows for the table plus the
// inclusive subtree totals. They ship together because both need the same
// GetPartnerOrgUsage result — splitting them into two endpoints ran the most
// expensive manager query twice per page load.
type orgUsageResponse struct {
	Rows   []storage.OrgUsageRow `json:"rows"`
	Totals orgTotalsResponse     `json:"totals"`
}

// HandleOrgUsage returns per-person usage rollups for the manager table and
// the inclusive subtree totals in one pass.
// GET /api/v1/org/usage?range=&root=
func (h *PartnerOrgHandler) HandleOrgUsage(w http.ResponseWriter, r *http.Request) {
	root, ok := h.resolvedScope(w, r)
	if !ok {
		return
	}
	since, until := parseTimeWindow(r)
	rows, err := h.store.GetPartnerOrgUsage(r.Context(), root, since, until)
	if err != nil {
		slog.Error("org usage query failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []storage.OrgUsageRow{}
	}
	superAdmin := IsPartnerSuperAdmin(r.Context(), h.cfg, h.store, r)
	// Totals cover the whole subtree, including blocked people.
	totals := orgTotals(rows, superAdmin)
	// Dashboard country policy: managers do not see people from blocked
	// regions in their tables; their usage stays in the totals and charts.
	// Super-admins see everyone: they own the policy and need the directory.
	visible := rows
	if !superAdmin {
		visible = make([]storage.OrgUsageRow, 0, len(rows))
		for _, row := range rows {
			if !row.Blocked {
				visible = append(visible, row)
			}
		}
	}
	writeJSON(w, orgUsageResponse{Rows: visible, Totals: totals})
}

// orgTotals sums the full (unfiltered) row set. An empty result must not
// marshal as null, so Rows is normalised by the caller.
func orgTotals(rows []storage.OrgUsageRow, superAdmin bool) orgTotalsResponse {
	out := orgTotalsResponse{People: len(rows)}
	for _, row := range rows {
		if row.Username != "" {
			out.WithKey++
		}
		out.Requests += row.Requests
		out.TotalTokens += row.TotalTokens
		out.CostUSD += row.CostUSD
		out.QuotaUSD += row.QuotaUSD
		out.QuotaSpentUSD += row.QuotaSpentUSD
		if row.OverLimit {
			out.OverLimit++
		}
		if row.Blocked {
			out.BlockedPeople++
		}
	}
	if superAdmin {
		// Super-admins see the blocked rows too, so nothing is hidden from
		// them and the manager-page note would be misleading.
		out.BlockedPeople = 0
	}
	out.CostUSD = math.Round(out.CostUSD*100) / 100
	out.QuotaUSD = math.Round(out.QuotaUSD*100) / 100
	out.QuotaSpentUSD = math.Round(out.QuotaSpentUSD*100) / 100
	return out
}

// orgChartsResponse bundles the analytics data the manager charts need in a
// single round-trip.
type orgChartsResponse struct {
	Overview storage.DashboardOverview `json:"overview"`
	Models   []storage.ModelSummary    `json:"models"`
	Users    []storage.UserSummary     `json:"users"`
	Timeline []storage.TimelineBucket  `json:"timeline"`
}

// orgChartsUserFilter turns a resolved root into the dashboard user filter.
// A non-empty root with no logins must not collapse to an empty filter: the
// dashboard queries interpret "" as whole-organisation, which would leak data
// outside the selected person's scope. The bool is false when charts must be
// empty instead.
func orgChartsUserFilter(root string, usernames []string) (string, bool) {
	if root != "" && len(usernames) == 0 {
		return "", false
	}
	return strings.Join(usernames, ","), true
}

func emptyOrgChartsResponse() orgChartsResponse {
	return orgChartsResponse{
		Overview: storage.DashboardOverview{},
		Models:   []storage.ModelSummary{},
		Users:    []storage.UserSummary{},
		Timeline: []storage.TimelineBucket{},
	}
}

// HandleOrgCharts returns the analytics bundle for the manager page charts.
// GET /api/v1/org/charts?range=&root=&ref=
func (h *PartnerOrgHandler) HandleOrgCharts(w http.ResponseWriter, r *http.Request) {
	root, ok := h.resolvedScope(w, r)
	if !ok {
		return
	}
	since, until := parseTimeWindow(r)
	ctx := r.Context()

	usernames, err := h.store.PartnerSubtreeUsernames(ctx, root)
	if err != nil {
		slog.Error("subtree usernames failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Join usernames with "," for the dashboard user filter; nil (whole org)
	// becomes "" which the dashboard queries treat as unfiltered.
	userFilter, ok := orgChartsUserFilter(root, usernames)
	if !ok {
		writeJSON(w, emptyOrgChartsResponse())
		return
	}
	refModel := r.URL.Query().Get("ref")

	overview, err := h.store.GetDashboardOverview(ctx, since, until, "", userFilter, "")
	if err != nil {
		slog.Error("org charts overview failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	saved, ratio, applied, serr := h.store.GetHostedSavings(ctx, since, until, "", userFilter, "", refModel)
	if serr != nil {
		slog.Error("org charts savings failed", "error", serr)
	} else {
		overview.SavedUSD, overview.SavingsRatio, overview.RatioApplied = saved, ratio, applied
	}

	models, err := h.store.GetDashboardModels(ctx, since, until, "", userFilter, "")
	if err != nil {
		slog.Error("org charts models failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []storage.ModelSummary{}
	}

	users, err := h.store.GetDashboardUsers(ctx, since, until, "", userFilter, "", "", "", 200, 0, refModel, "", true)
	if err != nil {
		slog.Error("org charts users failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if users == nil {
		users = []storage.UserSummary{}
	}
	// Dashboard country policy: a manager must not see blocked people named in
	// the user pie, but their spend must stay in the slice total. Fold them
	// into one labelled bucket instead of dropping them (which would shrink
	// the pie). Super-admins see the full breakdown.
	if !IsPartnerSuperAdmin(r.Context(), h.cfg, h.store, r) {
		blockedUsernames, blockErr := h.store.DashboardBlockedUsernamesInSubtree(ctx, root)
		if blockErr != nil {
			slog.Error("org charts country policy failed", "error", blockErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if len(blockedUsernames) > 0 {
			kept := make([]storage.UserSummary, 0, len(users))
			var hidden storage.UserSummary
			hiddenUsers := 0
			for _, user := range users {
				if _, blocked := blockedUsernames[user.Username]; !blocked {
					kept = append(kept, user)
					continue
				}
				hiddenUsers++
				hidden.Requests += user.Requests
				hidden.PromptTokens += user.PromptTokens
				hidden.CompletionTokens += user.CompletionTokens
				hidden.TotalTokens += user.TotalTokens
				hidden.CostUSD += user.CostUSD
				hidden.SavedUSD += user.SavedUSD
			}
			if hiddenUsers > 0 {
				hidden.DisplayName = "Hidden by dashboard policy"
				hidden.CostUSD = math.Round(hidden.CostUSD*100) / 100
				hidden.SavedUSD = math.Round(hidden.SavedUSD*100) / 100
				kept = append(kept, hidden)
			}
			users = kept
		}
	}

	timeline, err := h.store.GetDashboardTimeline(ctx, since, until, "", userFilter, "", "model")
	if err != nil {
		slog.Error("org charts timeline failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if timeline == nil {
		timeline = []storage.TimelineBucket{}
	}

	writeJSON(w, orgChartsResponse{
		Overview: overview,
		Models:   models,
		Users:    users,
		Timeline: timeline,
	})
}

// orgPersonResponse is the per-person drill-down payload.
type orgPersonResponse struct {
	FullName  string                      `json:"full_name"`
	Usernames []string                    `json:"usernames"`
	Models    []storage.OrgPersonModelRow `json:"models"`
}

// HandleOrgPerson returns the per-person model breakdown for the manager
// drill-down. The ?slug= parameter is a partner user_id.
// GET /api/v1/org/person?slug=&range=&ref=
func (h *PartnerOrgHandler) HandleOrgPerson(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Error(w, "slug parameter required", http.StatusBadRequest)
		return
	}

	// Authorize: the caller must be able to see this person.
	root, ok := h.resolvedScope(w, r)
	if !ok {
		return
	}
	// When root is non-empty, verify the target is within the subtree.
	if root != "" {
		inTree, err := h.store.PartnerInSubtree(r.Context(), root, slug)
		if err != nil {
			slog.Error("person subtree check failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !inTree {
			http.Error(w, "person outside your visibility scope", http.StatusForbidden)
			return
		}
	}
	// The country policy hides blocked people from manager tables; a direct
	// drill-down must not become a side door into their usage. Super-admins
	// are exempt for the same reason the tables are.
	if !IsPartnerSuperAdmin(r.Context(), h.cfg, h.store, r) {
		blocked, blockErr := h.store.PartnerUserCountryBlocked(r.Context(), slug)
		if blockErr != nil {
			slog.Error("person country policy check failed", "slug", slug, "error", blockErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if blocked {
			http.Error(w, "person outside your visibility scope", http.StatusForbidden)
			return
		}
	}

	ctx := r.Context()
	fullName, usernames, err := h.store.PartnerPersonDetail(ctx, slug)
	if err != nil {
		slog.Error("person detail failed", "slug", slug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	since, until := parseTimeWindow(r)
	refModel := r.URL.Query().Get("ref")

	models, err := h.store.GetPersonModelUsage(ctx, usernames, since, until, refModel)
	if err != nil {
		slog.Error("person model usage failed", "slug", slug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []storage.OrgPersonModelRow{}
	}

	writeJSON(w, orgPersonResponse{
		FullName:  fullName,
		Usernames: usernames,
		Models:    models,
	})
}
