package mailparse

import (
	"net/textproto"
	"strings"
	"testing"
)

func TestInjectHeadersThunderbirdSeparator(t *testing.T) {
	raw := []byte("From: Addison <wafwaf@emails.ax>\r\n" +
		"To: addidotlol@gmail.com\r\n" +
		"Subject: test again\r\n" +
		"Content-Type: text/plain; charset=UTF-8; format=flowed\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\n" +
		"waow, we need to test this agaiN!\r\n\r\n" +
		"oopsies,\r\n\r\n" +
		"addison leclair\r\n")
	traceID := "01KSA0JDM6QAEB7CXG6GF8R7EC"
	node := "haitatsu-haitatsu"

	out, err := InjectHeaders(raw, func(h textproto.MIMEHeader) {
		h.Set("X-Haitatsu-Trace-ID", traceID)
		h.Set("X-Haitatsu-Node", node)
	})
	if err != nil {
		t.Fatal(err)
	}
	header, body := SplitHeaderBody(out)
	if !strings.Contains(strings.ToLower(string(header)), strings.ToLower("X-Haitatsu-Trace-ID: "+traceID)) {
		t.Fatalf("trace header missing: %q", header)
	}
	if strings.Contains(string(header), "7bitX-Haitatsu") {
		t.Fatalf("header lines merged: %q", header)
	}
	if !strings.Contains(string(body), "waow, we need to test this agaiN!") {
		t.Fatalf("body missing message: %q", body)
	}
	if strings.Contains(string(body), "X-Haitatsu-Trace-ID") {
		t.Fatalf("trace header leaked into body: %q", body)
	}
}

func TestStripHaitatsuHeaders(t *testing.T) {
	header := []byte("X-Haitatsu-Trace-ID: old\r\n folded\r\nSubject: hi")
	if stripped := string(StripHaitatsuHeaders(header)); stripped != "Subject: hi" {
		t.Fatalf("stripped = %q", stripped)
	}
}

func TestInjectHeadersKeepsBodyAndFoldsLongHeaders(t *testing.T) {
	raw := []byte("X-Haitatsu-Node: spoofed\r\nTo: " + strings.Repeat("someone@example.test, ", 60) + "last@example.test\r\n\r\nX-Haitatsu-Note: keep\r\n")
	out, err := InjectHeaders(raw, func(textproto.MIMEHeader) {})
	if err != nil {
		t.Fatal(err)
	}
	header, body := SplitHeaderBody(out)
	if strings.Contains(string(header), "spoofed") || !strings.Contains(string(body), "X-Haitatsu-Note: keep") {
		t.Fatalf("out = %q", out)
	}
	for _, line := range strings.Split(string(header), "\r\n") {
		if len(line) > 78 {
			t.Fatalf("unfolded header line %q", line)
		}
	}
}
