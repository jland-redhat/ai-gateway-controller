package handler

import "testing"

func TestNormalizeCountry(t *testing.T) {
	tests := map[string]string{
		"us":   "USA",
		"USA":  "USA",
		" gb ": "GBR",
		"GBR":  "GBR",
		"ZZZ":  "",
		"XYZ":  "",
		"":     "",
		"U":    "",
		"unk":  "",
	}
	for input, want := range tests {
		if got := normalizeCountry(input); got != want {
			t.Errorf("normalizeCountry(%q) = %q, want %q", input, got, want)
		}
	}
}
