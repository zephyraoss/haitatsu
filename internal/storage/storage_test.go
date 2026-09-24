package storage

import (
	"context"
	"errors"
	"testing"
)

func TestValidateImportKey(t *testing.T) {
	valid := []string{"imports/archive.zip", "imports/mailbox-1/2024.zip"}
	for _, key := range valid {
		if err := ValidateImportKey(key); err != nil {
			t.Errorf("ValidateImportKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := []string{
		"",
		"imports/",
		"imports",
		"/imports/archive.zip",
		"exports/job-1",
		"messages/2024/01/01/id.eml",
		"imports/../exports/job-1",
		"imports/./archive.zip",
		"imports//archive.zip",
		"imports/a\\b.zip",
		"imports/a\x00.zip",
		"Imports/archive.zip",
	}
	for _, key := range invalid {
		if err := ValidateImportKey(key); !errors.Is(err, ErrInvalidImportKey) {
			t.Errorf("ValidateImportKey(%q) = %v, want ErrInvalidImportKey", key, err)
		}
	}
}

func TestGetObjectReaderRejectsKeysOutsideImportPrefix(t *testing.T) {
	client := &Client{bucket: "bucket"}
	if _, err := client.GetObjectReader(context.Background(), "exports/job-1"); !errors.Is(err, ErrInvalidImportKey) {
		t.Fatalf("err = %v, want ErrInvalidImportKey", err)
	}
}
