package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
)

const ctxPublicBot = "public_bot"

var (
	ruleWidgetConfigIP = ratelimit.Rule{Limit: 120, Window: time.Minute}
	ruleChatIP         = ratelimit.Rule{Limit: 30, Window: time.Minute}
	ruleChatVisitor    = ratelimit.Rule{Limit: 15, Window: time.Minute}
	ruleChatBot        = ratelimit.Rule{Limit: 600, Window: time.Minute}
	ruleLeadVisitor    = ratelimit.Rule{Limit: 5, Window: time.Hour}
	ruleLeadIP         = ratelimit.Rule{Limit: 20, Window: time.Hour}
	ruleHistoryIP      = ratelimit.Rule{Limit: 60, Window: time.Minute}
)

// widgetBot resolves the bot from the public key and enforces the origin allow-list. The
// web origin (which serves the chat iframe) is always accepted: frame-ancestors on the
// iframe already restricts which sites can embed it.
func (h *Handler) widgetBot(c *gin.Context) {
	pb, err := h.store.PublicBotByKey(c.Request.Context(), c.Param("key"))
	if errors.Is(err, bots.ErrNotFound) {
		fail(c, http.StatusNotFound, "not_found", "Unknown widget.")
		return
	}
	if err != nil {
		h.failInternal(c, err)
		return
	}
	origin := c.GetHeader("Origin")
	if origin == "" {
		if c.Request.Method != http.MethodGet {
			fail(c, http.StatusForbidden, "origin_required", "Origin header required.")
			return
		}
	} else if origin != h.cfg.WebOrigin && !bots.OriginAllowed(pb.Bot.AllowedOrigins, origin) {
		fail(c, http.StatusForbidden, "origin_not_allowed", "This website is not allowed to use this widget.")
		return
	}
	if origin != "" {
		hd := c.Writer.Header()
		hd.Set("Access-Control-Allow-Origin", origin)
		hd.Add("Vary", "Origin")
		if c.Request.Method == http.MethodOptions {
			hd.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			hd.Set("Access-Control-Allow-Headers", "Content-Type")
			hd.Set("Access-Control-Max-Age", "600")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
	}
	c.Set(ctxPublicBot, pb)
	c.Next()
}

func publicBot(c *gin.Context) *bots.PublicBot { return c.MustGet(ctxPublicBot).(*bots.PublicBot) }

type publicConfig struct {
	PublicKey      string        `json:"public_key"`
	Name           string        `json:"name"`
	Theme          bots.Theme    `json:"theme"`
	Content        bots.Content  `json:"content"`
	Behavior       bots.Behavior `json:"behavior"`
	Features       bots.Features `json:"features"`
	ConfigVersion  int           `json:"config_version"`
	AllowedOrigins []string      `json:"allowed_origins"`
}

func (h *Handler) widgetConfig(c *gin.Context) {
	pb := publicBot(c)
	etag := fmt.Sprintf(`W/"%d-%d"`, pb.Version, pb.Bot.UpdatedAt.Unix())
	c.Header("ETag", etag)
	c.Header("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	origins := []string(pb.Bot.AllowedOrigins)
	if origins == nil {
		origins = []string{}
	}
	c.JSON(http.StatusOK, publicConfig{
		PublicKey: pb.Bot.PublicKey, Name: pb.Bot.Name, Theme: pb.Settings.Theme, Content: pb.Settings.Content,
		Behavior: pb.Settings.Behavior, Features: pb.Settings.Features, ConfigVersion: pb.Version, AllowedOrigins: origins,
	})
}

func (h *Handler) widgetChat(c *gin.Context) {
	pb := publicBot(c)
	var req chatReq
	if !bindJSON(c, &req) {
		return
	}
	if !visitorIDRe.MatchString(req.VisitorID) {
		fail(c, http.StatusBadRequest, "bad_request", "Invalid visitor id.")
		return
	}
	text := strings.TrimSpace(req.Message)
	if text == "" || utf8.RuneCountInString(text) > maxMessageChars {
		fail(c, http.StatusUnprocessableEntity, "validation_error", "Message must be 1-2000 characters.")
		return
	}
	if !h.allow(c, "chat-ip:"+c.ClientIP(), ruleChatIP) ||
		!h.allow(c, "chat-visitor:"+pb.Bot.ID.String()+":"+req.VisitorID, ruleChatVisitor) ||
		!h.allow(c, "chat-bot:"+pb.Bot.ID.String(), ruleChatBot) {
		return
	}
	bot := pb.Bot
	conv, err := h.loadConversation(c.Request.Context(), &bot, req.VisitorID, req.ConversationID, models.ChannelWidget, c.GetHeader("Origin"), req.VisitorMeta)
	if err != nil {
		h.failInternal(c, err)
		return
	}
	h.streamChat(c, &bot, conv, text)
}

type historyMessage struct {
	ID        uuid.UUID `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// widgetHistory restores a visitor's conversation after a page reload. The visitor id is
// a random secret held in the iframe's localStorage.
func (h *Handler) widgetHistory(c *gin.Context) {
	pb := publicBot(c)
	visitor := c.Query("visitor_id")
	convID, err := uuid.Parse(c.Param("cid"))
	if err != nil || !visitorIDRe.MatchString(visitor) {
		fail(c, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	var msgs []historyMessage
	err = h.store.DB().WithContext(c.Request.Context()).Raw(`
		SELECT m.id, m.role, m.content, m.created_at FROM messages m
		JOIN conversations cv ON cv.id = m.conversation_id
		WHERE cv.id = ? AND cv.bot_id = ? AND cv.visitor_id = ?
		ORDER BY m.created_at DESC LIMIT 50`, convID, pb.Bot.ID, visitor).Scan(&msgs).Error
	if err != nil {
		h.failInternal(c, err)
		return
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	if msgs == nil {
		msgs = []historyMessage{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"messages": msgs})
}

type leadReq struct {
	VisitorID      string `json:"visitor_id"`
	ConversationID string `json:"conversation_id"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	Phone          string `json:"phone"`
}

func (h *Handler) widgetLead(c *gin.Context) {
	pb := publicBot(c)
	if !pb.Settings.Features.LeadCapture.Enabled {
		fail(c, http.StatusNotFound, "not_found", "Lead capture is disabled.")
		return
	}
	var req leadReq
	if !bindJSON(c, &req) {
		return
	}
	if !visitorIDRe.MatchString(req.VisitorID) {
		fail(c, http.StatusBadRequest, "bad_request", "Invalid visitor id.")
		return
	}
	if !h.allow(c, "lead-ip:"+c.ClientIP(), ruleLeadIP) || !h.allow(c, "lead-visitor:"+pb.Bot.ID.String()+":"+req.VisitorID, ruleLeadVisitor) {
		return
	}
	req.Name, req.Email, req.Phone = strings.TrimSpace(req.Name), strings.ToLower(strings.TrimSpace(req.Email)), strings.TrimSpace(req.Phone)
	if utf8.RuneCountInString(req.Name) > 120 || len(req.Email) > 254 || len(req.Phone) > 40 {
		fail(c, http.StatusUnprocessableEntity, "validation_error", "A field is too long.")
		return
	}
	if req.Email != "" {
		if _, err := mail.ParseAddress(req.Email); err != nil {
			fail(c, http.StatusUnprocessableEntity, "validation_error", "Enter a valid email address.")
			return
		}
	}
	for _, f := range pb.Settings.Features.LeadCapture.Fields {
		if (f == "name" && req.Name == "") || (f == "email" && req.Email == "") || (f == "phone" && req.Phone == "") {
			fail(c, http.StatusUnprocessableEntity, "validation_error", "Please fill in all fields.")
			return
		}
	}
	lead := &models.Lead{BotID: pb.Bot.ID, Name: req.Name, Email: req.Email, Phone: req.Phone}
	if id, err := uuid.Parse(req.ConversationID); err == nil {
		var n int64
		h.store.DB().WithContext(c.Request.Context()).Model(&models.Conversation{}).
			Where("id = ? AND bot_id = ? AND visitor_id = ?", id, pb.Bot.ID, req.VisitorID).Count(&n)
		if n == 1 {
			lead.ConversationID = &id
		}
	}
	if err := h.store.DB().WithContext(c.Request.Context()).Create(lead).Error; err != nil {
		h.failInternal(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"status": "ok"})
}
