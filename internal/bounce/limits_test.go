package bounce

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zephyraoss/haitatsu/internal/mailparse"
)

func dsnMessage(recipients int, filler int) []byte {
	var b strings.Builder
	b.WriteString("From: mailer-daemon@example.com\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=b\r\n\r\n")
	for i := 0; i < filler; i++ {
		b.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	b.WriteString("--b\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.com\r\n\r\n")
	for i := 0; i < recipients; i++ {
		fmt.Fprintf(&b, "Final-Recipient: rfc822; user%d@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n", i)
	}
	b.WriteString("--b--\r\n")
	return []byte(b.String())
}

func TestBounceDetailsCapsRecipients(t *testing.T) {
	details := bounceDetails(dsnMessage(maxStatusBlocks*4, 0))
	recipients, _ := details["recipients"].([]map[string]any)
	if len(recipients) != maxStatusRecipients {
		t.Fatalf("recipients = %d, want %d", len(recipients), maxStatusRecipients)
	}
	if details["reporting_mta"] != "dns; mx.example.com" {
		t.Fatalf("unexpected details %+v", details)
	}
}

func TestBounceDetailsCapsPartsScanned(t *testing.T) {
	if details := bounceDetails(dsnMessage(1, mailparse.MaxParts)); details["dsn"] != nil {
		t.Fatalf("expected delivery-status past part cap to be ignored, got %+v", details)
	}
	if details := bounceDetails(dsnMessage(1, 2)); details["dsn"] != true {
		t.Fatalf("expected dsn parsed, got %+v", details)
	}
}

func TestDeliveryStatusBlocksCapped(t *testing.T) {
	data := strings.Repeat("Action: failed\n\n", maxStatusBlocks*3)
	if got := len(deliveryStatusBlocks([]byte(data))); got != maxStatusBlocks {
		t.Fatalf("blocks = %d, want %d", got, maxStatusBlocks)
	}
}
