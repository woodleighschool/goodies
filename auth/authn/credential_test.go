package authn

import (
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword("")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	valid, err := VerifyPassword("", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !valid {
		t.Fatal("VerifyPassword() = false, want true")
	}

	valid, err = VerifyPassword("incorrect password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() wrong password error = %v", err)
	}
	if valid {
		t.Fatal("VerifyPassword() wrong password = true, want false")
	}
}
