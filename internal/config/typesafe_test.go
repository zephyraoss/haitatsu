package config

import "testing"

func TestTypeSafeDefaultsAndOffByDefault(t *testing.T) {
	cfg := validConfig()
	if cfg.Spam.TypeSafe.Enabled() {
		t.Fatal("missing typesafe block should default to off")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy config failed validation: %v", err)
	}
	defaults := TypeSafeConfig{}.WithDefaults()
	if defaults.Mode != "off" || defaults.Model != "jev-latest" || defaults.TimeoutMS != 2000 || defaults.MaxTextBytes != 16384 || defaults.MaxInFlight != 8 || defaults.MaxRequestsPerMinute != 60 || defaults.SpamThreshold != 0.98 || defaults.PhishingThreshold != 0.98 {
		t.Fatalf("defaults = %+v", defaults)
	}
}

func TestTypeSafeValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(*Config)
		valid bool
	}{
		{name: "shadow", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "shadow", APIKey: "k"} }, valid: true},
		{name: "junk", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk", APIKey: "k", TimeoutMS: 500} }, valid: true},
		{name: "bad mode", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "on", APIKey: "k"} }},
		{name: "missing key", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk"} }},
		{name: "timeout too low", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk", APIKey: "k", TimeoutMS: 50} }},
		{name: "timeout too high", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk", APIKey: "k", TimeoutMS: 20000} }},
		{name: "threshold above one", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk", APIKey: "k", SpamThreshold: 1.5} }},
		{name: "negative limit", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "junk", APIKey: "k", MaxInFlight: -1} }},
		{name: "off ignores fields", patch: func(c *Config) { c.Spam.TypeSafe = TypeSafeConfig{Mode: "off", SpamThreshold: 9} }, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.patch(&cfg)
			err := cfg.Validate()
			if tc.valid && err != nil {
				t.Fatalf("expected valid: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
