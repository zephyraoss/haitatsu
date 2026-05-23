package mailparse

import (
	"strings"
	"testing"
)

func TestNormalizeMessagePreservesHeadersAfterDKIMSignature(t *testing.T) {
	signed := []byte("DKIM-Signature: v=1; a=rsa-sha256; d=emails.ax; s=zpr1; h=From:Subject;\r\n b=abc\r\n" +
		"From: Addison <wafwaf@emails.ax>\r\n" +
		"To: test@gmail.com\r\n" +
		"Subject: Testing Bounces\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"X-Haitatsu-Trace-ID: 01TEST\r\n" +
		"X-Haitatsu-Node: node\r\n" +
		"\r\n" +
		"Body text here.\r\n")

	header, body := SplitHeaderBody(signed)
	t.Logf("split signed: header=%q body=%q", header, body)

	normalized := NormalizeMessage(signed)
	header, body = SplitHeaderBody(normalized)
	t.Logf("after norm: header=%q body=%q", header, body)

	if strings.Contains(string(body), "From:") || strings.Contains(string(body), "Subject:") {
		t.Fatalf("headers leaked into body after normalize: header=%q body=%q", header, body)
	}
	if !strings.Contains(string(header), "DKIM-Signature:") {
		t.Fatalf("DKIM missing from header: %q", header)
	}
	if !strings.Contains(string(header), "Subject: Testing Bounces") {
		t.Fatalf("Subject missing from header: %q", header)
	}
	if !strings.Contains(string(body), "Body text here.") {
		t.Fatalf("body missing: %q", body)
	}
}
