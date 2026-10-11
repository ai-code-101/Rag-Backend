package auth_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	pcmail "github.com/ai-code-101/rag-backend/api/internal/mail"
	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/testutil"
	"gorm.io/gorm"
)

type captureMailer struct {
	mu   sync.Mutex
	msgs []pcmail.Message
}

func (m *captureMailer) Enqueue(msg pcmail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

func (m *captureMailer) lastCode(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) == 0 {
		t.Fatal("no email sent")
	}
	match := codeRe.FindStringSubmatch(m.msgs[len(m.msgs)-1].Text)
	if match == nil {
		t.Fatal("no code in email")
	}
	return match[1]
}

func (m *captureMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

type env struct {
	svc  *auth.Service
	db   *gorm.DB
	mail *captureMailer
	now  time.Time
}

// advance moves the service clock forward (e.g. past the resend cooldown).
func (e *env) advance(d time.Duration) { e.now = e.now.Add(d) }

func newEnv(t *testing.T) *env {
	db, _ := testutil.NewDB(t)
	m := &captureMailer{}
	svc, err := auth.NewService(auth.Options{
		DB: db, Hasher: auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4),
		Tokens:    auth.NewTokenIssuer([]byte(strings.Repeat("j", 32)), "test", 15*time.Minute),
		OTPPepper: []byte(strings.Repeat("p", 32)), Mailer: m, AppName: "PeakChat", RefreshTTL: 30 * 24 * time.Hour,
		Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{svc: svc, db: db, mail: m, now: time.Now().UTC()}
	svc.Now = func() time.Time { return e.now }
	return e
}

var ci = auth.ClientInfo{IP: "203.0.113.7", UserAgent: "test"}

const pw = "s3cret-password"

func (e *env) signupVerified(t *testing.T, email string) *auth.Session {
	t.Helper()
	ctx := context.Background()
	if err := e.svc.Signup(ctx, email, pw); err != nil {
		t.Fatal(err)
	}
	s, err := e.svc.VerifyEmail(ctx, email, pw, e.mail.lastCode(t), ci)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignupVerifyLogin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := e.svc.Signup(ctx, "New@Example.com", pw); err != nil {
		t.Fatal(err)
	}
	// Unverified accounts get no tokens; login re-sends a code after the cooldown.
	e.advance(61 * time.Second)
	if _, err := e.svc.Login(ctx, "new@example.com", pw, ci); !errors.Is(err, auth.ErrVerificationRequired) {
		t.Fatalf("expected verification required, got %v", err)
	}
	if e.mail.count() != 2 {
		t.Fatalf("expected 2 emails, got %d", e.mail.count())
	}
	code := e.mail.lastCode(t)
	if _, err := e.svc.VerifyEmail(ctx, "new@example.com", "wrong-password1", code, ci); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatalf("verification must require the account password, got %v", err)
	}
	s, err := e.svc.VerifyEmail(ctx, "new@example.com", pw, code, ci)
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken == "" || s.RefreshToken == "" || !s.User.EmailVerified {
		t.Fatalf("bad session: %+v", s)
	}
	// Codes are single use.
	if _, err := e.svc.VerifyEmail(ctx, "new@example.com", pw, code, ci); err == nil {
		t.Fatal("code reused")
	}
	if _, err := e.svc.Login(ctx, "new@example.com", pw, ci); err != nil {
		t.Fatalf("login after verify: %v", err)
	}
	if _, err := e.svc.Login(ctx, "new@example.com", "bad-password1", ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	if _, err := e.svc.Login(ctx, "nobody@example.com", pw, ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown email must look like a bad password, got %v", err)
	}
}

func TestSignupDoesNotRevealExistingAccounts(t *testing.T) {
	e := newEnv(t)
	e.signupVerified(t, "taken@example.com")
	before := e.mail.count()
	if err := e.svc.Signup(context.Background(), "taken@example.com", "another-pass1"); err != nil {
		t.Fatalf("signup for an existing email must look successful, got %v", err)
	}
	if e.mail.count() != before {
		t.Fatal("no code should be sent for an already-verified account")
	}
	// The original password still works.
	if _, err := e.svc.Login(context.Background(), "taken@example.com", pw, ci); err != nil {
		t.Fatal(err)
	}
}

func TestSquattedUnverifiedEmailCannotBeHijacked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.Signup(ctx, "victim@example.com", "attacker-pass1"); err != nil {
		t.Fatal(err)
	}
	e.advance(61 * time.Second)
	// The real owner signs up later with their own password and verifies.
	if err := e.svc.Signup(ctx, "victim@example.com", "owner-pass1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VerifyEmail(ctx, "victim@example.com", "owner-pass1", e.mail.lastCode(t), ci); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, "victim@example.com", "attacker-pass1", ci); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("attacker password must not work, got %v", err)
	}
}

func TestOTPAttemptsCooldownAndInvalidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.signupVerified(t, "otp@example.com")

	if err := e.svc.RequestLoginCode(ctx, "otp@example.com"); err != nil {
		t.Fatal(err)
	}
	first := e.mail.lastCode(t)
	n := e.mail.count()
	// Within the 60s cooldown no new code is issued (but the call still "succeeds").
	if err := e.svc.RequestLoginCode(ctx, "otp@example.com"); err != nil {
		t.Fatal(err)
	}
	if e.mail.count() != n {
		t.Fatal("cooldown not enforced")
	}
	// A new code invalidates the previous one.
	e.advance(61 * time.Second)
	if err := e.svc.RequestLoginCode(ctx, "otp@example.com"); err != nil {
		t.Fatal(err)
	}
	second := e.mail.lastCode(t)
	if first != second {
		if _, err := e.svc.LoginWithCode(ctx, "otp@example.com", first, ci); err == nil {
			t.Fatal("previous code still valid after re-issue")
		}
	}
	// 5 wrong guesses burn the code, even if the 6th guess is right.
	wrong := "000000"
	if second == wrong {
		wrong = "111111"
	}
	for i := 0; i < auth.OTPMaxAttempts; i++ {
		if _, err := e.svc.LoginWithCode(ctx, "otp@example.com", wrong, ci); !errors.Is(err, auth.ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := e.svc.LoginWithCode(ctx, "otp@example.com", second, ci); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatal("code must be unusable after max attempts")
	}
	// Expiry: a fresh code stops working after 10 minutes.
	e.advance(61 * time.Second)
	_ = e.svc.RequestLoginCode(ctx, "otp@example.com")
	third := e.mail.lastCode(t)
	e.advance(auth.OTPTTL + time.Second)
	if _, err := e.svc.LoginWithCode(ctx, "otp@example.com", third, ci); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatal("expired code accepted")
	}
	// The OTP is stored only as a hash.
	var stored []models.EmailOTP
	e.db.Find(&stored)
	for _, o := range stored {
		if strings.Contains(o.CodeHash, third) || len(o.CodeHash) != 64 {
			t.Fatalf("code stored insecurely: %q", o.CodeHash)
		}
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.signupVerified(t, "rt@example.com")

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
	s := e.signupVerified(t, "lo@example.com")
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

func TestPasswordReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.signupVerified(t, "reset@example.com")
	e.advance(61 * time.Second)

	if err := e.svc.ForgotPassword(ctx, "ghost@example.com"); err != nil {
		t.Fatal("unknown email must not error")
	}
	if err := e.svc.ForgotPassword(ctx, "reset@example.com"); err != nil {
		t.Fatal(err)
	}
	code := e.mail.lastCode(t)
	if err := e.svc.ResetPassword(ctx, "reset@example.com", code, "weak"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password accepted: %v", err)
	}
	if err := e.svc.ResetPassword(ctx, "reset@example.com", code, "brand-new-pass9"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Refresh(ctx, s.RefreshToken, ci); err == nil {
		t.Fatal("sessions must be revoked on password reset")
	}
	if _, err := e.svc.Login(ctx, "reset@example.com", pw, ci); err == nil {
		t.Fatal("old password still works")
	}
	if _, err := e.svc.Login(ctx, "reset@example.com", "brand-new-pass9", ci); err != nil {
		t.Fatal(err)
	}
	// A login code cannot be used as a reset code (purpose binding).
	e.advance(61 * time.Second)
	_ = e.svc.RequestLoginCode(ctx, "reset@example.com")
	if err := e.svc.ResetPassword(ctx, "reset@example.com", e.mail.lastCode(t), "another-pass9"); !errors.Is(err, auth.ErrInvalidCode) {
		t.Fatal("login code accepted for password reset")
	}
}
