// Package database opens the pooled GORM connection.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Options struct {
	MaxOpenConns int
	MaxIdleConns int
	ConnMaxLife  time.Duration
	Debug        bool
}

func Open(ctx context.Context, dsn string, opts Options, log *slog.Logger) (*gorm.DB, error) {
	lvl := logger.Warn
	if opts.Debug {
		lvl = logger.Info
	}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger:                 logger.New(slogWriter{log}, logger.Config{SlowThreshold: 300 * time.Millisecond, LogLevel: lvl, IgnoreRecordNotFoundError: true}),
		SkipDefaultTransaction: true,
		PrepareStmt:            true,
		NowFunc:                func() time.Time { return time.Now().UTC() },
		TranslateError:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(opts.ConnMaxLife)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

type slogWriter struct{ l *slog.Logger }

func (w slogWriter) Printf(format string, args ...any) {
	w.l.Warn(fmt.Sprintf(format, args...), "component", "gorm")
}
