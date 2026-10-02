package mailparse

import (
	"bufio"
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"
)

func readHeaderMap(t *testing.T, raw []byte) map[string][]string {
	t.Helper()
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("ReadHeader(%q): %v", raw, err)
	}
	return header.Map()
}

func TestSplitHeaderBodyMatchesGoMessageBoundary(t *testing.T) {
	cases := map[string]string{
		"whitespace-only line folds into previous header": "From: a@example.com\r\n" +
			"Subject: hi\r\n" +
			" \r\n" +
			"X-Injected: after-fold\r\n" +
			"\r\n" +
			"Body\r\n",
		"tab-only line folds into previous header": "From: a@example.com\r\n" +
			"Subject: hi\r\n" +
			"\t\r\n" +
			"Reply-To: evil@example.com\r\n" +
			"\r\n" +
			"Body\r\n",
		"mixed CRLF and LF separators": "From: a@example.com\r\n" +
			"Subject: hi\n" +
			" folded\r\n" +
			"To: b@example.com\n" +
			"\n" +
			"Body\r\n",
		"CRLF-LF terminator": "From: a@example.com\r\n" +
			"Subject: hi\r\n" +
			"\n" +
			"X-Body: not-a-header\r\n" +
			"\r\n" +
			"Body\r\n",
		"folded line with only spaces then more spaces": "From: a@example.com\r\n" +
			"Subject: hi\r\n" +
			"   \r\n" +
			"  \r\n" +
			"X-Trailing: yes\r\n" +
			"\r\n" +
			"Body\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			evaluated := readHeaderMap(t, []byte(raw))
			stored := readHeaderMap(t, NormalizeMessage([]byte(raw)))
			if !reflect.DeepEqual(evaluated, stored) {
				t.Fatalf("header set diverged\nevaluated: %v\nstored:    %v", evaluated, stored)
			}
		})
	}
}

func TestSplitHeaderBodyWhitespaceLineIsContinuation(t *testing.T) {
	raw := []byte("From: a@example.com\r\nSubject: hi\r\n \r\nX-After: yes\r\n\r\nBody\r\n")
	header, body := SplitHeaderBody(raw)
	if !strings.Contains(string(header), "X-After: yes") {
		t.Fatalf("header lost field after folded whitespace line: %q", header)
	}
	if string(body) != "Body\r\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestSplitHeaderBodyLeadingBlankLineYieldsEmptyHeader(t *testing.T) {
	raw := []byte("\r\nFrom: a@example.com\r\n\r\nBody\r\n")
	evaluated := readHeaderMap(t, raw)
	stored := readHeaderMap(t, NormalizeMessage(raw))
	if len(evaluated) != 0 || !reflect.DeepEqual(evaluated, stored) {
		t.Fatalf("evaluated = %v stored = %v", evaluated, stored)
	}
}
