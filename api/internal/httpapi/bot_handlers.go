package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/models"
)

func parseID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		fail(c, http.StatusNotFound, "not_found", "Not found.")
		return uuid.Nil, false
	}
	return id, true
}

type botView struct {
	models.Bot
	EmbedSnippet string `json:"embed_snippet"`
}

func (h *Handler) viewBot(b *models.Bot) botView {
	return botView{Bot: *b, EmbedSnippet: `<script src="` + h.cfg.WebOrigin + `/widget.js" data-bot="` + b.PublicKey + `" async></script>`}
}

func (h *Handler) listModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"models": h.cfg.AllowedModels, "default": h.cfg.DefaultModel})
}

func (h *Handler) listBots(c *gin.Context) {
	list, err := h.store.ListBots(c.Request.Context(), userID(c))
	if err != nil {
		h.failInternal(c, err)
		return
	}
	out := make([]botView, 0, len(list))
	for i := range list {
		out = append(out, h.viewBot(&list[i]))
	}
	c.JSON(http.StatusOK, gin.H{"bots": out})
}

func (h *Handler) createBot(c *gin.Context) {
	var in bots.BotInput
	if !bindJSON(c, &in) {
		return
	}
	if err := in.Validate(h.cfg.AllowedModels); err != nil {
		failValidation(c, err)
		return
	}
	b, err := h.store.CreateBot(c.Request.Context(), userID(c), in, h.cfg.DefaultModel)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.viewBot(b))
}

func (h *Handler) getBot(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	b, err := h.store.GetBot(c.Request.Context(), userID(c), id)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, h.viewBot(b))
}

func (h *Handler) updateBot(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var in bots.BotInput
	if !bindJSON(c, &in) {
		return
	}
	if err := in.Validate(h.cfg.AllowedModels); err != nil {
		failValidation(c, err)
		return
	}
	b, err := h.store.UpdateBot(c.Request.Context(), userID(c), id, in)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, h.viewBot(b))
}

func (h *Handler) deleteBot(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	keys, err := h.store.DeleteBot(c.Request.Context(), userID(c), id)
	if err != nil {
		h.failStore(c, err)
		return
	}
	h.deleteObjects(keys)
	c.Status(http.StatusNoContent)
}

func (h *Handler) rotateKey(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	b, err := h.store.RotatePublicKey(c.Request.Context(), userID(c), id)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, h.viewBot(b))
}

// deleteObjects removes stored source files in the background; orphans are harmless and
// can also be cleaned with a bucket lifecycle rule.
func (h *Handler) deleteObjects(keys []string) {
	if len(keys) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, k := range keys {
			if err := h.objects.Delete(ctx, k); err != nil {
				h.log.Warn("delete object", "key", k, "error", err)
			}
		}
	}()
}

type widgetView struct {
	bots.WidgetSettings
	ConfigVersion int       `json:"config_version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (h *Handler) getWidget(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	b, wc, err := h.store.GetWidgetConfig(c.Request.Context(), userID(c), id)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, widgetView{WidgetSettings: bots.SettingsFrom(wc, b.Name), ConfigVersion: wc.ConfigVersion, UpdatedAt: wc.UpdatedAt})
}

type widgetUpdate struct {
	bots.WidgetSettings
	ConfigVersion int `json:"config_version"`
}

func (h *Handler) updateWidget(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	var in widgetUpdate
	if !bindJSON(c, &in) {
		return
	}
	if err := in.WidgetSettings.Validate(!h.cfg.IsProduction()); err != nil {
		failValidation(c, err)
		return
	}
	wc, err := h.store.UpdateWidgetConfig(c.Request.Context(), userID(c), id, in.WidgetSettings, in.ConfigVersion)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, widgetView{WidgetSettings: in.WidgetSettings, ConfigVersion: wc.ConfigVersion, UpdatedAt: wc.UpdatedAt})
}

type usageRow struct {
	Day          time.Time `json:"day"`
	Messages     int       `json:"messages"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CachedTokens int       `json:"cached_input_tokens"`
}

// usage returns per-day assistant message counts and token usage for the last 30 days.
func (h *Handler) usage(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.store.GetBot(c.Request.Context(), userID(c), id); err != nil {
		h.failStore(c, err)
		return
	}
	var rows []usageRow
	err := h.store.DB().WithContext(c.Request.Context()).Raw(`
		SELECT date_trunc('day', created_at) AS day, count(*) AS messages,
		       coalesce(sum(input_tokens),0) AS input_tokens, coalesce(sum(output_tokens),0) AS output_tokens,
		       coalesce(sum(cached_input_tokens),0) AS cached_tokens
		FROM messages WHERE bot_id = ? AND role = 'assistant' AND created_at > now() - interval '30 days'
		GROUP BY 1 ORDER BY 1`, id).Scan(&rows).Error
	if err != nil {
		h.failInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": rows})
}

func (h *Handler) listLeads(c *gin.Context) {
	id, ok := parseID(c, "id")
	if !ok {
		return
	}
	if _, err := h.store.GetBot(c.Request.Context(), userID(c), id); err != nil {
		h.failStore(c, err)
		return
	}
	var leads []models.Lead
	if err := h.store.DB().WithContext(c.Request.Context()).Where("bot_id = ?", id).
		Order("created_at DESC").Limit(500).Find(&leads).Error; err != nil {
		h.failInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"leads": leads})
}
