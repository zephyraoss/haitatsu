package importexport

import (
	"reflect"
	"testing"
)

func TestRedactSource(t *testing.T) {
	source := map[string]any{
		"addr":     "imap.example.com:993",
		"username": "user@example.com",
		"Password": "hunter2",
		"token":    "abc",
	}
	got := RedactSource(source)
	want := map[string]any{
		"addr":     "imap.example.com:993",
		"username": "user@example.com",
		"Password": "[redacted]",
		"token":    "[redacted]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RedactSource = %v, want %v", got, want)
	}
	if source["Password"] != "hunter2" {
		t.Error("RedactSource mutated its input")
	}
	if RedactSource(nil) != nil {
		t.Error("RedactSource(nil) should be nil")
	}
}

func TestScrubSource(t *testing.T) {
	source := map[string]any{
		"addr":     "imap.example.com:993",
		"username": "user@example.com",
		"password": "hunter2",
		"secret":   "s",
	}
	got := ScrubSource(source)
	want := map[string]any{
		"addr":     "imap.example.com:993",
		"username": "user@example.com",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScrubSource = %v, want %v", got, want)
	}
	if source["password"] != "hunter2" {
		t.Error("ScrubSource mutated its input")
	}
	if ScrubSource(nil) != nil {
		t.Error("ScrubSource(nil) should be nil")
	}
}
