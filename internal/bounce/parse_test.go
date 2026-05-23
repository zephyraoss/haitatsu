package bounce

import (
	"context"
	"testing"
)

type fakeHosted struct {
	domains map[string]bool
}

func (f fakeHosted) IsHosted(_ context.Context, domain string) (bool, error) {
	return f.domains[domain], nil
}

func TestParseRecipientValidVERP(t *testing.T) {
	h := &Handler{hosted: fakeHosted{domains: map[string]bool{"emails.ax": true}}}
	recipient, isBounce, valid := h.ParseRecipient(context.Background(), "bounces+01ARZ3NDEKTSV4RRFFQ69G5FAV@emails.ax")
	if !isBounce || !valid {
		t.Fatalf("expected valid bounce, got isBounce=%v valid=%v", isBounce, valid)
	}
	if recipient.MessageID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Fatalf("unexpected message id %q", recipient.MessageID)
	}
}

func TestParseRecipientRejectsUnhostedDomain(t *testing.T) {
	h := &Handler{hosted: fakeHosted{domains: map[string]bool{}}}
	_, isBounce, valid := h.ParseRecipient(context.Background(), "bounces+01ARZ3NDEKTSV4RRFFQ69G5FAV@emails.ax")
	if !isBounce || valid {
		t.Fatal("expected invalid bounce on unhosted domain")
	}
}

func TestParseRecipientReservedLocalWithoutTag(t *testing.T) {
	h := &Handler{hosted: fakeHosted{domains: map[string]bool{"emails.ax": true}}}
	_, isBounce, valid := h.ParseRecipient(context.Background(), "bounces@emails.ax")
	if !isBounce || valid {
		t.Fatal("expected invalid bounce for bare bounces@ address")
	}
}

func TestParseRecipientIgnoresNormalAddress(t *testing.T) {
	h := &Handler{hosted: fakeHosted{domains: map[string]bool{"emails.ax": true}}}
	_, isBounce, _ := h.ParseRecipient(context.Background(), "alice@emails.ax")
	if isBounce {
		t.Fatal("expected normal address to skip bounce handler")
	}
}
