// Package httpapi wires the Gin router, middleware and handlers.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/config"
	"github.com/ai-code-101/rag-backend/api/internal/ragclient"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
	"github.com/ai-code-101/rag-backend/api/internal/storage"
)

type Handler struct {
	cfg         *config.Config
	db          *gorm.DB
	auth        *auth.Service
	store       *bots.Store
	objects     storage.ObjectStore
	rag         *ragclient.Client
	limiter     ratelimit.Limiter
	log         *slog.Logger
	corsAllowed map[string]bool
}

type Deps struct {
	Config  *config.Config
	DB      *gorm.DB
	Auth    *auth.Service
	Store   *bots.Store
	Objects storage.ObjectStore
	RAG     *ragclient.Client
	Limiter ratelimit.Limiter
	Log     *slog.Logger
}

const jsonBodyLimit = 1 << 20

func NewRouter(d Deps) (*gin.Engine, error) {
	if d.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}
	h := &Handler{
		cfg: d.Config, db: d.DB, auth: d.Auth, store: d.Store, objects: d.Objects, rag: d.RAG,
		limiter: d.Limiter, log: d.Log, corsAllowed: map[string]bool{},
	}
	for _, o := range d.Config.CORSOrigins {
		h.corsAllowed[o] = true
	}

	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RemoteIPHeaders = d.Config.ClientIPHeaders
	if err := r.SetTrustedProxies(d.Config.TrustedProxies); err != nil {
		return nil, err
	}
	r.Use(requestID(), recovery(d.Log), accessLog(d.Log), securityHeaders(),
		dashboardCORS(append([]string{d.Config.WebOrigin}, d.Config.CORSOrigins...)))

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", h.ready)

	v1 := r.Group("/v1")

	// Dashboard API (owner-authenticated).
	dash := v1.Group("", noStore())

	a := dash.Group("/auth", maxBody(16<<10), h.limit("auth-ip", ruleAuthIP, byIP))
	a.POST("/signup", h.signup)
	a.POST("/login", h.login)
	a.POST("/refresh", h.limit("refresh-ip", ruleRefreshIP, byIP), h.csrfGuard, h.refresh)
	a.POST("/logout", h.csrfGuard, h.logout)

	authed := dash.Group("", requireAuth(d.Auth.Tokens()))
	authed.GET("/me", h.me)
	authed.GET("/models", h.listModels)

	b := authed.Group("/bots")
	json := maxBody(jsonBodyLimit)
	b.GET("", h.listBots)
	b.POST("", json, h.createBot)
	b.GET("/:id", h.getBot)
	b.PATCH("/:id", json, h.updateBot)
	b.DELETE("/:id", h.deleteBot)
	b.POST("/:id/rotate-key", h.rotateKey)
	b.GET("/:id/widget", h.getWidget)
	b.PUT("/:id/widget", json, h.updateWidget)
	b.GET("/:id/usage", h.usage)
	b.GET("/:id/leads", h.listLeads)
	b.GET("/:id/documents", h.listDocuments)
	b.POST("/:id/documents", h.uploadDocument) // size limit applied inside the handler
	b.POST("/:id/documents/url", json, h.addURL)
	b.POST("/:id/documents/text", maxBody(d.Config.MaxTextBytes+16<<10), h.addText)
	b.DELETE("/:id/documents/:docId", h.deleteDocument)
	b.POST("/:id/documents/:docId/reingest", h.reingestDocument)
	b.POST("/:id/chat", json, h.testChat)

	// Public widget API: public key + origin check + per-IP/visitor rate limits.
	w := v1.Group("/widget/:key", maxBody(16<<10), h.widgetBot)
	w.OPTIONS("/*any", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w.GET("/config", h.limit("widget-config-ip", ruleWidgetConfigIP, byIP), h.widgetConfig)
	w.POST("/chat", h.widgetChat)
	w.POST("/leads", h.widgetLead)
	w.GET("/conversations/:cid", h.limit("history-ip", ruleHistoryIP, byIP), h.widgetHistory)

	r.NoRoute(func(c *gin.Context) { fail(c, http.StatusNotFound, "not_found", "Not found.") })
	return r, nil
}

// ready reports whether dependencies are reachable (used as the readiness probe).
func (h *Handler) ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	sqlDB, err := h.db.DB()
	if err == nil {
		err = sqlDB.PingContext(ctx)
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "database": "down"})
		return
	}
	rag := "ok"
	if err := h.rag.Health(ctx); err != nil {
		rag = "down" // degraded: dashboard still works, chat and ingestion do not
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok", "rag": rag})
}
