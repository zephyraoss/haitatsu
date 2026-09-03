package config

import (
	"slices"
	"testing"
)

func TestInboundHostnamesDedupesAndNormalises(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{PublicHostname: "Haitatsu.Kessoku.zpr.ax"},
		TLS:    TLSConfig{Storage: TLSStorageConfig{Hostnames: []string{" mx.zephyra.email ", "haitatsu.kessoku.zpr.ax", ""}}},
	}
	got := cfg.InboundHostnames()
	want := []string{"haitatsu.kessoku.zpr.ax", "mx.zephyra.email"}
	if !slices.Equal(got, want) {
		t.Fatalf("InboundHostnames() = %v, want %v", got, want)
	}
}
