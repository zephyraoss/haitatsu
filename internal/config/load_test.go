package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsSecretsFromEnvironment(t *testing.T) {
	if _, err := exec.LookPath("pkl"); err != nil {
		t.Skip("pkl binary not on PATH")
	}
	t.Setenv("HAITATSU_TEST_TOKEN", "from-env")
	t.Setenv("HOSTNAME", "replica-1")
	root := repoRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "haitatsu.compose.pkl"))
	if err != nil {
		t.Fatal(err)
	}
	module := strings.Replace(string(source), `service_token = "woof"`, `service_token = read("env:HAITATSU_TEST_TOKEN")`, 1)
	module = strings.Replace(module, `instance_name = "compose-test-01"`, `instance_name = read("env:HOSTNAME")`, 1)
	path := filepath.Join(t.TempDir(), "haitatsu.pkl")
	if err := os.WriteFile(path, []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.ServiceToken != "from-env" {
		t.Fatalf("service token = %q, want from-env", cfg.API.ServiceToken)
	}
	if cfg.Server.InstanceName != "replica-1" {
		t.Fatalf("instance name = %q, want replica-1", cfg.Server.InstanceName)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
