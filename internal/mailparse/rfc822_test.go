package mailparse

import (
	"strings"
	"testing"
)

func TestSplitHeaderBodyMissingSeparator(t *testing.T) {
	raw := []byte("From: Sender <woof@example.com>\n" +
		"To: dog@mail.localhost\n" +
		"Subject: Hello from inbound test\n" +
		"Date: Thu, 21 May 2026 12:00:00 +0000\n" +
		"This is a test message.\n")

	header, body := SplitHeaderBody(raw)
	if !strings.Contains(string(header), "Subject: Hello from inbound test") {
		t.Fatalf("header missing subject: %q", header)
	}
	if string(body) != "This is a test message.\n" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestNormalizeMessageParsesMetadata(t *testing.T) {
	raw := []byte("Received: by mail.localhost\r\n" +
		"X-Haitatsu-Node: compose-test-01\r\n" +
		"From: Sender <woof@example.com>\n" +
		"To: dog@mail.localhost\n" +
		"Subject: Hello from inbound test\n" +
		"Date: Thu, 21 May 2026 12:00:00 +0000\n" +
		"This is a test message.\n")

	metadata := Parse(NormalizeMessage(raw))
	if metadata.Subject != "Hello from inbound test" {
		t.Fatalf("subject = %q", metadata.Subject)
	}
	if len(metadata.From) != 1 || metadata.From[0] != "woof@example.com" {
		t.Fatalf("from = %v", metadata.From)
	}
	if !strings.Contains(metadata.TextExtract, "This is a test message.") {
		t.Fatalf("text = %q", metadata.TextExtract)
	}
}

func TestSplitHeaderBodyBodyAfterSubject(t *testing.T) {
	raw := []byte("From: Sender <woof@example.com>\nSubject: Hello\nBody line\n")
	header, body := SplitHeaderBody(raw)
	if strings.Contains(string(header), "Body line") {
		t.Fatalf("header should not contain body: %q", header)
	}
	if string(body) != "Body line\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestToCRLFHeader(t *testing.T) {
	formatted := string(toCRLF([]byte("From: a@example.com\nSubject: Hello")))
	if formatted != "From: a@example.com\r\nSubject: Hello\r\n" {
		t.Fatalf("formatted = %q", formatted)
	}
}

func TestJoinHeaderBody(t *testing.T) {
	joined := string(JoinHeaderBody([]byte("From: a@example.com\nSubject: Hello"), []byte("Body line\n")))
	if !strings.Contains(joined, "Subject: Hello\r\n\r\nBody line") {
		t.Fatalf("joined = %q", joined)
	}
}
