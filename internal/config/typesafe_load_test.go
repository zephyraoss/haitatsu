package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypeSafePklLoadingAndReload(t *testing.T) {
	if _, err := exec.LookPath("pkl"); err != nil {
		t.Skip("pkl binary not on PATH")
	}
	t.Setenv("HAITATSU_TYPESAFE_API_KEY", "test-environment-key")
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "haitatsu.compose.pkl"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "  typesafe {")
	if start < 0 {
		t.Fatal("example is missing typesafe block")
	}
	end := strings.Index(source[start:], "\n  }") + start + len("\n  }")
	if end < start {
		t.Fatal("malformed example block")
	}
	legacy := source[:start] + source[end:]
	enabled := source[:start] + strings.Replace(source[start:end], `mode = "off"`, `mode = "shadow"`, 1) + source[end:]
	load := func(name, body string) *Config {
		path := filepath.Join(t.TempDir(), name+".pkl")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	old, next := load("legacy", legacy), load("enabled", enabled)
	if old.Spam.TypeSafe.WithDefaults().Mode != "off" {
		t.Fatal("legacy config enabled integration")
	}
	if next.Spam.TypeSafe.APIKey != "test-environment-key" || next.Spam.TypeSafe.Mode != "shadow" {
		t.Fatal("environment/mode not loaded")
	}
	if old.ReloadImpact(next).RequiresRestart() {
		t.Fatal("enabling typesafe should not require restart")
	}
	rotated := *next
	rotated.Spam.TypeSafe.APIKey = "rotated"
	rotated.Spam.TypeSafe.Model = "new-model"
	rotated.Spam.TypeSafe.MaxInFlight = 2
	if next.ReloadImpact(&rotated).RequiresRestart() {
		t.Fatal("runtime changes require restart")
	}
}
