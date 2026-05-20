package routing

import "testing"

func TestSplitPlusTag(t *testing.T) {
	base, tag := splitPlusTag("alice+bank@example.com")
	if base != "alice@example.com" || tag != "bank" {
		t.Fatalf("unexpected split: %q %q", base, tag)
	}
}

func TestSplitPlusTagWithoutTag(t *testing.T) {
	base, tag := splitPlusTag("alice@example.com")
	if base != "alice@example.com" || tag != "" {
		t.Fatalf("unexpected split: %q %q", base, tag)
	}
}
