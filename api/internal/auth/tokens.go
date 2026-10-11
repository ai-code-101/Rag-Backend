package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const accessAudience = "peakchat-dashboard"

var ErrInvalidToken = errors.New("invalid token")

type AccessClaims struct {
	Verified bool `json:"ev"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	secret []byte
	issuer string
	ttl    time.Duration
}

func NewTokenIssuer(secret []byte, issuer string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, issuer: issuer, ttl: ttl}
}

func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

func (t *TokenIssuer) Issue(userID uuid.UUID, verified bool, now time.Time) (string, error) {
	claims := AccessClaims{
		Verified: verified,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    t.issuer,
			Audience:  jwt.ClaimStrings{accessAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
			ID:        uuid.NewString(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

func (t *TokenIssuer) Parse(raw string) (*AccessClaims, error) {
	claims := &AccessClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(tok *jwt.Token) (any, error) {
		return t.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(accessAudience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if _, err := uuid.Parse(claims.Subject); err != nil {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// NewRefreshToken returns a 256-bit random opaque token and its SHA-256 hash. Only the
// hash is stored; the token has enough entropy that no pepper or slow hash is needed.
func NewRefreshToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, HashRefreshToken(raw), nil
}

func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
