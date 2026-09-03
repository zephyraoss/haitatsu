package api

import "testing"

func TestMXTargetsInboundHost(t *testing.T) {
	inbound := []string{"haitatsu.kessoku.zpr.ax", "mx.zephyra.email"}
	cases := map[string]bool{
		"mx.zephyra.email.":        true,
		"MX.Zephyra.Email":         true,
		"haitatsu.kessoku.zpr.ax.": true,
		"mail.zephyra.email.":      false,
		"":                         false,
	}
	for target, want := range cases {
		if got := mxTargetsInboundHost(target, inbound); got != want {
			t.Errorf("mxTargetsInboundHost(%q) = %v, want %v", target, got, want)
		}
	}
}
