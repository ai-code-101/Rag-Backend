package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pcmail "github.com/ai-code-101/rag-backend/api/internal/mail"
	"github.com/ai-code-101/rag-backend/api/internal/models"
)

var (
	ErrInvalidCredentials   = errors.New("invalid email or password")
	ErrVerificationRequired = errors.New("email verification required")
	ErrInvalidEmail         = errors.New("invalid email address")
	ErrSessionExpired       = errors.New("session expired")
)

// refreshReuseGrace tolerates two tabs refreshing concurrently with the same token.
const refreshReuseGrace = 15 * time.Second

type Mailer interface {
	Enqueue(msg pcmail.Message) error
}

type ClientInfo struct {
	IP        string
	UserAgent string
}

type Session struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             *models.User
	refreshID        uuid.UUID
}

type Service struct {
	db         *gorm.DB
	hasher     *Hasher
	tokens     *TokenIssuer
	pepper     []byte
	mailer     Mailer
	appName    string
	refreshTTL time.Duration
	log        *slog.Logger
	Now        func() time.Time
	dummyHash  string
}

type Options struct {
	DB         *gorm.DB
	Hasher     *Hasher
	Tokens     *TokenIssuer
	OTPPepper  []byte
	Mailer     Mailer
	AppName    string
	RefreshTTL time.Duration
	Log        *slog.Logger
}

func NewService(o Options) (*Service, error) {
	s := &Service{
		db: o.DB, hasher: o.Hasher, tokens: o.Tokens, pepper: o.OTPPepper, mailer: o.Mailer,
		appName: o.AppName, refreshTTL: o.RefreshTTL, log: o.Log, Now: func() time.Time { return time.Now().UTC() },
	}
	h, err := o.Hasher.Hash(context.Background(), "dummy-password-for-timing-0")
	if err != nil {
		return nil, err
	}
	s.dummyHash = h
	return s, nil
}

func (s *Service) Tokens() *TokenIssuer { return s.tokens }

// NormalizeEmail lower-cases and validates an email address.
func NormalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if len(e) > 254 {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || !strings.Contains(e[strings.LastIndexByte(e, '@'):], ".") {
		return "", ErrInvalidEmail
	}
	return e, nil
}

func (s *Service) findUser(ctx context.Context, email string) (*models.User, error) {
	var u models.User
	err := s.db.WithContext(ctx).Where("email = ?", email).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &u, err
}

// Signup creates an account (or refreshes an unverified one) and mails a verification
// code. It returns nil for existing verified accounts too, so callers cannot learn which
// emails are registered.
func (s *Service) Signup(ctx context.Context, rawEmail, password string) error {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(ctx, password) // always hash: equal timing for every branch
	if err != nil {
		return err
	}
	u, err := s.findUser(ctx, email)
	if err != nil {
		return err
	}
	switch {
	case u == nil:
		u = &models.User{Email: email, PasswordHash: hash}
		err := s.db.WithContext(ctx).Create(u).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil // concurrent signup for the same email
		}
		if err != nil {
			return err
		}
	case u.EmailVerified:
		return nil
	default:
		// Unverified accounts belong to nobody yet. The latest signup sets the password, and
		// verification requires that password, so squatting an address gains nothing.
		if err := s.db.WithContext(ctx).Model(u).Updates(map[string]any{"password_hash": hash, "updated_at": s.Now()}).Error; err != nil {
			return err
		}
	}
	return s.issueOTPQuiet(ctx, u, models.PurposeVerifyEmail)
}

// Login checks the password. Unverified accounts get a fresh verification code and
// ErrVerificationRequired instead of tokens.
func (s *Service) Login(ctx context.Context, rawEmail, password string, ci ClientInfo) (*Session, error) {
	u, err := s.checkPassword(ctx, rawEmail, password)
	if err != nil {
		return nil, err
	}
	if !u.EmailVerified {
		if err := s.issueOTPQuiet(ctx, u, models.PurposeVerifyEmail); err != nil {
			return nil, err
		}
		return nil, ErrVerificationRequired
	}
	return s.newSession(ctx, u, uuid.New(), ci)
}

