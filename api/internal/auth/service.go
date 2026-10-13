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

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrEmailTaken         = errors.New("an account with this email already exists")
	ErrInvalidEmail       = errors.New("invalid email address")
	ErrSessionExpired     = errors.New("session expired")
)

// refreshReuseGrace tolerates two tabs refreshing concurrently with the same token.
const refreshReuseGrace = 15 * time.Second

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
	refreshTTL time.Duration
	log        *slog.Logger
	Now        func() time.Time
	dummyHash  string
}

type Options struct {
	DB         *gorm.DB
	Hasher     *Hasher
	Tokens     *TokenIssuer
	RefreshTTL time.Duration
	Log        *slog.Logger
}

func NewService(o Options) (*Service, error) {
	s := &Service{
		db: o.DB, hasher: o.Hasher, tokens: o.Tokens, refreshTTL: o.RefreshTTL, log: o.Log,
		Now: func() time.Time { return time.Now().UTC() },
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

// Signup creates an account and signs the user in. There is no email verification step,
// so an address that is already registered returns ErrEmailTaken.
func (s *Service) Signup(ctx context.Context, rawEmail, password string, ci ClientInfo) (*Session, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return nil, err
	}
	u := &models.User{Email: email, PasswordHash: hash}
	err = s.db.WithContext(ctx).Create(u).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, ErrEmailTaken
	}
	if err != nil {
		return nil, err
	}
	return s.newSession(ctx, u, uuid.New(), ci)
}

// Login checks the password and starts a new session (refresh-token family).
func (s *Service) Login(ctx context.Context, rawEmail, password string, ci ClientInfo) (*Session, error) {
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
		// Same argon2id cost as a real check, so response time doesn't reveal whether the email exists.
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
	return s.newSession(ctx, u, uuid.New(), ci)
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
	access, err := s.tokens.Issue(u.ID, now)
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
