package importexport

import "strings"

func sensitiveSourceKey(key string) bool {
	switch strings.ToLower(key) {
	case "password", "secret", "token", "access_key", "secret_key":
		return true
	}
	return false
}

func RedactSource(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	redacted := make(map[string]any, len(source))
	for key, value := range source {
		if sensitiveSourceKey(key) {
			redacted[key] = "[redacted]"
			continue
		}
		redacted[key] = value
	}
	return redacted
}

func ScrubSource(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	scrubbed := make(map[string]any, len(source))
	for key, value := range source {
		if sensitiveSourceKey(key) {
			continue
		}
		scrubbed[key] = value
	}
	return scrubbed
}
