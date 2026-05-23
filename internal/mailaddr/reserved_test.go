package mailaddr

import "testing"

func TestReservedLocalPart(t *testing.T) {
	if !ReservedLocalPart("bounces") || !ReservedLocalPart("BOUNCES") {
		t.Fatal("expected bounces to be reserved")
	}
	if ReservedLocalPart("bounces+tag") || ReservedLocalPart("alice") {
		t.Fatal("expected only exact bounces local to be reserved")
	}
}

func TestValidateAddressNotReserved(t *testing.T) {
	if err := ValidateAddressNotReserved("alice@example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateAddressNotReserved("bounces@example.com"); err != ErrReservedLocalPart {
		t.Fatalf("expected reserved error, got %v", err)
	}
}
