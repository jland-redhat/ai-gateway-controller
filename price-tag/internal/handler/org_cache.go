package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/redhat-et/pricetag-metering/internal/config"
	"github.com/redhat-et/pricetag-metering/internal/metrics"
)

// orgCacheMaxBodyBytes bounds one cached manager response. /org/usage can be
// org-wide and is the largest manager payload; 2 MiB leaves ample headroom
// while preventing 256 unbounded bodies from pinning memory. Larger responses
// still stream to the caller, they are just not stored.
const orgCacheMaxBodyBytes = 2 << 20

// Manager-page response cache.
//
// /api/v1/org/usage and /api/v1/org/charts are comparatively expensive
// (rollup aggregation plus directory joins) and are hit again whenever a
// manager changes range, opens a person, or navigates back. They are not
// polled, so a short TTL is enough; the point is to collapse duplicate
// computations, not to bound a 30s fan-out like DashboardCache.
//
// Keying: the resolved scope root (the result of scopeRoot, including the
// super-admin bypass decision) plus caller identity, method, path and raw
// query. Two callers can never share an entry, and a caller whose role or
// subtree changed cannot reuse the old entry.
//
// Authorization runs BEFORE the cache lookup on every request. Session auth
// (managerAuth) is not enough: scopeRoot enforces the manager/super-admin
// subtree rules, so a hit must never skip it. The resolved root is handed to
// the handler through the context so the authorization lookup runs once.
type orgResponseCache struct {
	cfg     config.Config
	entries *expirable.LRU[string, cachedResponse]
	flights singleflight.Group
	enabled bool
}

func newOrgResponseCache(cfg config.Config) *orgResponseCache {
	if !cfg.DashboardCacheEnabled {
		return &orgResponseCache{}
	}
	ttl := time.Duration(cfg.DashboardCacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &orgResponseCache{
		cfg:     cfg,
		entries: expirable.NewLRU[string, cachedResponse](256, nil, ttl),
		enabled: true,
	}
}

// orgScopeContextKey carries the already-authorized root from the cache
// wrapper into the handler. Handlers fall back to scopeRoot when it is absent
// (cache disabled or a non-cached route).
type orgScopeContextKey struct{}

func orgScopeFromRequest(r *http.Request) (string, bool) {
	root, ok := r.Context().Value(orgScopeContextKey{}).(string)
	return root, ok
}

// InvalidateAll drops every cached manager response. The dashboard country
// policy calls it on a change so hidden rows/totals apply immediately instead
// of waiting for the entry's TTL.
func (c *orgResponseCache) InvalidateAll() {
	if c == nil || !c.enabled || c.entries == nil {
		return
	}
	c.entries.Purge()
}

func (c *orgResponseCache) wrap(next http.HandlerFunc, resolve func(http.ResponseWriter, *http.Request) (string, bool)) http.HandlerFunc {
	if c == nil || !c.enabled {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Authorize first, unconditionally. A cache hit must never outlive a
		// revoked role or subtree, and a rejected caller must not reach the
		// entries at all.
		root, ok := resolve(w, r)
		if !ok {
			return
		}
		// Hand the verified root to the handler so it does not repeat the
		// directory/subtree checks on the computation path.
		if bypassDashboardCache(r) {
			metrics.OrgCacheSkipsTotal.Inc()
			next(w, r.WithContext(context.WithValue(r.Context(), orgScopeContextKey{}, root)))
			return
		}
		identity := caller(r, c.cfg)
		if identity == "" {
			// scopeRoot rejects this too, but keep the guard: never build a
			// shared key from an empty identity.
			next(w, r.WithContext(context.WithValue(r.Context(), orgScopeContextKey{}, root)))
			return
		}
		key := orgCacheKey(identity, root, r)
		if entry, found := c.entries.Get(key); found {
			metrics.OrgCacheHitsTotal.Inc()
			writeCached(w, entry)
			return
		}
		v, _, _ := c.flights.Do(key, func() (any, error) {
			if entry, found := c.entries.Get(key); found {
				metrics.OrgCacheHitsTotal.Inc()
				return entry, nil
			}
			metrics.OrgCacheMissesTotal.Inc()
			inner := &capture{header: make(http.Header)}
			// Do not let the first caller's disconnect cancel a computation
			// other waiters are riding. The verified root is already in the
			// context and survives WithoutCancel.
			ctx := context.WithValue(context.WithoutCancel(r.Context()), orgScopeContextKey{}, root)
			next.ServeHTTP(inner, r.WithContext(ctx))
			if inner.status == http.StatusOK && inner.body.Len() <= orgCacheMaxBodyBytes {
				entry := cachedResponse{
					status:      inner.status,
					contentType: inner.header.Get("Content-Type"),
					body:        append([]byte(nil), inner.body.Bytes()...),
				}
				c.entries.Add(key, entry)
				return entry, nil
			}
			if inner.status == http.StatusOK {
				metrics.OrgCacheSkipsTotal.Inc()
			}
			return responseOnly{rec: inner}, nil
		})
		switch res := v.(type) {
		case cachedResponse:
			writeCached(w, res)
		case responseOnly:
			res.rec.replayTo(w)
		}
	}
}

func orgCacheKey(identity, root string, r *http.Request) string {
	h := sha256.New()
	h.Write([]byte(identity))
	h.Write([]byte("\n"))
	h.Write([]byte(root))
	h.Write([]byte("\n"))
	h.Write([]byte(r.Method))
	h.Write([]byte("\n"))
	h.Write([]byte(r.URL.Path))
	h.Write([]byte("\n"))
	h.Write([]byte(r.URL.RawQuery))
	return hex.EncodeToString(h.Sum(nil))
}
