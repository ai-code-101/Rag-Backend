// Package testutil provides a throwaway, fully migrated Postgres database for tests.
//
// Set TEST_DATABASE_URL to a server where the user may CREATE DATABASE and the pgvector
// extension is installed; tests that need a database are skipped otherwise.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/ai-code-101/rag-backend/api/internal/database"
	"github.com/ai-code-101/rag-backend/api/internal/migrate"
)

const EmbeddingDim = 8

func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// NewDB creates a uniquely named database, migrates it and drops it when the test ends.
// It returns the GORM handle and the DSN of the new database.
func NewDB(t testing.TB) (*gorm.DB, string) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "hv_test_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	admin.Close(ctx)

	u, _ := url.Parse(base)
	u.Path = "/" + name
	dsn := u.String()
	if err := migrate.Run(ctx, dsn, EmbeddingDim, Logger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, dsn, database.Options{MaxOpenConns: 10, MaxIdleConns: 2, ConnMaxLife: time.Minute}, Logger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			return
		}
		defer admin.Close(ctx)
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db, dsn
}
