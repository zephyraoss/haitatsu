package bounce

import "testing"

func TestParseDeliveryStatus(t *testing.T) {
	details := parseDeliveryStatus([]byte("Reporting-MTA: dns; mx.example.com\r\n\r\nFinal-Recipient: rfc822; user@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\nDiagnostic-Code: smtp; 550 5.1.1 user unknown\r\n"))

	if details["dsn"] != true {
		t.Fatal("expected dsn details")
	}
	if details["reporting_mta"] != "dns; mx.example.com" {
		t.Fatalf("unexpected reporting_mta: %v", details["reporting_mta"])
	}
	recipients, ok := details["recipients"].([]map[string]any)
	if !ok || len(recipients) != 1 {
		t.Fatalf("unexpected recipients: %#v", details["recipients"])
	}
	if recipients[0]["final_recipient_address"] != "user@example.com" {
		t.Fatalf("unexpected final recipient address: %v", recipients[0]["final_recipient_address"])
	}
	if recipients[0]["status"] != "5.1.1" {
		t.Fatalf("unexpected status: %v", recipients[0]["status"])
	}
}

func TestBounceDetailsReadsMultipartReport(t *testing.T) {
	raw := []byte("From: mailer-daemon@example.com\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=dsn\r\n\r\n--dsn\r\nContent-Type: text/plain\r\n\r\nFailed.\r\n--dsn\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.com\r\n\r\nFinal-Recipient: rfc822; user@example.com\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n--dsn--\r\n")
	details := bounceDetails(raw)

	if details["dsn"] != true {
		t.Fatalf("expected DSN details, got %#v", details)
	}
}
