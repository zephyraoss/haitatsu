package api

import "testing"

func TestNormalizeAddress(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		" User@Example.COM ":       {"user@example.com", true},
		"user@example.com":         {"user@example.com", true},
		"User <user@example.com>":  {"", false},
		"not-an-address":           {"", false},
		"a@b@example.com":          {"", false},
		"user@example.com, x@y.co": {"", false},
	}
	for input, want := range cases {
		got, ok := normalizeAddress(input)
		if ok != want.ok || (ok && got != want.want) {
			t.Errorf("normalizeAddress(%q) = %q, %v; want %q, %v", input, got, ok, want.want, want.ok)
		}
	}
}
