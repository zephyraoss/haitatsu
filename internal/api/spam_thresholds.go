package api

import (
	"encoding/json"
	"fmt"
	"math"
)

func parseSpamThresholds(raw json.RawMessage) (map[string]float64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Pointers distinguish a null member from an explicit numeric zero.
	var fields map[string]*float64
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("spam_thresholds must be an object of numbers or null")
	}
	values := make(map[string]float64, len(fields))
	for key, value := range fields {
		switch key {
		case "junk_threshold", "spam_threshold", "phishing_threshold":
		default:
			return nil, fmt.Errorf("unknown spam threshold %q", key)
		}
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return nil, fmt.Errorf("spam threshold %q must be a finite number", key)
		}
		if key == "junk_threshold" {
			if *value < 0 {
				return nil, fmt.Errorf("junk_threshold must be >= 0")
			}
		} else if *value <= 0 || *value > 1 {
			return nil, fmt.Errorf("%s must be greater than 0 and at most 1", key)
		}
		values[key] = *value
	}
	return values, nil
}
