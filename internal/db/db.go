// Package db provides the Postgres layer for the plays.bot AppView: a lazily
// connecting pgx/v5 pool and goose-driven embedded migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register "pgx" database/sql driver
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/haileyok/botplaysbot/migrations"
)

// OpenPool creates a pgx connection pool for databaseURL.
//
// The pool is lazy: no connection is dialed at construction (or at package
// import); the first Acquire establishes sockets. Callers should Ping the pool
// (or run Migrate) when they need to fail fast on an unreachable database.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	return pool, nil
}

// Migrate applies pending embedded migrations to the database at databaseURL.
//
// It uses goose with the migrations embedded at build time, so deployed
// binaries carry their schema with them.
func Migrate(ctx context.Context, databaseURL string) error {
	goose.SetBaseFS(migrations.Files)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("db: open for migration: %w", err)
	}
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}

// Ping verifies the database is reachable, with a timeout.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
