package main

import (
	"os"
	"strings"
	"testing"
)

// TestManagerReadRoutesUseAuthorizedCache guards the rebase-sensitive wiring:
// the cache wrapper must be applied in main, not only exercised in a handler
// unit test. A route that loses Cached still passes handler tests but silently
// gives up the manager-page speedup.
func TestManagerReadRoutesUseAuthorizedCache(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	want := map[string]string{
		`"/api/v1/org/usage"`:  "managerAuth(orgHandler.Cached(orgHandler.HandleOrgUsage))",
		`"/api/v1/org/charts"`: "managerAuth(orgHandler.Cached(orgHandler.HandleOrgCharts))",
	}
	for route, call := range want {
		found := false
		for _, line := range lines {
			if strings.Contains(line, route) {
				found = true
				if !strings.Contains(line, call) {
					t.Errorf("%s is not wrapped in the authorized cache: %s", route, strings.TrimSpace(line))
				}
			}
		}
		if !found {
			t.Errorf("route %s not found in main.go", route)
		}
	}
}
