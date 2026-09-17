package spam

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTypeSafeAlternativeBudgetsIgnoreMIMEOrder(t *testing.T) {
	plain := "--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("Legitimate € message ", 1000) + "\r\n"
	html := "--b\r\nContent-Type: text/html\r\n\r\n<p>steal your password</p><a href=\"https://user:pass@evil.test/login?secret=yes#token\">Sign in</a>\r\n"
	for _, parts := range []string{plain + html, html + plain} {
		raw := []byte("Content-Type: multipart/alternative; boundary=b\r\n\r\n" + parts + "--b--\r\n")
		state, info := BuildTypeSafeState(raw, 80, nil)
		email := state["email"].(map[string]any)
		p, h := email["plain_text"].(string), email["html_text"].(string)
		if p == "" || !strings.Contains(h, "steal") || len(p)+len(h) > 80 || !utf8.ValidString(p+h) || !info.Truncated {
			t.Fatalf("lost alternatives/budget: %+v %#v", info, email)
		}
		links := email["links"].([]typeSafeLink)
		if len(links) != 1 || links[0].Destination != "https://evil.test/login" {
			t.Fatalf("links: %+v", links)
		}
	}
}

func TestTypeSafeInputBoundsAndPartialMIME(t *testing.T) {
	_, info := BuildTypeSafeState([]byte("Subject: "+strings.Repeat("x", 40000)+"\r\n\r\nbody"), 100, nil)
	if info.Status != "skipped_parse_error" {
		t.Fatalf("unbounded headers: %+v", info)
	}
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n"
	for range 100 {
		raw += "--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n"
	}
	raw += "--b--\r\n"
	_, info = BuildTypeSafeState([]byte(raw), 100, nil)
	if !info.Truncated || info.Parts > typeSafeMaxParts {
		t.Fatalf("part limit: %+v", info)
	}
	state, info := BuildTypeSafeState([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nUsable content\r\n--b\r\nbroken header\r\n"), 100, nil)
	if info.Status != "partial" || !strings.Contains(state["email"].(map[string]any)["plain_text"].(string), "Usable") {
		t.Fatalf("lost partial MIME: %+v %#v", info, state)
	}
}

func TestTypeSafeUnicodeAndUntrustedHeaders(t *testing.T) {
	raw := []byte("Authentication-Results: forged; spf=pass\r\nBcc: hidden@test\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n€€\xffsee https://user:pass@evil.test/path?token=secret#private")
	state, info := BuildTypeSafeState(raw, 200, map[string]any{"spf": "fail", "remote_ip": "private-ip", "spam_reasons": []string{"hidden"}})
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "partial" || !utf8.ValidString(state["email"].(map[string]any)["plain_text"].(string)) {
		t.Fatalf("Unicode: %+v", info)
	}
	for _, secret := range []string{"forged", "hidden@test", "private-ip", "token=secret", "user:pass"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if state["authentication"].(map[string]string)["spf"] != "fail" {
		t.Fatal("trusted auth missing")
	}
	_, info = BuildTypeSafeState([]byte("Content-Type: text/html\r\n\r\n<img src=\"https://remote.test/image\">"), 100, nil)
	if info.Status != "skipped_no_content" {
		t.Fatalf("image-only: %+v", info)
	}
}
