package mailparse

import (
	"fmt"
	"strings"
	"testing"
)

func manyPartsMessage(parts int) []byte {
	var b strings.Builder
	b.WriteString("From: a@example.com\r\nTo: b@example.com\r\nSubject: x\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n")
	for i := 0; i < parts; i++ {
		fmt.Fprintf(&b, "--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=%d\r\n\r\nx\r\n", i)
	}
	b.WriteString("--b--\r\n")
	return []byte(b.String())
}

func deeplyNestedMessage(depth int) []byte {
	var b strings.Builder
	b.WriteString("From: a@example.com\r\nContent-Type: multipart/mixed; boundary=b0\r\n\r\n")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "--b%d\r\nContent-Type: multipart/mixed; boundary=b%d\r\n\r\n", i-1, i)
	}
	fmt.Fprintf(&b, "--b%d\r\nContent-Type: text/plain\r\n\r\ndeep\r\n--b%d--\r\n", depth, depth)
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--b%d--\r\n", i)
	}
	return []byte(b.String())
}

func TestParseCapsPartsAndAttachments(t *testing.T) {
	metadata := Parse(manyPartsMessage(MaxParts * 4))
	if len(metadata.Attachments) != MaxAttachments {
		t.Fatalf("attachments = %d, want %d", len(metadata.Attachments), MaxAttachments)
	}
	if metadata.Subject != "x" || len(metadata.From) != 1 {
		t.Fatalf("header metadata lost: %+v", metadata)
	}
}

func TestExtractAttachmentBeyondPartCap(t *testing.T) {
	if _, err := ExtractAttachment(manyPartsMessage(MaxParts*2), MaxParts+1); err == nil {
		t.Fatal("expected error for part beyond cap")
	}
	attachment, err := ExtractAttachment(manyPartsMessage(3), 2)
	if err != nil || attachment.Filename != "1" {
		t.Fatalf("attachment = %+v, err = %v", attachment, err)
	}
}

func TestParseCapsNestingDepth(t *testing.T) {
	if got := Parse(deeplyNestedMessage(MaxDepth - 1)).TextExtract; got != "deep" {
		t.Fatalf("text extract = %q, want deep", got)
	}
	if got := Parse(deeplyNestedMessage(MaxDepth * 4)).TextExtract; got != "" {
		t.Fatalf("expected no text extract past depth cap, got %q", got)
	}
}
