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
	ruleAuthIP      = ratelimit.Rule{Limit: 30, Window: time.Minute}
	ruleLoginEmail  = ratelimit.Rule{Limit: 10, Window: 15 * time.Minute}
	ruleOTPSendMail = ratelimit.Rule{Limit: 5, Window: time.Hour}
	ruleOTPSendIP   = ratelimit.Rule{Limit: 20, Window: time.Hour}
	ruleOTPCheckIP  = ratelimit.Rule{Limit: 20, Window: 10 * time.Minute}
	ruleOTPCheckEm  = ratelimit.Rule{Limit: 10, Window: 10 * time.Minute}
	ruleRefreshIP   = ratelimit.Rule{Limit: 60, Window: time.Minute}
)

type sessionResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	User        userView  `json:"user"`
}

type userView struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

func viewUser(u *models.User) userView {
	return userView{ID: u.ID.String(), Email: u.Email, EmailVerified: u.EmailVerified, CreatedAt: u.CreatedAt}
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
		Name: "pc_has_session", Value: hint, Path: "/", Domain: h.cfg.CookieDomain,
		MaxAge: maxAge, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) writeSession(c *gin.Context, s *auth.Session) {
	h.setRefreshCookie(c, s.RefreshToken, s.RefreshExpiresAt)
	c.JSON(http.StatusOK, sessionResponse{AccessToken: s.AccessToken, ExpiresAt: s.AccessExpiresAt, User: viewUser(s.User)})
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
	case errors.Is(err, auth.ErrInvalidCode):
		fail(c, http.StatusUnauthorized, "invalid_code", "Invalid or expired code.")
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

// accepted is the uniform response for any request that may send an email. It never
// reveals whether the account exists or a cooldown applied.
func accepted(c *gin.Context) {
	c.JSON(http.StatusAccepted, gin.H{"status": "verification_required", "retry_after": int(auth.OTPCooldown.Seconds())})
}

type credentialsReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) signup(c *gin.Context) {
	var req credentialsReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-send:"+emailKey(req.Email), ruleOTPSendMail) || !h.allow(c, "otp-send-ip:"+c.ClientIP(), ruleOTPSendIP) {
		return
	}
	if err := h.auth.Signup(c.Request.Context(), req.Email, req.Password); err != nil {
		h.authError(c, err)
		return
	}
	accepted(c)
}

func (h *Handler) login(c *gin.Context) {
	var req credentialsReq
	if !bindJSON(c, &req) || !h.allow(c, "login:"+emailKey(req.Email), ruleLoginEmail) {
		return
	}
	s, err := h.auth.Login(c.Request.Context(), req.Email, req.Password, h.clientInfo(c))
	if errors.Is(err, auth.ErrVerificationRequired) {
		accepted(c)
		return
	}
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, s)
}

type verifyReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (h *Handler) verifyEmail(c *gin.Context) {
	var req verifyReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-check-ip:"+c.ClientIP(), ruleOTPCheckIP) || !h.allow(c, "otp-check:"+emailKey(req.Email), ruleOTPCheckEm) {
		return
	}
	s, err := h.auth.VerifyEmail(c.Request.Context(), req.Email, req.Password, req.Code, h.clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, s)
}

type resendReq struct {
	Email   string            `json:"email"`
	Purpose models.OTPPurpose `json:"purpose"`
}

func (h *Handler) resendOTP(c *gin.Context) {
	var req resendReq
	if !bindJSON(c, &req) {
		return
	}
	if !req.Purpose.Valid() {
		fail(c, http.StatusBadRequest, "bad_request", "Unknown purpose.")
		return
	}
	if !h.allow(c, "otp-send:"+emailKey(req.Email), ruleOTPSendMail) || !h.allow(c, "otp-send-ip:"+c.ClientIP(), ruleOTPSendIP) {
		return
	}
	if err := h.auth.ResendOTP(c.Request.Context(), req.Email, req.Purpose); err != nil {
		h.authError(c, err)
		return
	}
	accepted(c)
}

type emailReq struct {
	Email string `json:"email"`
}

func (h *Handler) requestLoginCode(c *gin.Context) {
	var req emailReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-send:"+emailKey(req.Email), ruleOTPSendMail) || !h.allow(c, "otp-send-ip:"+c.ClientIP(), ruleOTPSendIP) {
		return
	}
	if err := h.auth.RequestLoginCode(c.Request.Context(), req.Email); err != nil {
		h.authError(c, err)
		return
	}
	accepted(c)
}

type codeReq struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

func (h *Handler) loginWithCode(c *gin.Context) {
	var req codeReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-check-ip:"+c.ClientIP(), ruleOTPCheckIP) || !h.allow(c, "otp-check:"+emailKey(req.Email), ruleOTPCheckEm) {
		return
	}
	s, err := h.auth.LoginWithCode(c.Request.Context(), req.Email, req.Code, h.clientInfo(c))
	if err != nil {
		h.authError(c, err)
		return
	}
	h.writeSession(c, s)
}

func (h *Handler) forgotPassword(c *gin.Context) {
	var req emailReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-send:"+emailKey(req.Email), ruleOTPSendMail) || !h.allow(c, "otp-send-ip:"+c.ClientIP(), ruleOTPSendIP) {
		return
	}
	if err := h.auth.ForgotPassword(c.Request.Context(), req.Email); err != nil && !errors.Is(err, auth.ErrInvalidEmail) {
		h.authError(c, err)
		return
	}
	accepted(c)
}

type resetReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (h *Handler) resetPassword(c *gin.Context) {
	var req resetReq
	if !bindJSON(c, &req) || !h.allow(c, "otp-check-ip:"+c.ClientIP(), ruleOTPCheckIP) || !h.allow(c, "otp-check:"+emailKey(req.Email), ruleOTPCheckEm) {
		return
	}
	if err := h.auth.ResetPassword(c.Request.Context(), req.Email, req.Code, req.Password); err != nil {
		h.authError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "password_reset"})
}

// csrfGuard protects the cookie-authenticated endpoints. Besides SameSite=Strict, it
// requires a custom header (forcing a CORS preflight cross-origin) and, when the browser
// sends an Origin, that it is one of ours.
func (h *Handler) csrfGuard(c *gin.Context) {
	if c.GetHeader("X-Requested-With") != "peakchat" {
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
	h.writeSession(c, s)
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
