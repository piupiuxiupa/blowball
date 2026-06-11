package jwt

import (
	"testing"
	"time"
)

const testSecret = "test-secret-key"

func TestSignAndVerify_Valid(t *testing.T) {
	const userID = "11111111-1111-4111-8111-111111111111"
	tok, err := Sign(testSecret, userID, time.Hour)
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}
	if tok == "" {
		t.Fatal("Sign returned empty token")
	}

	got, err := Verify(testSecret, tok)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if got != userID {
		t.Errorf("Verify user_id = %q, want %q", got, userID)
	}
}

func TestVerify_ExpiredReturnsErr(t *testing.T) {
	const userID = "22222222-2222-4222-8222-222222222222"
	// Sign a token that expired 1 hour ago.
	tok, err := Sign(testSecret, userID, -time.Hour)
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	if _, err := Verify(testSecret, tok); err == nil {
		t.Fatal("Verify expected error for expired token, got nil")
	}
}

func TestVerify_InvalidSignatureReturnsErr(t *testing.T) {
	const userID = "33333333-3333-4333-8333-333333333333"
	tok, err := Sign(testSecret, userID, time.Hour)
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	if _, err := Verify("wrong-secret", tok); err == nil {
		t.Fatal("Verify expected error for mismatched signature, got nil")
	}
}

func TestVerify_MalformedTokenReturnsErr(t *testing.T) {
	if _, err := Verify(testSecret, "not.a.jwt"); err == nil {
		t.Fatal("Verify expected error for malformed token, got nil")
	}
}

func TestSign_EmptySecretReturnsErr(t *testing.T) {
	if _, err := Sign("", "user", time.Hour); err == nil {
		t.Fatal("Sign expected error for empty secret, got nil")
	}
}
