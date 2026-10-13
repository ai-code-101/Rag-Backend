// Package config loads and validates service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string // development | production | test
	Port     string
	LogLevel string

	DatabaseURL     string
	DBMaxOpenConns  int
	DBMaxIdleConns  int
	DBConnMaxLife   time.Duration
	MigrateOnStart  bool
	EmbeddingDim    int
	TrustedProxies  []string
	ClientIPHeaders []string

	// Public origin of the web app (dashboard + widget iframe), e.g. https://app.example.com.
	WebOrigin string
	// Extra origins allowed to call dashboard endpoints with credentials (CORS).
	CORSOrigins []string

	JWTSecret       []byte
	JWTIssuer       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	CookieName     string
	CookiePath     string
	CookieDomain   string
	CookieSecure   bool
	CookieSameSite string

	RedisURL string

	StorageDriver   string // gcs | local
	GCSBucket       string
	LocalStorageDir string
	MaxUploadBytes  int64
	MaxTextBytes    int64

	RAGURL         string
	RAGToken       string
	RAGTimeout     time.Duration
	RAGIDTokenAud  string // set to the rag URL to send Cloud Run IAM ID tokens
	HistoryTurns   int
	AllowedModels  []string
	DefaultModel   string
	MaxBotsPerUser int
	MaxDocsPerBot  int
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

// Load reads configuration from the environment. Missing required values return an error.
func Load() (*Config, error) {
	c := &Config{
		Env:             getEnv("APP_ENV", "development"),
		Port:            getEnv("PORT", "8080"),
		LogLevel:        getEnv("LOG_LEVEL", "info"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxOpenConns:  getInt("DB_MAX_OPEN_CONNS", 20),
		DBMaxIdleConns:  getInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLife:   getDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		MigrateOnStart:  getBool("MIGRATE_ON_START", true),
		EmbeddingDim:    getInt("EMBEDDING_DIM", 1536),
		TrustedProxies:  getList("TRUSTED_PROXIES", nil),
		ClientIPHeaders: getList("CLIENT_IP_HEADERS", []string{"X-Forwarded-For", "X-Real-IP"}),
		WebOrigin:       strings.TrimRight(getEnv("WEB_ORIGIN", "http://localhost:3000"), "/"),
		CORSOrigins:     getList("CORS_ORIGINS", nil),
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:       getEnv("JWT_ISSUER", "horizon-valley-api"),
		AccessTokenTTL:  getDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL: getDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		CookiePath:      getEnv("COOKIE_PATH", "/api/v1/auth"),
		CookieDomain:    os.Getenv("COOKIE_DOMAIN"),
		CookieSecure:    getBool("COOKIE_SECURE", true),
		CookieSameSite:  strings.ToLower(getEnv("COOKIE_SAMESITE", "strict")),
		RedisURL:        os.Getenv("REDIS_URL"),
		StorageDriver:   getEnv("STORAGE_DRIVER", "local"),
		GCSBucket:       os.Getenv("GCS_BUCKET"),
		LocalStorageDir: getEnv("LOCAL_STORAGE_DIR", "./data/uploads"),
		MaxUploadBytes:  int64(getInt("MAX_UPLOAD_MB", 20)) << 20,
		MaxTextBytes:    int64(getInt("MAX_TEXT_KB", 512)) << 10,
		RAGURL:          strings.TrimRight(getEnv("RAG_URL", "http://localhost:8000"), "/"),
		RAGToken:        os.Getenv("RAG_INTERNAL_TOKEN"),
		RAGTimeout:      getDuration("RAG_TIMEOUT", 120*time.Second),
		RAGIDTokenAud:   os.Getenv("RAG_ID_TOKEN_AUDIENCE"),
		HistoryTurns:    getInt("CHAT_HISTORY_TURNS", 6),
		// Model ids are "<provider>:<model>" (openrouter:, openai:) or bare claude-* for Anthropic.
		AllowedModels:  getList("ALLOWED_MODELS", []string{"openrouter:openai/gpt-4o-mini"}),
		DefaultModel:   getEnv("DEFAULT_MODEL", "openrouter:openai/gpt-4o-mini"),
		MaxBotsPerUser: getInt("MAX_BOTS_PER_USER", 20),
		MaxDocsPerBot:  getInt("MAX_DOCS_PER_BOT", 500),
	}
	if c.CookieSecure {
		c.CookieName = getEnv("COOKIE_NAME", "__Secure-hv_rt")
	} else {
		c.CookieName = getEnv("COOKIE_NAME", "hv_rt")
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if len(c.JWTSecret) < 32 {
		errs = append(errs, errors.New("JWT_SECRET must be at least 32 bytes"))
	}
	if c.RAGToken == "" || len(c.RAGToken) < 24 {
		errs = append(errs, errors.New("RAG_INTERNAL_TOKEN must be at least 24 characters"))
	}
	if c.EmbeddingDim <= 0 || c.EmbeddingDim > 2000 {
		errs = append(errs, errors.New("EMBEDDING_DIM must be between 1 and 2000 (HNSW limit)"))
	}
	switch c.StorageDriver {
	case "gcs":
		if c.GCSBucket == "" {
			errs = append(errs, errors.New("GCS_BUCKET is required when STORAGE_DRIVER=gcs"))
		}
	case "local":
	default:
		errs = append(errs, fmt.Errorf("unknown STORAGE_DRIVER %q", c.StorageDriver))
	}
	switch c.CookieSameSite {
	case "strict", "lax", "none":
	default:
		errs = append(errs, errors.New("COOKIE_SAMESITE must be strict, lax or none"))
	}
	if c.IsProduction() && !c.CookieSecure {
		errs = append(errs, errors.New("COOKIE_SECURE must be true in production"))
	}
	if !contains(c.AllowedModels, c.DefaultModel) {
		errs = append(errs, errors.New("DEFAULT_MODEL must be one of ALLOWED_MODELS"))
	}
	return errors.Join(errs...)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getList(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
