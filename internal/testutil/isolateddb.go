package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/botplaysbot/internal/db"
)

// IsolatedDB provisions a throwaway database for t, migrates it to the
// current schema, and returns a pool connected to it. The database is dropped
// (WITH FORCE) at cleanup, so every test starts from a clean, freshly
// migrated schema and `go test ./...`'s parallel packages cannot corrupt each
// other's state. The shared database named by DATABASE_URL is never modified.
//
// Skips the test when DATABASE_URL is unset or the server is unreachable
// (same convention as PostgresURL).
func IsolatedDB(t testingT) *pgxpool.Pool {
	t.Helper()

	testURL := isolatedDBURL(t)

	pool, err := db.OpenPool(context.Background(), testURL)
	if err != nil {
		t.Fatalf("testutil: open pool for isolated database: %v", err)
	}
	if err := db.Ping(context.Background(), pool); err != nil {
		pool.Close()
		t.Skipf("integration test: isolated database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// IsolatedDBURL is IsolatedDB for callers that need the connection URL (for
// example to boot a config.Config); it provisions, migrates, and registers
// the drop cleanup identically.
func IsolatedDBURL(t testingT) string {
	t.Helper()
	return isolatedDBURL(t)
}

// isolatedDBURL creates the database, migrates it, and registers cleanup.
func isolatedDBURL(t testingT) string {
	t.Helper()

	baseURL := PostgresURL(t) // skips when DATABASE_URL is unset or unreachable

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	name := "playsbot_test_" + randomSuffix()
	adminURL, err := urlForDatabase(baseURL, "postgres")
	if err != nil {
		t.Fatalf("testutil: rewrite DATABASE_URL for admin connection: %v", err)
	}
	testURL, err := urlForDatabase(baseURL, name)
	if err != nil {
		t.Fatalf("testutil: rewrite DATABASE_URL for isolated database: %v", err)
	}

	// CREATE/DROP DATABASE must run outside any transaction and not against
	// the database being dropped, so they go through the server's `postgres`
	// database.
	admin, err := db.OpenPool(ctx, adminURL)
	if err != nil {
		t.Fatalf("testutil: connect to the server's postgres database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		admin.Close()
		t.Fatalf("testutil: create test database %s: %v", name, err)
	}
	admin.Close()

	if err := db.Migrate(ctx, testURL); err != nil {
		dropTestDatabase(t, adminURL, name)
		t.Fatalf("testutil: migrate test database %s: %v", name, err)
	}

	t.Cleanup(func() { dropTestDatabase(t, adminURL, name) })
	return testURL
}

// dropTestDatabase drops the named database, force-terminating any remaining
// backends. Failures are reported (they would leave databases behind) but do
// not abort the remaining cleanup chain.
func dropTestDatabase(t testingT, adminURL, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := db.OpenPool(ctx, adminURL)
	if err != nil {
		t.Errorf("testutil: drop test database %s: connect: %v", name, err)
		return
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		t.Errorf("testutil: drop test database %s: %v", name, err)
	}
}

// urlForDatabase returns databaseURL rewritten to target dbname.
func urlForDatabase(databaseURL, dbname string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("DATABASE_URL must be a postgres:// URL, got scheme %q", u.Scheme)
	}
	u.Path = "/" + dbname
	return u.String(), nil
}

// randomSuffix returns 12 lowercase-alphanumeric characters from crypto/rand
// for test database names.
func randomSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("testutil: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
