package handler

import (
	"sort"
	"strings"
)

// technicalGroups are IdP plumbing that must never count as business
// authorization. The Stage realm currently ships these in the groups claim
// because the mapper is a User Realm Role mapper.
var technicalGroups = map[string]struct{}{
	"offline_access":    {},
	"uma_authorization": {},
	"account":           {},
}

// NormalizeGroups trims, drops technical roles, de-duplicates, and sorts.
func NormalizeGroups(groups []string) []string {
	seen := make(map[string]struct{}, len(groups))
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		lower := strings.ToLower(g)
		if _, technical := technicalGroups[lower]; technical {
			continue
		}
		if strings.HasPrefix(lower, "default-roles-") || strings.HasSuffix(lower, "_authorization") {
			continue
		}
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}
