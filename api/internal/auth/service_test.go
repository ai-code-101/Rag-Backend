package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/testutil"
	"gorm.io/gorm"
)

type env struct {
	svc *auth.Service
	db  *gorm.DB
	now time.Time
}

// advance moves the service clock forward.
func (e *env) advance(d time.Duration) { e.now = e.now.Add(d) }

func newEnv(t *testing.T) *env {
	db, _ := testutil.NewDB(t)
	svc, err := auth.NewService(auth.Options{
		DB: db, Hasher: auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4),
		Tokens:     auth.NewTokenIssuer([]byte(strings.Repeat("j", 32)), "test", 15*time.Minute),
		RefreshTTL: 30 * 24 * time.Hour, Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{svc: svc, db: db, now: time.Now().UTC()}
	svc.Now = func() time.Time { return e.now }
	return e
}

var ci = auth.ClientInfo{IP: "203.0.113.7", UserAgent: "test"}

const pw = "s3cret-password"

func (e *env) signup(t *testing.T, email string) *auth.Session {
	t.Helper()
	s, err := e.svc.Signup(context.Background(), email, pw, ci)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignupAndLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	s := e.signup(t, "New@Example.com")
	if s.AccessToken == "" || s.RefreshToken == "" || s.User.Email != "new@example.com" {
		t.Fatalf("bad session: %+v", s)
	}
	if _, err := e.svc.Signup(ctx, "new@example.com", "another-pass1", ci); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate signup: got %v", err)
	}
	if _, err := e.svc.Signup(ctx, "weak@example.com", "short", ci); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password accepted: %v", err)
	}
	if _, err := e.svc.Signup(ctx, "not-an-email", pw, ci); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Fatalf("invalid email accepted: %v", err)
	}
	if _, err := e.svc.Login(ctx, "NEW@example.com", pw, ci); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := e.svc.Login(ctx, "new@example.com", "bad-password1", ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	if _, err := e.svc.Login(ctx, "nobody@example.com", pw, ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown email must look like a bad password, got %v", err)
	}
	// Passwords are stored as argon2id hashes only.
	var u models.User
	e.db.Where("email = ?", "new@example.com").Take(&u)
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$") || strings.Contains(u.PasswordHash, pw) {
		t.Fatalf("password stored insecurely: %q", u.PasswordHash)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.signup(t, "rt@example.com")

	s2, err := e.svc.Refresh(ctx, s.RefreshToken, ci)
	if err != nil {
		t.Fatal(err)
	}
	if s2.RefreshToken == s.RefreshToken {
		t.Fatal("refresh token not rotated")
	}
	// Concurrent-tab race inside the grace window is tolerated.
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, ci); err != nil {
		t.Fatalf("benign race rejected: %v", err)
	}
	s3, err := e.svc.Refresh(ctx, s2.RefreshToken, ci)
	if err != nil {
		t.Fatal(err)
	}
	// Replaying an old token after the grace window revokes the whole family.
	e.advance(time.Minute)
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, ci); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("reuse not detected: %v", err)
	}
	if _, err := e.svc.Refresh(ctx, s3.RefreshToken, ci); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatal("family not revoked after reuse")
	}
	// Tokens are stored hashed.
	var n int64
	e.db.Model(&models.RefreshToken{}).Where("token_hash = ?", s3.RefreshToken).Count(&n)
	if n != 0 {
		t.Fatal("raw refresh token stored")
	}
}

func TestRefreshExpiryAndLogout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.signup(t, "lo@example.com")
	if err := e.svc.Logout(ctx, s.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, ci); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatal("logged-out token still works")
	}
	s, _ = e.svc.Login(ctx, "lo@example.com", pw, ci)
	e.advance(31 * 24 * time.Hour)
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, ci); !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatal("expired refresh token accepted")
	}
}
