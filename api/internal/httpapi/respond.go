package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ai-code-101/rag-backend/api/internal/bots"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func fail(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": apiError{Code: code, Message: msg}})
}

func failValidation(c *gin.Context, err error) {
	var ve *bots.ValidationError
	if errors.As(err, &ve) {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": apiError{Code: "validation_error", Message: ve.Msg, Field: ve.Field}})
		return
	}
	fail(c, http.StatusUnprocessableEntity, "validation_error", err.Error())
}

// failInternal logs the real error and returns a generic message.
func (h *Handler) failInternal(c *gin.Context, err error) {
	h.log.ErrorContext(c.Request.Context(), "internal error", "error", err, "path", c.FullPath(), "request_id", c.GetString(ctxRequestID))
	fail(c, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
}

func (h *Handler) failStore(c *gin.Context, err error) {
	switch {
	case errors.Is(err, bots.ErrNotFound):
		fail(c, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, bots.ErrLimitReached):
		fail(c, http.StatusForbidden, "limit_reached", "Plan limit reached.")
	case errors.Is(err, bots.ErrVersionClash):
		fail(c, http.StatusConflict, "conflict", err.Error())
	default:
		var ve *bots.ValidationError
		if errors.As(err, &ve) {
			failValidation(c, err)
			return
		}
		h.failInternal(c, err)
	}
}

func tooMany(c *gin.Context, retry time.Duration) {
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.Itoa(secs))
	fail(c, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please slow down.")
}

// bindJSON decodes a JSON body strictly (unknown fields rejected, size-limited by middleware).
func bindJSON(c *gin.Context, v any) bool {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		fail(c, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large.")
		return false
	}
	if err := bots.DecodeStrict(body, v); err != nil {
		fail(c, http.StatusBadRequest, "bad_request", "Malformed JSON body.")
		return false
	}
	return true
}
