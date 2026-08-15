package auth_test

import (
	"errors"
	"testing"

	"github.com/toluwalase/kolo-bank-server/internal/auth"
)

func TestPasswordHashVerifyRoundTrip(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	if err := auth.VerifyPassword("correct horse battery staple", hash); err != nil {
		t.Fatalf("verify correct password: %v", err)
	}

	err = auth.VerifyPassword("wrong password", hash)
	if !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("verify wrong password: got %v, want ErrInvalidPassword", err)
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	h1, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatalf("hash 1: %v", err)
	}
	h2, err := auth.HashPassword("same password")
	if err != nil {
		t.Fatalf("hash 2: %v", err)
	}
	if h1 == h2 {
		t.Fatal("expected two hashes of the same password to differ (random salt)")
	}
}
