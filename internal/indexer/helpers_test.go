package indexer

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// shared test helpers for the indexer package tests.

func testSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func testCfg() *config.Config {
	return &config.Config{
		ServiceDID: "did:plc:service-test",
		Tunables: config.Tunables{
			MissingRecordWindow: 10 * time.Minute,
			ChallengeTTL:        10 * time.Minute,
		},
	}
}

func testPubKey() ed25519.PublicKey {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub
}

func repoNew(pool *pgxpool.Pool) *repo.Pool { return repo.New(pool) }
