// Command api runs the Horizon Valley API server.
//
//	api            start the HTTP server (runs migrations first if MIGRATE_ON_START=true)
//	api serve      same as above
//	api migrate    apply database migrations and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/config"
	"github.com/ai-code-101/rag-backend/api/internal/database"
	"github.com/ai-code-101/rag-backend/api/internal/httpapi"
	"github.com/ai-code-101/rag-backend/api/internal/logger"
	"github.com/ai-code-101/rag-backend/api/internal/migrate"
	"github.com/ai-code-101/rag-backend/api/internal/ragclient"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
	"github.com/ai-code-101/rag-backend/api/internal/storage"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:\n"+err.Error())
		os.Exit(2)
	}
	log := logger.New(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "migrate":
		if err := migrate.Run(ctx, cfg.DatabaseURL, cfg.EmbeddingDim, log); err != nil {
			log.Error("migration failed", "error", err)
			os.Exit(1)
		}
		log.Info("migrations complete")
	case "serve":
		if err := serve(ctx, cfg, log); err != nil {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (use serve or migrate)\n", cmd)
		os.Exit(2)
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.MigrateOnStart {
		if err := migrate.Run(ctx, cfg.DatabaseURL, cfg.EmbeddingDim, log); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	db, err := database.Open(ctx, cfg.DatabaseURL, database.Options{
		MaxOpenConns: cfg.DBMaxOpenConns, MaxIdleConns: cfg.DBMaxIdleConns, ConnMaxLife: cfg.DBConnMaxLife,
		Debug: cfg.LogLevel == "debug",
	}, log)
	if err != nil {
		return err
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	var limiter ratelimit.Limiter = ratelimit.NewMemory()
	if cfg.RedisURL != "" {
		rl, err := ratelimit.NewRedis(cfg.RedisURL)
		if err != nil {
			return err
		}
		defer rl.Close()
		limiter = rl
	}

	var objects storage.ObjectStore
	switch cfg.StorageDriver {
	case "gcs":
		g, err := storage.NewGCS(ctx, cfg.GCSBucket)
		if err != nil {
			return err
		}
		defer g.Close()
		objects = g
	default:
		objects = &storage.Local{Root: cfg.LocalStorageDir}
	}

	authSvc, err := auth.NewService(auth.Options{
		DB: db, Hasher: auth.NewHasher(auth.DefaultArgon2, 4),
		Tokens:     auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTIssuer, cfg.AccessTokenTTL),
		RefreshTTL: cfg.RefreshTokenTTL, Log: log,
	})
	if err != nil {
		return err
	}

	rag := ragclient.New(cfg.RAGURL, cfg.RAGToken, cfg.RAGTimeout)
	if cfg.RAGIDTokenAud != "" {
		if err := rag.WithIDToken(ctx, cfg.RAGIDTokenAud); err != nil {
			return err
		}
	}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Config: cfg, DB: db, Auth: authSvc, Store: bots.NewStore(db, cfg.MaxBotsPerUser, cfg.MaxDocsPerBot),
		Objects: objects, RAG: rag, Limiter: limiter, Log: log,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // large uploads
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// No WriteTimeout: chat responses are long-lived SSE streams bounded by RAG_TIMEOUT.
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "port", cfg.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	return nil
}
