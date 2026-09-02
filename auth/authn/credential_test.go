package authn

import (
	"errors"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	valid, err := VerifyPassword("correct horse battery staple", hash)
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

func TestHashPasswordRejectsShortPassword(t *testing.T) {
	t.Parallel()

	if _, err := HashPassword("too short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("HashPassword() error = %v, want ErrWeakPassword", err)
	}
}
