package api

import (
	"bytes"
	"testing"
)

func TestMessageBytesRejectsControlCharactersInSubject(t *testing.T) {
	for _, subject := range []string{"Hi\nBcc: x@evil.test", "Hi\rBcc: x@evil.test", "Hi\r\nBcc: x@evil.test", "Hi\x00"} {
		req := outboundMessageRequest{From: "a@example.test", To: []string{"b@example.test"}, Subject: subject}
		if _, err := req.messageBytes(); err == nil {
			t.Fatalf("expected error for subject %q", subject)
		}
	}
}

func TestWriteHeaderNeutralizesBareLineBreaks(t *testing.T) {
	var buf bytes.Buffer
	writeHeader(&buf, "Subject", "Hi\nBcc: x@evil.test\rX-Inj: 1\r\nX-Other: 2")
	if got, want := buf.String(), "Subject: Hi Bcc: x@evil.test X-Inj: 1  X-Other: 2\r\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
