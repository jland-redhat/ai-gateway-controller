package storage

import "testing"

func TestSlugNorm(t *testing.T) {
	cases := map[string]string{
		"Grace Hopper":            "grace_hopper",
		"  ada lovelace ":         "ada_lovelace",
		"José García-López":       "jos_garc_a_l_pez", // accents fold to underscores, never dropped
		"42_widget":               "42_widget",
		"__padded__":              "padded",
		"Jean-Pierre O'Brien":     "jean_pierre_o_brien",
		"Already_Snake":           "already_snake",
		"trailing.dot@weird.com!": "trailing_dot_weird_com",
	}
	for in, want := range cases {
		if got := SlugNorm(in); got != want {
			t.Errorf("SlugNorm(%q) = %q, want %q", in, got, want)
		}
	}
}
