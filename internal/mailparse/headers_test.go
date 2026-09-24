package mailparse

import (
	"reflect"
	"testing"
)

func TestParseHeadersMatchesParse(t *testing.T) {
	raw := []byte("Received: by mx (Haitatsu) with SMTP; Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
		"Received: from relay\r\n" +
		"X-Haitatsu-Trace-ID: 01TEST\r\n" +
		"From: Sender <sender@example.com>\r\n" +
		"To: rcpt@example.com\r\n" +
		"Subject: Hello\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=a.bin\r\n\r\nxxxx\r\n--b--\r\n")

	if got, want := ParseHeaders(raw), Parse(raw).Headers; !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseHeaders = %v, want %v", got, want)
	}
}
