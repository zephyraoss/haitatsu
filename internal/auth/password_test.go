package auth

import "testing"

func TestPasswordHashVerifiesOnlyOriginalPassword(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	valid, err := VerifyPassword("secret", hash)
	if err != nil {
		t.Fatalf("verify password: %v", err)
	}
	if !valid {
		t.Fatal("expected password to verify")
	}

	valid, err = VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("verify wrong password: %v", err)
	}
	if valid {
		t.Fatal("expected wrong password to fail")
	}
}
