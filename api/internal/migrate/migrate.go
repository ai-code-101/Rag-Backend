// Package migrate applies the embedded, versioned SQL migrations.
//
// Migrations run inside a session-level advisory lock so that many API instances
// starting at once (e.g. a Cloud Run scale-out) apply each migration exactly once.
// Every migration runs in its own transaction together with its bookkeeping row.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed sql/*.sql
var files embed.FS

// advisoryLockKey is an arbitrary constant shared by every instance of this service.
const advisoryLockKey int64 = 0x686f72697a6f6e76 // "horizonv"

type Migration struct {
	Version string
	SQL     string
}

// Load returns the embedded migrations in order with template variables substituted.
func Load(embeddingDim int) ([]Migration, error) {
	entries, err := fs.ReadDir(files, "sql")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := files.ReadFile("sql/" + e.Name())
		if err != nil {
			return nil, err
		}
		sql := strings.ReplaceAll(string(b), "{{EMBEDDING_DIM}}", strconv.Itoa(embeddingDim))
		out = append(out, Migration{Version: strings.TrimSuffix(e.Name(), ".sql"), SQL: sql})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Run applies all pending migrations. It is safe to call concurrently from many processes.
func Run(ctx context.Context, dsn string, embeddingDim int, log *slog.Logger) error {
	migrations, err := Load(embeddingDim)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate connect: %w", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		log.Info("applying migration", "version", m.Version)
		// The simple protocol accepts multi-statement scripts; wrapping in BEGIN/COMMIT makes
		// the schema change and its bookkeeping row atomic.
		script := "BEGIN;\n" + m.SQL + "\nINSERT INTO schema_migrations (version) VALUES ('" + m.Version + "');\nCOMMIT;"
		if _, err := conn.PgConn().Exec(ctx, script).ReadAll(); err != nil {
			_, _ = conn.Exec(context.Background(), "ROLLBACK")
			return fmt.Errorf("migration %s: %w", m.Version, err)
		}
	}
	return nil
}
