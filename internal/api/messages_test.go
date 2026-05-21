package api

import "testing"

func TestMessageSearchWhereTermsClause(t *testing.T) {
	where, args := messageSearchWhere(messageSearch{Terms: []string{"inbound"}})
	if len(where) != 1 {
		t.Fatalf("where len = %d, want 1", len(where))
	}
	if got := where[0]; got != messageSearchVector+" @@ query" {
		t.Fatalf("where[0] = %q", got)
	}
	if len(args) != 1 || args[0] != "inbound" {
		t.Fatalf("args = %#v, want [inbound]", args)
	}
}

func TestMessageSearchWhereFromUsesPlaceholder(t *testing.T) {
	where, args := messageSearchWhere(messageSearch{From: "alice"})
	if len(where) != 1 {
		t.Fatalf("where len = %d, want 1", len(where))
	}
	if got := where[0]; got != "from_addresses::text ILIKE '%' || $1 || '%'" {
		t.Fatalf("where[0] = %q", got)
	}
	if len(args) != 1 || args[0] != "alice" {
		t.Fatalf("args = %#v, want [alice]", args)
	}
}
