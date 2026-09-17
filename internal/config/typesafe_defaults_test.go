package config

import (
	"math"
	"testing"
)

func TestTypeSafeDefaultsPreserveInvalidValues(t *testing.T) {
	c := TypeSafeConfig{TimeoutMS: -1, MaxTextBytes: -1, MaxInFlight: -1, MaxRequestsPerMinute: -1, SpamThreshold: 2, PhishingThreshold: -1}.WithDefaults()
	if c.TimeoutMS != -1 || c.MaxTextBytes != -1 || c.MaxInFlight != -1 || c.MaxRequestsPerMinute != -1 || c.SpamThreshold != 2 || c.PhishingThreshold != -1 {
		t.Fatal("defaults concealed invalid settings")
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := validConfig()
		c.Spam.TypeSafe = TypeSafeConfig{Mode: "shadow", APIKey: "test", SpamThreshold: value}
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted non-finite threshold %v", value)
		}
	}
}
