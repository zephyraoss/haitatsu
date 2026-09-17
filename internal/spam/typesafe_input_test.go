package spam

import (
	"strings"
	"testing"
)

func TestBuildTypeSafeStateHTMLAndLinks(t *testing.T) {
	raw := []byte("From: \"Acme Billing\" <billing@acme.test>\r\n" +
		"Reply-To: support@acme.test\r\n" +
		"Subject: =?UTF-8?Q?Invoice_=E2=82=AC42?=\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><head><style>p{color:red}</style></head><body>" +
		"<p>Your invoice is ready</p><script>steal()</script>" +
		"<a href=\"https://user:pass@evil.example/path?token=abc#frag\">Click here</a>" +
		"</body></html>")
	state, info := BuildTypeSafeState(raw, 8192, map[string]any{"spf": "pass", "dkim": "pass", "dmarc": "pass", "dmarc_policy": "none"})
	if info.Status != "ok" {
		t.Fatalf("info: %+v", info)
	}
	if strings.Contains(state["email"].(map[string]any)["html_text"].(string), "style") || strings.Contains(state["email"].(map[string]any)["html_text"].(string), "steal()") {
		t.Fatalf("script/style leaked: %q", state["email"])
	}
	if !strings.Contains(state["email"].(map[string]any)["html_text"].(string), "Your invoice is ready") {
		t.Fatalf("visible text missing: %q", state["email"])
	}
	if state["email"].(map[string]any)["subject"] != "Invoice €42" {
		t.Fatalf("subject = %q", state["email"].(map[string]any)["subject"])
	}
	if from := state["email"].(map[string]any)["from"].([]map[string]string); len(from) != 1 || from[0]["name"] != "Acme Billing" || from[0]["address"] != "billing@acme.test" {
		t.Fatalf("from = %v", state["from"])
	}
	links := state["email"].(map[string]any)["links"].([]typeSafeLink)
	if len(links) != 1 || links[0].Destination != "https://evil.example/path" || links[0].Label != "Click here" {
		t.Fatalf("links: %+v", links)
	}
	if _, ok := state["authentication"]; !ok {
		t.Fatal("authentication summary missing")
	}
}

func TestBuildTypeSafeStateTruncationAndUnusable(t *testing.T) {
	raw := []byte("Subject: hello\r\nMessage-ID: <a@b>\r\nContent-Type: text/plain\r\n\r\n" + strings.Repeat("a", 5000))
	state, info := BuildTypeSafeState(raw, 1000, nil)
	if info.Status != "partial" || !info.Truncated {
		t.Fatalf("info: %+v", info)
	}
	if len(state["email"].(map[string]any)["plain_text"].(string)) > 1000 {
		t.Fatal("text budget exceeded")
	}

	empty := []byte("Subject: blank\r\nContent-Type: text/plain\r\n\r\n   \r\n")
	_, info = BuildTypeSafeState(empty, 1000, nil)
	if info.Status != "skipped_no_content" {
		t.Fatalf("info: %+v", info)
	}

	_, info = BuildTypeSafeState([]byte("not a message at all"), 1000, nil)
	if info.Status != "skipped_parse_error" {
		t.Fatalf("info: %+v", info)
	}
}

func TestBuildTypeSafeStateAttachmentMetadataOnly(t *testing.T) {
	raw := []byte("Subject: files\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nSee attached\r\n" +
		"--b\r\nContent-Type: application/pdf; name=invoice.pdf\r\nContent-Disposition: attachment; filename=invoice.pdf\r\n\r\n%PDF-1.7 secret payload\r\n--b--\r\n")
	state, info := BuildTypeSafeState(raw, 8192, nil)
	if info.Status != "ok" && info.Status != "partial" {
		t.Fatalf("info: %+v", info)
	}
	names := state["email"].(map[string]any)["attachments"].([]map[string]string)
	if len(names) != 1 || !strings.Contains(names[0]["filename"], "invoice.pdf") {
		t.Fatalf("attachments = %v", names)
	}
	if strings.Contains(state["email"].(map[string]any)["plain_text"].(string), "%PDF") || strings.Contains(state["email"].(map[string]any)["plain_text"].(string), "secret payload") {
		t.Fatal("attachment bytes leaked into text")
	}
}
