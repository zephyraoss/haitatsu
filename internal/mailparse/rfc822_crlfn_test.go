package mailparse

import (
	"strings"
	"testing"
)

func TestSplitHeaderBodyCRLFLineEndingLFBodySeparator(t *testing.T) {
	raw := []byte("From: a@emails.ax\r\n" +
		"To: b@gmail.com\r\n" +
		"Subject: Hello, world!\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\n" +
		"Hello, world!\r\n\r\n" +
		"Addison LeClair\r\n")

	header, body := SplitHeaderBody(raw)
	if strings.Contains(string(header), "Content-Transfer-Encoding: 7bit\r\n\r\nHello") ||
		strings.Contains(string(header), "Content-Transfer-Encoding: 7bit\n\nHello") {
		t.Fatalf("header should not contain body: %q", header)
	}
	if !strings.Contains(string(body), "Hello, world!") {
		t.Fatalf("body missing message text: %q", body)
	}
	if !strings.Contains(string(body), "Addison LeClair") {
		t.Fatalf("body missing signature: %q", body)
	}
}

func TestSplitHeaderBodyDoesNotSplitOnBodyParagraphSeparator(t *testing.T) {
	raw := []byte("From: a@emails.ax\r\n" +
		"Subject: test\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\r\n" +
		"Hello, world!\r\n\r\n" +
		"Addison LeClair\r\n")

	header, body := SplitHeaderBody(raw)
	if strings.Contains(string(header), "Content-Transfer-Encoding: 7bit\r\n\r\nHello") {
		t.Fatalf("header should not contain body: %q", header)
	}
	if !strings.Contains(string(body), "Hello, world!") {
		t.Fatalf("body missing message text: %q", body)
	}
}