func (s *Service) checkPassword(ctx context.Context, rawEmail, password string) (*models.User, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil || len(password) > 512 {
		_, _ = s.hasher.Verify(ctx, password, s.dummyHash)
		return nil, ErrInvalidCredentials
	}
	u, err := s.findUser(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		_, _ = s.hasher.Verify(ctx, password, s.dummyHash)
		return nil, ErrInvalidCredentials
	}
	ok, err := s.hasher.Verify(ctx, password, u.PasswordHash)
	if err != nil && !errors.Is(err, ErrMalformedHash) {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// VerifyEmail consumes a verify_email code and signs the user in. The password is
// required too (see Signup).
func (s *Service) VerifyEmail(ctx context.Context, rawEmail, password, code string, ci ClientInfo) (*Session, error) {
	u, err := s.checkPassword(ctx, rawEmail, password)
	if errors.Is(err, ErrInvalidCredentials) {
		return nil, ErrInvalidCode
	}
	if err != nil {
		return nil, err
	}
	if u.EmailVerified {
		return nil, ErrInvalidCode
	}
	if err := s.consumeOTP(ctx, u, models.PurposeVerifyEmail, code); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(u).Updates(map[string]any{"email_verified": true, "updated_at": s.Now()}).Error; err != nil {
		return nil, err
	}
	u.EmailVerified = true
	return s.newSession(ctx, u, uuid.New(), ci)
}

// ResendOTP issues a new code if the account exists and the purpose applies to it. It
// always returns nil (unless input is malformed) to avoid account enumeration.
func (s *Service) ResendOTP(ctx context.Context, rawEmail string, purpose models.OTPPurpose) error {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	if !purpose.Valid() {
		return errors.New("invalid purpose")
	}
	u, err := s.findUser(ctx, email)
	if err != nil || u == nil {
		return err
	}
	if purpose == models.PurposeVerifyEmail && u.EmailVerified {
		return nil
	}
	if purpose == models.PurposeLogin && !u.EmailVerified {
		return nil
	}
	return s.issueOTPQuiet(ctx, u, purpose)
}

// RequestLoginCode mails a passwordless sign-in code to a verified account.
func (s *Service) RequestLoginCode(ctx context.Context, rawEmail string) error {
	return s.ResendOTP(ctx, rawEmail, models.PurposeLogin)
}

func (s *Service) LoginWithCode(ctx context.Context, rawEmail, code string, ci ClientInfo) (*Session, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return nil, ErrInvalidCode
	}
	u, err := s.findUser(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil || !u.EmailVerified {
		return nil, ErrInvalidCode
	}
	if err := s.consumeOTP(ctx, u, models.PurposeLogin, code); err != nil {
		return nil, err
	}
	return s.newSession(ctx, u, uuid.New(), ci)
}

func (s *Service) ForgotPassword(ctx context.Context, rawEmail string) error {
	return s.ResendOTP(ctx, rawEmail, models.PurposeResetPassword)
}

// ResetPassword sets a new password using a reset code, marks the email verified (the
// code proves inbox ownership) and revokes every existing session.
func (s *Service) ResetPassword(ctx context.Context, rawEmail, code, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return ErrInvalidCode
	}
	hash, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return err
	}
	u, err := s.findUser(ctx, email)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrInvalidCode
	}
	if err := s.consumeOTP(ctx, u, models.PurposeResetPassword, code); err != nil {
		return err
	}
	now := s.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(u).Updates(map[string]any{"password_hash": hash, "email_verified": true, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.RefreshToken{}).Where("user_id = ? AND revoked_at IS NULL", u.ID).Update("revoked_at", now).Error
	})
}

// issueOTPQuiet issues a code but swallows the cooldown error so public responses stay uniform.
func (s *Service) issueOTPQuiet(ctx context.Context, u *models.User, purpose models.OTPPurpose) error {
	err := s.IssueOTP(ctx, u, purpose)
	if errors.Is(err, ErrCooldown) {
		return nil
	}
	return err
}

// IssueOTP creates a new code for (user, purpose), invalidates outstanding ones and mails it.
// It enforces the resend cooldown under a row lock on the user so concurrent requests
// cannot issue two codes.
func (s *Service) IssueOTP(ctx context.Context, u *models.User, purpose models.OTPPurpose) error {
	code, err := GenerateOTP()
	if err != nil {
		return err
	}
	now := s.Now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked models.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", u.ID).Take(&locked).Error; err != nil {
			return err
		}
		var last models.EmailOTP
		err := tx.Where("user_id = ? AND purpose = ?", u.ID, purpose).Order("created_at DESC").Limit(1).Take(&last).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && CooldownRemaining(last.CreatedAt, now) > 0 {
			return ErrCooldown
		}
		if err := tx.Model(&models.EmailOTP{}).
			Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", u.ID, purpose).
			Update("consumed_at", now).Error; err != nil {
			return err
		}
		return tx.Create(&models.EmailOTP{
			UserID:    u.ID,
			Purpose:   purpose,
			CodeHash:  HashOTP(s.pepper, u.ID.String(), purpose, code),
			ExpiresAt: now.Add(OTPTTL),
			CreatedAt: now,
		}).Error
	})
	if err != nil {
		return err
	}
	msg, err := pcmail.OTPEmail(s.appName, u.Email, purpose, code, int(OTPTTL/time.Minute))
	if err != nil {
		return err
	}
	if err := s.mailer.Enqueue(msg); err != nil {
		s.log.Error("enqueue otp email", "error", err)
	}
	return nil
}

