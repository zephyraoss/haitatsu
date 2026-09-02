package api

import "testing"

func TestCursorRoundTrip(t *testing.T) {
	encoded := encodeCursor("2025-09-01T10:00:00Z", "01ABC")
	decoded, ok := decodeCursor(encoded)
	if !ok || decoded.CreatedAt != "2025-09-01T10:00:00Z" || decoded.ID != "01ABC" {
		t.Fatalf("round trip failed: %+v %v", decoded, ok)
	}
	if _, ok := decodeCursor("not-base64!"); ok {
		t.Fatal("garbage cursor must be rejected")
	}
}

func TestNextCursorTrimsPage(t *testing.T) {
	type row struct{ id string }
	items := []row{{"a"}, {"b"}, {"c"}}
	page, next := nextCursor(items, 2, func(r row) (string, string) { return "t", r.id })
	if len(page) != 2 || next == "" {
		t.Fatalf("page=%v next=%q", page, next)
	}
	page, next = nextCursor(items, 3, func(r row) (string, string) { return "t", r.id })
	if len(page) != 3 || next != "" {
		t.Fatal("full page without overflow must not produce a cursor")
	}
}

func TestServiceTokenConstantTime(t *testing.T) {
	if !hasServiceToken("Bearer secret", "secret") {
		t.Fatal("matching token should pass")
	}
	if hasServiceToken("Bearer secre", "secret") || hasServiceToken("Bearer secret", "") || hasServiceToken("secret", "secret") {
		t.Fatal("mismatched tokens should fail")
	}
}
