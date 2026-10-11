package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/ragclient"
)

const maxMessageChars = 2000

var visitorIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type chatReq struct {
	VisitorID      string          `json:"visitor_id"`
	ConversationID string          `json:"conversation_id"`
	Message        string          `json:"message"`
	VisitorMeta    json.RawMessage `json:"visitor_meta"`
}

// sseWriter writes server-sent events and flushes after each one.
type sseWriter struct {
	c       *gin.Context
	started bool
}

func (w *sseWriter) start() {
	if w.started {
		return
	}
	h := w.c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.c.Status(http.StatusOK)
	w.started = true
}

func (w *sseWriter) send(event string, data any) error {
	w.start()
	var payload []byte
	switch v := data.(type) {
	case json.RawMessage:
		payload = v
	default:
		var err error
		if payload, err = json.Marshal(v); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w.c.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	w.c.Writer.Flush()
	return nil
}

// loadConversation returns the conversation if it belongs to this bot and visitor, or
// creates a new one. A foreign or unknown ID silently starts a fresh conversation.
func (h *Handler) loadConversation(ctx context.Context, bot *models.Bot, visitorID, convID string, ch models.Channel, origin string, meta json.RawMessage) (*models.Conversation, error) {
	db := h.store.DB().WithContext(ctx)
	if id, err := uuid.Parse(convID); err == nil {
		var conv models.Conversation
		err := db.Where("id = ? AND bot_id = ? AND visitor_id = ?", id, bot.ID, visitorID).Take(&conv).Error
		if err == nil {
			return &conv, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	conv := &models.Conversation{BotID: bot.ID, VisitorID: visitorID, Channel: ch, Origin: origin, VisitorMeta: sanitizeMeta(meta)}
	return conv, db.Create(conv).Error
}

// sanitizeMeta keeps only small string fields from identify() data.
func sanitizeMeta(raw json.RawMessage) models.JSONB {
	if len(raw) == 0 || len(raw) > 4096 {
		return models.JSONB("{}")
	}
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return models.JSONB("{}")
	}
	out := map[string]string{}
	for _, k := range []string{"id", "name", "email", "plan", "company"} {
		if s, ok := in[k].(string); ok && s != "" {
			if utf8.RuneCountInString(s) > 200 {
				s = string([]rune(s)[:200])
			}
			out[k] = s
		}
	}
	b, _ := json.Marshal(out)
	return models.JSONB(b)
}

func (h *Handler) history(ctx context.Context, convID uuid.UUID) ([]ragclient.HistoryTurn, error) {
	var msgs []models.Message
	err := h.store.DB().WithContext(ctx).Select("role", "content").
		Where("conversation_id = ?", convID).Order("created_at DESC").Limit(h.cfg.HistoryTurns * 2).Find(&msgs).Error
	if err != nil {
		return nil, err
	}
	slices.Reverse(msgs)
	out := make([]ragclient.HistoryTurn, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, ragclient.HistoryTurn{Role: m.Role, Content: m.Content})
	}
	return out, nil
}

// streamChat is the chat gateway shared by the widget and the dashboard test panel. It
// persists the user turn, relays rag SSE events to the client, then records the
// assistant turn with token usage. If the client disconnects, the upstream request is
// cancelled, so no tokens are spent on an answer nobody will read.
func (h *Handler) streamChat(c *gin.Context, bot *models.Bot, conv *models.Conversation, text string) {
	ctx := c.Request.Context()
	hist, err := h.history(ctx, conv.ID)
	if err != nil {
		h.failInternal(c, err)
		return
	}
	db := h.store.DB().WithContext(ctx)
	userMsg := &models.Message{ConversationID: conv.ID, BotID: bot.ID, VisitorID: conv.VisitorID, Role: "user", Content: text}
	if err := db.Create(userMsg).Error; err != nil {
		h.failInternal(c, err)
		return
	}
	// knowledge_version is read fresh (the public bot cache may be up to 30s old) so the
	// answer cache never serves answers built from deleted documents.
	var kv int64
	if err := db.Model(&models.Bot{}).Select("knowledge_version").Where("id = ?", bot.ID).Scan(&kv).Error; err != nil {
		h.failInternal(c, err)
		return
	}

	w := &sseWriter{c: c}
	_ = w.send("meta", gin.H{"conversation_id": conv.ID, "message_id": userMsg.ID})

	start := time.Now()
	var answer strings.Builder
	var usage ragclient.Usage
	streamErr := h.rag.ChatStream(ctx, ragclient.ChatRequest{
		BotID: bot.ID.String(), KnowledgeVersion: kv, SystemInstructions: bot.SystemInstructions,
		FallbackMessage: bot.FallbackMessage, Model: bot.Model, Temperature: bot.Temperature,
		History: hist, Message: text,
	}, func(ev ragclient.Event) error {
		switch ev.Name {
		case "token":
			var t struct {
				T string `json:"t"`
			}
			if json.Unmarshal(ev.Data, &t) == nil {
				answer.WriteString(t.T)
			}
			return w.send("token", ev.Data)
		case "sources":
			return w.send("sources", ev.Data)
		case "done":
			_ = json.Unmarshal(ev.Data, &usage)
			return nil
		case "error":
			h.log.Warn("rag stream error event", "data", string(ev.Data), "bot_id", bot.ID)
			return w.send("error", gin.H{"message": "The assistant is unavailable right now. Please try again."})
		}
		return nil
	})
	latency := int(time.Since(start).Milliseconds())

	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		h.log.Error("chat stream failed", "error", streamErr, "bot_id", bot.ID)
		if answer.Len() == 0 {
			_ = w.send("error", gin.H{"message": "The assistant is unavailable right now. Please try again."})
		}
	}

	// Persist even if the client went away: use a context detached from the request.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	sdb := h.store.DB().WithContext(saveCtx)
	var assistantID uuid.UUID
	if answer.Len() > 0 {
		model := usage.Model
		if model == "" {
			model = bot.Model
		}
		am := &models.Message{
			ConversationID: conv.ID, BotID: bot.ID, VisitorID: conv.VisitorID, Role: "assistant", Content: answer.String(),
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CachedInputTokens: usage.CachedInputTokens,
			Model: model, LatencyMS: latency,
		}
		if err := sdb.Create(am).Error; err != nil {
			h.log.Error("save assistant message", "error", err)
		}
		assistantID = am.ID
		h.log.Info("chat completed", "bot_id", bot.ID, "conversation_id", conv.ID, "model", model,
			"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
			"cached_input_tokens", usage.CachedInputTokens, "answer_cache", usage.Cached, "latency_ms", latency)
	}
	_ = sdb.Model(conv).Update("last_message_at", time.Now().UTC()).Error
	if ctx.Err() == nil {
		_ = w.send("done", gin.H{"message_id": assistantID, "cached": usage.Cached})
	}
}

// testChat lets the bot owner try the bot from the dashboard. It bypasses origin checks
// but is authenticated and scoped to the owner.
func (h *Handler) testChat(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req chatReq
	if !bindJSON(c, &req) {
		return
	}
	text := strings.TrimSpace(req.Message)
	if text == "" || utf8.RuneCountInString(text) > maxMessageChars {
		fail(c, http.StatusUnprocessableEntity, "validation_error", "Message must be 1-2000 characters.")
		return
	}
	bot, err := h.store.GetBot(c.Request.Context(), userID(c), id)
	if err != nil {
		h.failStore(c, err)
		return
	}
	visitor := "dash_" + strings.ReplaceAll(userID(c).String(), "-", "")
	conv, err := h.loadConversation(c.Request.Context(), bot, visitor, req.ConversationID, models.ChannelTest, "dashboard", nil)
	if err != nil {
		h.failInternal(c, err)
		return
	}
	h.streamChat(c, bot, conv, text)
}
