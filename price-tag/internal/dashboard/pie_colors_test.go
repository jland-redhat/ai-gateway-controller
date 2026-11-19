package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestPiePalettesHaveUniqueColors(t *testing.T) {
	files := []string{"account.html", "dashboard.html", "manager.html", "user-dashboard.html"}
	paletteRE := regexp.MustCompile(`(?m)(?:const|var) (?:COLORS|PIE_COLORS)\s*=\s*\[([^\]]+)\]`)
	hexRE := regexp.MustCompile(`'#[0-9a-fA-F]{6}'`)

	for _, file := range files {
		data, err := FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m := paletteRE.FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s: pie palette not found", file)
		}
		colors := hexRE.FindAllString(string(m[1]), -1)
		if len(colors) < 10 {
			t.Errorf("%s: palette has %d colors, want at least 10 distinct hues", file, len(colors))
		}
		seen := map[string]bool{}
		for _, c := range colors {
			c = strings.ToLower(c)
			if seen[c] {
				t.Errorf("%s: duplicate pie color %s", file, c)
			}
			seen[c] = true
		}
	}
}

func TestManagerPieDoesNotCollapseHostedModels(t *testing.T) {
	data, err := FS.ReadFile("manager.html")
	if err != nil {
		t.Fatal(err)
	}
	// Hosted models used to share one green slice; every model slice must
	// come from the categorical palette instead.
	if strings.Contains(string(data), "d.hosted?'#3fb950'") {
		t.Error("manager.html collapses all hosted models into one color")
	}
}

func TestToolPieColorsAreUnique(t *testing.T) {
	data, err := FS.ReadFile("dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	lineRE := regexp.MustCompile(`(?m)^\s*return \{'Claude Code'.*;$`)
	line := lineRE.Find(data)
	if line == nil {
		t.Fatal("dashboard.html: tool color map not found")
	}
	hexRE := regexp.MustCompile(`'#[0-9a-fA-F]{6}'|OTHER_SLICE_COLOR`)
	seen := map[string]bool{}
	for _, c := range hexRE.FindAllString(string(line), -1) {
		c = strings.ToLower(c)
		if seen[c] {
			t.Errorf("dashboard.html: duplicate tool pie color %s", c)
		}
		seen[c] = true
	}
}
