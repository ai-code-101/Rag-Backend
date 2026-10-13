package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
)

// Rate-limit rules for auth endpoints. Per-email rules use a normalised address.
var (
	ruleAuthIP     = ratelimit.Rule{Limit: 30, Window: time.Minute}
	ruleLoginEmail = ratelimit.Rule{Limit: 10, Window: 15 * time.Minute}
	ruleSignupIP   = ratelimit.Rule{Limit: 10, Window: time.Hour}
	ruleRefreshIP  = ratelimit.Rule{Limit: 60, Window: time.Minute}
)

type sessionResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        userView  `json:"user"`
}

type userView struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func viewUser(u *models.User) userView {
	return userView{ID: u.ID.String(), Email: u.Email, CreatedAt: u.CreatedAt}
}

func (h *Handler) clientInfo(c *gin.Context) auth.ClientInfo {
	return auth.ClientInfo{IP: c.ClientIP(), UserAgent: c.Request.UserAgent()}
}

func (h *Handler) setRefreshCookie(c *gin.Context, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	ss := http.SameSiteStrictMode
	switch h.cfg.CookieSameSite {
	case "lax":
		ss = http.SameSiteLaxMode
	case "none":
		ss = http.SameSiteNoneMode
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: h.cfg.CookieName, Value: value, Path: h.cfg.CookiePath, Domain: h.cfg.CookieDomain,
		MaxAge: maxAge, HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: ss,
	})
	// Readable hint (no secret) so the dashboard only attempts a session refresh when one
	// may exist, sparing anonymous visitors a guaranteed-401 request.
	hint := "1"
	if value == "" {
		hint = ""
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: "hv_has_session", Value: hint, Path: "/", Domain: h.cfg.CookieDomain,
		MaxAge: maxAge, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) writeSession(c *gin.Context, status int, s *auth.Session) {
	h.setRefreshCookie(c, s.RefreshToken, s.RefreshExpiresAt)
	c.JSON(status, sessionResponse{AccessToken: s.AccessToken, ExpiresAt: s.AccessExpiresAt, User: viewUser(s.User)})
}

// emailKey normalises an address for rate-limit keys; invalid input falls back to the raw
// lower-cased string so it is still limited.
func emailKey(raw string) string {
	if e, err := auth.NormalizeEmail(raw); err == nil {
		return e
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func (h *Handler) authError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		fail(c, http.StatusUnauthorized, "invalid_credentials", "Invalid email or password.")
	case errors.Is(err, auth.ErrEmailTaken):
		fail(c, http.StatusConflict, "email_taken", "An account with this email already exists. Sign in instead.")
	case errors.Is(err, auth.ErrInvalidEmail):
		failValidation(c, err)
	case errors.Is(err, auth.ErrWeakPassword):
		failValidation(c, err)
	case errors.Is(err, auth.ErrSessionExpired):
		h.setRefreshCookie(c, "", time.Time{})
		fail(c, http.StatusUnauthorized, "session_expired", "Your session has expired. Please sign in again.")
	default:
		h.failInternal(c, err)
	}
}

type credentialsReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) signup(c *gin.Context) {
	var req credentialsReq
	if !bindJSON(c, &req) || !h.allow(c, "signup-ip:"+c.ClientIP(), ruleSignupIP) {
		return
	}
	s, err := h.auth.Signup(c.Request.Context(), req.Email, req.Password, h.clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, http.StatusCreated, s)
}

func (h *Handler) login(c *gin.Context) {
	var req credentialsReq
	if !bindJSON(c, &req) || !h.allow(c, "login:"+emailKey(req.Email), ruleLoginEmail) {
		return
	}
	s, err := h.auth.Login(c.Request.Context(), req.Email, req.Password, h.clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, http.StatusOK, s)
}

// csrfGuard protects the cookie-authenticated endpoints. Besides SameSite=Strict, it
// requires a custom header (forcing a CORS preflight cross-origin) and, when the browser
// sends an Origin, that it is one of ours.
func (h *Handler) csrfGuard(c *gin.Context) {
	if c.GetHeader("X-Requested-With") != "horizonvalley" {
		fail(c, http.StatusForbidden, "csrf", "Missing CSRF header.")
		return
	}
	if o := c.GetHeader("Origin"); o != "" && o != h.cfg.WebOrigin && !h.corsAllowed[o] {
		fail(c, http.StatusForbidden, "csrf", "Origin not allowed.")
		return
	}
	c.Next()
}

func (h *Handler) refresh(c *gin.Context) {
	raw, _ := c.Cookie(h.cfg.CookieName)
	s, err := h.auth.Refresh(c.Request.Context(), raw, h.clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, http.StatusOK, s)
}

func (h *Handler) logout(c *gin.Context) {
	raw, _ := c.Cookie(h.cfg.CookieName)
	if err := h.auth.Logout(c.Request.Context(), raw); err != nil {
		h.failInternal(c, err)
		return
	}
	h.setRefreshCookie(c, "", time.Time{})
	c.Status(http.StatusNoContent)
}

func (h *Handler) me(c *gin.Context) {
	u, err := h.auth.GetUser(c.Request.Context(), userID(c))
	if err != nil {
		fail(c, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	c.JSON(http.StatusOK, viewUser(u))
}
