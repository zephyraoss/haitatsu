package routing

import (
	"context"
	"testing"
)

func TestResolveRejectsReservedBouncesLocal(t *testing.T) {
	r := NewResolver(nil)
	_, ok, err := r.Resolve(context.Background(), "bounces@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected reserved bounces@ address to not resolve")
	}
}
