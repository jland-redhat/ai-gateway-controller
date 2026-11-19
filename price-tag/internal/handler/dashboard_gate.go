package handler

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/dashboard"
	"github.com/redhat-et/pricetag-metering/internal/storage"
)

// DashboardCountryGate enforces the dashboard country policy.
//
// It wraps the session-authenticated routes (every route mounted behind
// RequireAuth) so the whole dashboard surface — pages and their backing JSON
// APIs — is gated in one place. Inference, key minting and ingestion never
// pass through it: keys keep working while the dashboard is region-gated.
//
// The gate fails open. A directory or policy read error is logged and the
// request is allowed: this policy is a staged rollout ("coming soon"), not a
// security boundary, and a database blip must not lock out every user.
type DashboardCountryGate struct {
	store *storage.Store
	cfg   config.Config
	page  []byte
	ttl   time.Duration
	// failTTL bounds how often a failing policy read is retried. Without it a
	// database incident turns every session request into another 5s query.
	failTTL time.Duration

	mu       sync.RWMutex
	blocked  map[string]struct{}
	loadedAt time.Time
	failAt   time.Time
	// flights collapses concurrent refreshes so one slow read does not fan out
	// into one query (and one lock hold) per request.
	flights singleflight.Group
}

// NewDashboardCountryGate builds the gate and loads the blocked-country page.
func NewDashboardCountryGate(store *storage.Store, cfg config.Config) *DashboardCountryGate {
	page, err := fs.ReadFile(dashboard.FS, "blocked.html")
	if err != nil {
		slog.Error("dashboard policy page missing", "error", err)
		page = []byte("<!doctype html><title>Coming soon</title><p>The dashboard is not available in your region yet. Coming soon.</p>")
	}
	return &DashboardCountryGate{
		store:   store,
		cfg:     cfg,
		page:    page,
		ttl:     30 * time.Second,
		failTTL: 5 * time.Second,
	}
}

// Invalidate drops the cached policy so an admin change applies immediately.
func (g *DashboardCountryGate) Invalidate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadedAt = time.Time{}
	g.failAt = time.Time{}
}

// gateExemptPath lists the session routes the gate must never deny.
//
// /admin/impersonate is the control that clears (or switches) the "view as"
// cookie. RequireAuth rewrites the identity header to the impersonated user,
// so a super-admin viewing a blocked person is evaluated as that person and
// would otherwise be locked out of the very route that returns them to their
// own account. The handler authorizes against the real session identity, so
// letting the request through cannot widen access. Impersonated dashboard
// views stay blocked on purpose: the operator sees exactly what the user sees.
func gateExemptPath(path string) bool {
	return path == "/admin/impersonate"
}

// blockedCountries returns the blocked-country set, refreshed at most once per
// ttl. Concurrent callers share one refresh; a failed refresh is cached for
// failTTL and serves the last good set (nil before the first success).
func (g *DashboardCountryGate) blockedCountries(ctx context.Context) map[string]struct{} {
	if g == nil || g.store == nil {
		return nil
	}
	if blocked, fresh := g.snapshot(); fresh {
		return blocked
	}
	value, _, _ := g.flights.Do("policy", func() (any, error) {
		// A queued caller may find the refresh already done.
		if blocked, fresh := g.snapshot(); fresh {
			return blocked, nil
		}
		// Detach from the first caller's context: a disconnected browser must
		// not fail the shared refresh for everyone else.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		countries, err := g.store.DashboardBlockedCountries(readCtx)
		if err != nil {
			slog.Error("dashboard country policy unavailable — allowing request", "error", err)
			g.mu.Lock()
			g.failAt = time.Now()
			last := g.blocked
			g.mu.Unlock()
			return last, nil
		}
		normalized := storage.NormalizeDashboardCountries(countries)
		g.mu.Lock()
		g.blocked = normalized
		g.loadedAt = time.Now()
		g.failAt = time.Time{}
		g.mu.Unlock()
		return normalized, nil
	})
	if blocked, ok := value.(map[string]struct{}); ok {
		return blocked
	}
	return nil
}

// snapshot reports the cached set and whether it is still usable (fresh, or
// inside the failure backoff). It never performs I/O and holds the lock only
// long enough to copy the two timestamps.
func (g *DashboardCountryGate) snapshot() (map[string]struct{}, bool) {
	now := time.Now()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.loadedAt.IsZero() && now.Sub(g.loadedAt) < g.ttl {
		return g.blocked, true
	}
	if !g.failAt.IsZero() && now.Sub(g.failAt) < g.failTTL {
		// Serve the last good set during backoff (nil before the first success,
		// which the caller reads as "no country blocked" — fail open).
		return g.blocked, true
	}
	return nil, false
}

// Wrap gates one session-authenticated handler.
func (g *DashboardCountryGate) Wrap(next http.HandlerFunc) http.HandlerFunc {
	if g == nil || g.store == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if gateExemptPath(r.URL.Path) {
			next(w, r)
			return
		}
		// RequireAuth sets the identity header before this runs. Requests
		// without it are not session requests; the auth layer decides.
		user := strings.TrimSpace(r.Header.Get(g.cfg.UserHeader))
		if user == "" {
			next(w, r)
			return
		}
		blockedSet := g.blockedCountries(r.Context())
		if len(blockedSet) == 0 {
			// No country is blocked: skip the per-user lookups entirely.
			next(w, r)
			return
		}
		profile, err := g.store.PartnerUserByMaaSUsername(r.Context(), user)
		if err != nil {
			// The header carries the caller identity; never log it (CodeQL:
			// clear-text logging of sensitive information).
			slog.Warn("dashboard country check skipped: directory lookup failed", "error", err)
			next(w, r)
			return
		}
		country := storage.DashboardCountryOf(profile.Tags)
		if country == "" {
			// No country on file is allowed: the policy only blocks what the
			// directory explicitly places in a blocked region.
			next(w, r)
			return
		}
		if _, blocked := blockedSet[country]; !blocked {
			next(w, r)
			return
		}
		// Super-admins are exempt: they own the policy and must never be able
		// to lock themselves (and the policy tab) out of the dashboard. This
		// runs only for identities the policy would otherwise deny.
		if IsPartnerSuperAdmin(r.Context(), g.cfg, g.store, r) {
			next(w, r)
			return
		}
		g.deny(w, r)
	}
}

// deny answers a blocked identity. Browser navigations get the "coming soon"
// page; the dashboard's own JSON calls get a machine-readable 403 so the UI can
// render the same message without a redirect loop.
func (g *DashboardCountryGate) deny(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	api := strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.HasPrefix(r.URL.Path, "/me/") ||
		(r.Method != http.MethodGet && r.Method != http.MethodHead)
	if api {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"dashboard_unavailable_region","message":"The dashboard is not available in your region yet. Coming soon."}`)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(g.page)
}
