package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
)

const (
	ctxRequestID = "request_id"
	ctxUserID    = "user_id"
)

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" || len(id) > 64 {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		c.Set(ctxRequestID, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func accessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "/healthz" {
			return
		}
		if route == "" {
			route = "unmatched"
		}
		lvl := slog.LevelInfo
		if c.Writer.Status() >= 500 {
			lvl = slog.LevelError
		}
		log.Log(c.Request.Context(), lvl, "request",
			"method", c.Request.Method, "route", route, "status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(), "ip", c.ClientIP(),
			"request_id", c.GetString(ctxRequestID), "bytes", c.Writer.Size())
	}
}

func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, err any) {
		log.Error("panic", "error", err, "route", c.FullPath(), "request_id", c.GetString(ctxRequestID))
		fail(c, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	})
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		c.Next()
	}
}

func maxBody(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}

func noStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

// dashboardCORS allows the configured dashboard origins with credentials. In the default
// deployment the dashboard calls through a same-origin rewrite and CORS is not exercised.
func dashboardCORS(origins []string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/v1/widget/") {
			c.Next() // public widget routes have their own per-bot CORS policy
			return
		}
		origin := c.GetHeader("Origin")
		if origin != "" && allowed[origin] {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if c.Request.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
				h.Set("Access-Control-Max-Age", "600")
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
		}
		c.Next()
	}
}

// requireAuth validates the bearer access token. Tokens are only ever issued to verified
// users, and the "ev" claim is checked as defence in depth.
func requireAuth(tokens *auth.TokenIssuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.GetHeader("Authorization")
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || raw == "" {
			fail(c, http.StatusUnauthorized, "unauthorized", "Authentication required.")
			return
		}
		claims, err := tokens.Parse(raw)
		if err != nil {
			fail(c, http.StatusUnauthorized, "unauthorized", "Authentication required.")
			return
		}
		if !claims.Verified {
			fail(c, http.StatusForbidden, "email_unverified", "Verify your email first.")
			return
		}
		c.Set(ctxUserID, uuid.MustParse(claims.Subject))
		c.Next()
	}
}

func userID(c *gin.Context) uuid.UUID { return c.MustGet(ctxUserID).(uuid.UUID) }

// limit applies a rate-limit rule to a key derived from the request.
func (h *Handler) limit(name string, rule ratelimit.Rule, key func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		k := key(c)
		if k == "" {
			c.Next()
			return
		}
		if !h.allow(c, name+":"+k, rule) {
			return
		}
		c.Next()
	}
}

// allow checks a rule and writes a 429 if exceeded. Returns false when the request was aborted.
func (h *Handler) allow(c *gin.Context, key string, rule ratelimit.Rule) bool {
	res, err := h.limiter.Allow(c.Request.Context(), key, rule)
	if err != nil {
		h.log.Warn("rate limiter error", "error", err)
	}
	if !res.Allowed {
		tooMany(c, res.RetryAfter)
		return false
	}
	return true
}

func byIP(c *gin.Context) string { return c.ClientIP() }
