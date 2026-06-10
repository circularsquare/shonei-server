package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Tokens are signed with authSecret, which initAuth normally sets. Tests set it
// directly so they don't depend on the environment.
func init() {
	authSecret = []byte("test-secret")
}

func TestTokenRoundTrip(t *testing.T) {
	tok := makeToken("anita")
	name, err := verifyToken(tok)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if name != "anita" {
		t.Fatalf("got username %q, want %q", name, "anita")
	}
}

func TestTokenRejectsTamperedSignature(t *testing.T) {
	tok := makeToken("anita")
	// Flip the last character of the signature segment.
	parts := strings.Split(tok, "|")
	sig := []byte(parts[2])
	if sig[len(sig)-1] == 'A' {
		sig[len(sig)-1] = 'B'
	} else {
		sig[len(sig)-1] = 'A'
	}
	tampered := parts[0] + "|" + parts[1] + "|" + string(sig)
	if _, err := verifyToken(tampered); err != errBadSignature {
		t.Fatalf("got %v, want errBadSignature", err)
	}
}

func TestTokenRejectsWrongSecret(t *testing.T) {
	tok := makeToken("anita")
	saved := authSecret
	authSecret = []byte("a-different-secret")
	defer func() { authSecret = saved }()
	if _, err := verifyToken(tok); err != errBadSignature {
		t.Fatalf("got %v, want errBadSignature", err)
	}
}

func TestTokenRejectsExpired(t *testing.T) {
	// Hand-build a token whose expiry is in the past, signed correctly.
	payload := encodeUser("anita") + "|" + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	expired := payload + "|" + sign(payload)
	if _, err := verifyToken(expired); err != errExpiredToken {
		t.Fatalf("got %v, want errExpiredToken", err)
	}
}

func TestTokenRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "nodelimiters", "only|two", "a|b|c|d"} {
		if _, err := verifyToken(bad); err == nil {
			t.Fatalf("expected error for malformed token %q, got nil", bad)
		}
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2!"), bcryptCost)
	if err != nil {
		t.Fatalf("hash failed: %v", err)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte("hunter2!")) != nil {
		t.Fatal("correct password rejected")
	}
	if bcrypt.CompareHashAndPassword(hash, []byte("wrong")) == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestUsernameValidation(t *testing.T) {
	valid := []string{"anita", "a_b_c", "mouse123", "abc"}
	invalid := []string{"ab", "AnitA", "has space", "toolongusername123456", "bad-dash", "pipe|name", ""}
	for _, u := range valid {
		if !usernameRe.MatchString(u) {
			t.Errorf("expected %q valid", u)
		}
	}
	for _, u := range invalid {
		if usernameRe.MatchString(u) {
			t.Errorf("expected %q invalid", u)
		}
	}
}

// encodeUser mirrors makeToken's username encoding for the expiry test.
func encodeUser(u string) string {
	return strings.Split(makeToken(u), "|")[0]
}
