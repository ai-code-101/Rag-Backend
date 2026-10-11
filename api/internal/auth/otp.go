package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

const (
	OTPLength      = 6
	OTPTTL         = 10 * time.Minute
	OTPMaxAttempts = 5
	OTPCooldown    = 60 * time.Second
)

var (
	ErrInvalidCode = errors.New("invalid or expired code")
	ErrCooldown    = errors.New("please wait before requesting another code")
)

// GenerateOTP returns a uniformly distributed 6-digit code from crypto/rand.
func GenerateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashOTP binds the code to the user and purpose and keys it with a server-side pepper,
// so a leaked table cannot be brute-forced offline (the code space is only 10^6).
func HashOTP(pepper []byte, userID string, purpose models.OTPPurpose, code string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(userID))
	m.Write([]byte{0})
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(code))
	return hex.EncodeToString(m.Sum(nil))
}

// OTPCheck is the outcome of evaluating a submitted code against a stored record.
type OTPCheck int

const (
	OTPOK OTPCheck = iota
	OTPWrong
	OTPUnusable // expired, consumed or out of attempts
)

// CheckOTP is the pure decision function behind verification. The caller must persist
// the attempt increment (on OTPWrong) or consumption (on OTPOK).
func CheckOTP(rec *models.EmailOTP, pepper []byte, code string, now time.Time) OTPCheck {
	if rec == nil || rec.ConsumedAt != nil || !now.Before(rec.ExpiresAt) || rec.Attempts >= OTPMaxAttempts {
		return OTPUnusable
	}
	if len(code) != OTPLength {
		return OTPWrong
	}
	want, err := hex.DecodeString(rec.CodeHash)
	if err != nil {
		return OTPUnusable
	}
	got, _ := hex.DecodeString(HashOTP(pepper, rec.UserID.String(), rec.Purpose, code))
	if hmac.Equal(want, got) {
		return OTPOK
	}
	return OTPWrong
}

// CooldownRemaining reports how long until a new code may be issued, given the most
// recent code's creation time.
func CooldownRemaining(lastCreated time.Time, now time.Time) time.Duration {
	if lastCreated.IsZero() {
		return 0
	}
	if d := lastCreated.Add(OTPCooldown).Sub(now); d > 0 {
		return d
	}
	return 0
}
