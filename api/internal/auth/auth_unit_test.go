package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var fastArgon = Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestPasswordHashAndVerify(t *testing.T) {
	h := NewHasher(fastArgon, 2)
	ctx := context.Background()
	hash, err := h.Hash(ctx, "correct horse 1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("unexpected PHC format: %s", hash)
	}
	if ok, err := h.Verify(ctx, "correct horse 1", hash); err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}
	if ok, _ := h.Verify(ctx, "wrong horse 1", hash); ok {
		t.Fatal("wrong password verified")
	}
	other, _ := h.Hash(ctx, "correct horse 1")
	if other == hash {
		t.Fatal("hashes must be salted")
	}
	if _, err := h.Verify(ctx, "x", "$bcrypt$nope"); err != ErrMalformedHash {
		t.Fatalf("expected ErrMalformedHash, got %v", err)
	}
}

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short1":                   false,
		"onlyletters":              false,
		"12345678":                 false,
		"letters123":               true,
		"pässwört9":                true,
		strings.Repeat("a1", 65):   false,
		strings.Repeat("ab12", 10): true,
	} {
		if got := ValidatePassword(pw) == nil; got != ok {
			t.Errorf("ValidatePassword(%q) ok=%v, want %v", pw, got, ok)
		}
	}
}

func TestAccessTokens(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	iss := NewTokenIssuer(secret, "test", 15*time.Minute)
	uid := uuid.New()
	now := time.Now()
	tok, err := iss.Issue(uid, now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := iss.Parse(tok)
	if err != nil || claims.Subject != uid.String() {
		t.Fatalf("parse failed: %v %+v", err, claims)
	}

	expired, _ := iss.Issue(uid, now.Add(-time.Hour))
	if _, err := iss.Parse(expired); err == nil {
		t.Error("expired token accepted")
	}
	other := NewTokenIssuer([]byte(strings.Repeat("x", 32)), "test", time.Minute)
	if _, err := other.Parse(tok); err == nil {
		t.Error("token signed with another secret accepted")
	}
	wrongIss := NewTokenIssuer(secret, "someone-else", time.Minute)
	if _, err := wrongIss.Parse(tok); err == nil {
		t.Error("token from another issuer accepted")
	}
	// alg=none must be rejected.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{Subject: uid.String(), Issuer: "test", ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))})
	raw, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := iss.Parse(raw); err == nil {
		t.Error("alg=none accepted")
	}
	if _, err := iss.Parse(tok[:len(tok)-2] + "xx"); err == nil {
		t.Error("tampered signature accepted")
	}
}

func TestRefreshTokenEntropyAndHash(t *testing.T) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 43 || len(hash) != 64 || HashRefreshToken(raw) != hash {
		t.Fatalf("unexpected token/hash: %q %q", raw, hash)
	}
	raw2, _, _ := NewRefreshToken()
	if raw == raw2 {
		t.Fatal("tokens must be unique")
	}
}

func TestNormalizeEmail(t *testing.T) {
	good := map[string]string{" Alice@Example.COM ": "alice@example.com", "a.b+c@sub.example.org": "a.b+c@sub.example.org"}
	for in, want := range good {
		got, err := NormalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "nope", "a@b", "Alice <a@example.com>", "a@example.com\r\nBcc: x@y.z"} {
		if _, err := NormalizeEmail(in); err == nil {
			t.Errorf("NormalizeEmail(%q) should fail", in)
		}
	}
}
