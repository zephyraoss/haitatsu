package outbound

import (
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func TestNormalizeSubmittedMessageThunderbirdSeparator(t *testing.T) {
	raw := []byte("From: Sender <wafwaf@emails.ax>\r\n" +
		"To: recipient@gmail.com\r\n" +
		"Subject: test again\r\n" +
		"Content-Type: text/plain; charset=UTF-8; format=flowed\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\n" +
		"waow, we need to test this agaiN!\r\n\r\n" +
		"oopsies,\r\n\r\n" +
		"addison leclair\r\n")
	traceID := "01KS9Z9ZSKKJPNVEEQS9ZTYVE4"
	node := "haitatsu-haitatsu"

	normalized := normalizeSubmittedMessage(raw, "wafwaf@emails.ax", "msg-id", "emails.ax", traceID, node)
	header, body := mailparse.SplitHeaderBody(normalized)

	if strings.Contains(string(header), "7bitX-Haitatsu") {
		t.Fatalf("trace header concatenated onto previous line: %q", header)
	}
	if strings.Contains(string(header), "Content-Transfer-Encoding: 7bit\r\n\r\nHello") ||
		strings.Contains(string(header), "Content-Transfer-Encoding: 7bit\n\nHello") {
		t.Fatalf("body text leaked into header: %q", header)
	}
	if !strings.Contains(string(body), "waow, we need to test this agaiN!") {
		t.Fatalf("message body missing: %q", body)
	}
	if strings.Contains(string(body), "X-Haitatsu-Trace-ID") {
		t.Fatalf("trace header leaked into body: %q", body)
	}
}

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
	lower := strings.ToLower(string(normalized))
	if !strings.Contains(lower, strings.ToLower(traceID)) || !strings.Contains(lower, "x-haitatsu-trace-id") {
		t.Fatalf("normalized message missing trace header: %q", normalized)
	}
	header, body := mailparse.SplitHeaderBody(normalized)

	if !strings.Contains(strings.ToLower(string(header)), strings.ToLower("X-Haitatsu-Trace-ID: "+traceID)) {
		t.Fatalf("trace header missing from header block: %q", header)
	}
	if !strings.Contains(strings.ToLower(string(header)), strings.ToLower("X-Haitatsu-Node: "+node)) {
		t.Fatalf("node header missing from header block: %q", header)
	}
	if strings.Contains(string(body), "X-Haitatsu-Trace-ID") {
		t.Fatalf("trace header leaked into body: %q", body)
	}
	if !strings.Contains(string(body), "Hello from the body.") {
		t.Fatalf("body content missing: %q", body)
	}
}