// consumeOTP validates a code against the newest outstanding code for (user, purpose).
// Wrong guesses increment attempts; the 5th wrong guess burns the code.
func (s *Service) consumeOTP(ctx context.Context, u *models.User, purpose models.OTPPurpose, code string) error {
	now := s.Now()
	var result OTPCheck
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rec models.EmailOTP
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", u.ID, purpose).
			Order("created_at DESC").Limit(1).Take(&rec).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = OTPUnusable
			return nil
		}
		if err != nil {
			return err
		}
		result = CheckOTP(&rec, s.pepper, strings.TrimSpace(code), now)
		switch result {
		case OTPOK:
			return tx.Model(&rec).Update("consumed_at", now).Error
		case OTPWrong:
			return tx.Model(&rec).Update("attempts", gorm.Expr("attempts + 1")).Error
		}
		return nil
	})
	if err != nil {
		return err
	}
	if result != OTPOK {
		return ErrInvalidCode
	}
	return nil
}

func (s *Service) newSession(ctx context.Context, u *models.User, family uuid.UUID, ci ClientInfo) (*Session, error) {
	return s.newSessionTx(s.db.WithContext(ctx), u, family, ci)
}

func (s *Service) newSessionTx(tx *gorm.DB, u *models.User, family uuid.UUID, ci ClientInfo) (*Session, error) {
	now := s.Now()
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}
	rt := &models.RefreshToken{
		ID: uuid.New(), UserID: u.ID, FamilyID: family, TokenHash: hash,
		ExpiresAt: now.Add(s.refreshTTL), UserAgent: truncate(ci.UserAgent, 255), IP: truncate(ci.IP, 64), CreatedAt: now,
	}
	if err := tx.Create(rt).Error; err != nil {
		return nil, err
	}
	access, err := s.tokens.Issue(u.ID, u.EmailVerified, now)
	if err != nil {
		return nil, err
	}
	return &Session{
		AccessToken: access, AccessExpiresAt: now.Add(s.tokens.TTL()),
		RefreshToken: raw, RefreshExpiresAt: rt.ExpiresAt, User: u, refreshID: rt.ID,
	}, nil
}

// Refresh rotates a refresh token. Presenting a token that was already rotated (outside a
// short race window) is treated as theft: the whole token family is revoked.
func (s *Service) Refresh(ctx context.Context, raw string, ci ClientInfo) (*Session, error) {
	if raw == "" {
		return nil, ErrSessionExpired
	}
	now := s.Now()
	hash := HashRefreshToken(raw)
	var sess *Session
	var reuse *models.RefreshToken
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rt models.RefreshToken
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", hash).Take(&rt).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSessionExpired
		}
		if err != nil {
			return err
		}
		if rt.RevokedAt != nil {
			benignRace := rt.ReplacedBy != nil && now.Sub(*rt.RevokedAt) < refreshReuseGrace
			if !benignRace {
				reuse = &rt
				return ErrSessionExpired
			}
		}
		if !now.Before(rt.ExpiresAt) {
			return ErrSessionExpired
		}
		var u models.User
		if err := tx.Where("id = ?", rt.UserID).Take(&u).Error; err != nil {
			return ErrSessionExpired
		}
		sess, err = s.newSessionTx(tx, &u, rt.FamilyID, ci)
		if err != nil {
			return err
		}
		if rt.RevokedAt == nil {
			return tx.Model(&rt).Updates(map[string]any{"revoked_at": now, "replaced_by": sess.refreshID}).Error
		}
		return nil
	})
	if reuse != nil {
		s.log.Warn("refresh token reuse detected; revoking family", "user_id", reuse.UserID, "family_id", reuse.FamilyID, "ip", ci.IP)
		if rerr := s.revokeFamily(ctx, reuse.FamilyID); rerr != nil {
			s.log.Error("revoke token family", "error", rerr)
		}
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// Logout revokes the token's whole family (this device's session chain).
func (s *Service) Logout(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	var rt models.RefreshToken
	err := s.db.WithContext(ctx).Select("family_id").Where("token_hash = ?", HashRefreshToken(raw)).Take(&rt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.revokeFamily(ctx, rt.FamilyID)
}

func (s *Service) revokeFamily(ctx context.Context, family uuid.UUID) error {
	return s.db.WithContext(ctx).Model(&models.RefreshToken{}).
		Where("family_id = ? AND revoked_at IS NULL", family).
		Update("revoked_at", s.Now()).Error
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*models.User, error) {
	var u models.User
	if err := s.db.WithContext(ctx).Where("id = ?", id).Take(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
