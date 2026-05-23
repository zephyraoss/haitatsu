package outbound

import (
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func TestNormalizeSubmittedMessageTraceHeadersInHeaderBlock(t *testing.T) {
	raw := []byte(
		"From: Sender <wafwaf@emails.ax>\n" +
			"To: recipient@example.com\n" +
			"Subject: test\n" +
			"Content-Type: text/plain; charset=UTF-8\n" +
			"Content-Transfer-Encoding: 7bit\n" +
			"\n" +
			"Hello from the body.\n",
	)
	traceID := "01KS9YFB8STSRGGJWN5CFW398S"
	node := "haitatsu-haitatsu"

	normalized := normalizeSubmittedMessage(raw, "wafwaf@emails.ax", "msg-id", "emails.ax", traceID, node)
	header, body := mailparse.SplitHeaderBody(normalized)

	if !strings.Contains(string(header), "X-Haitatsu-Trace-ID: "+traceID) {
		t.Fatalf("trace header missing from header block: %q", header)
	}
	if !strings.Contains(string(header), "X-Haitatsu-Node: "+node) {
		t.Fatalf("node header missing from header block: %q", header)
	}
	if strings.Contains(string(body), "X-Haitatsu-Trace-ID") {
		t.Fatalf("trace header leaked into body: %q", body)
	}
	if !strings.Contains(string(body), "Hello from the body.") {
		t.Fatalf("body content missing: %q", body)
	}
}
