package db

import (
	"context"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/haileyok/botplaysbot/migrations"
)

// tablesFromAppendixA lists the 11 tables the schema must create per
// docs/spec-v0.1.md Appendix A. There is intentionally no `ratings` table:
// ratings are AppView-signed ATProto records (bot.plays.bot.rating).
var tablesFromAppendixA = []string{
	"actors",
	"games",
	"moves",
	"commentary",
	"escrow_keys",
	"challenges",
	"seeks",
	"pairing_history",
	"seat_history",
	"flags",
	"commentary_reads",
}

func TestMigrationsAreEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	if !slices.Contains(names, "0001_init.sql") {
		t.Fatalf("embedded migrations = %v, want 0001_init.sql present", names)
	}
}

func TestSchemaCoversAppendixA(t *testing.T) {
	sqlBytes, err := fs.ReadFile(migrations.Files, "0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001_init.sql: %v", err)
	}
	sql := string(sqlBytes)

	up := sql
	if i := strings.Index(sql, "-- +goose Down"); i >= 0 {
		up = sql[:i]
	}
	down := ""
	if i := strings.Index(sql, "-- +goose Down"); i >= 0 {
		down = sql[i:]
	}

	createRe := regexp.MustCompile(`(?mi)^\s*CREATE TABLE (\w+)`)
	created := map[string]bool{}
	for _, m := range createRe.FindAllStringSubmatch(up, -1) {
		created[m[1]] = true
	}

	for _, table := range tablesFromAppendixA {
		if !created[table] {
			t.Errorf("schema is missing table %q", table)
		}
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+table) {
			t.Errorf("down migration does not drop %q", table)
		}
	}
	if len(created) != len(tablesFromAppendixA) {
		t.Errorf("schema creates %d tables, want %d: %v", len(created), len(tablesFromAppendixA), created)
	}
	if created["ratings"] {
		t.Errorf("ratings must not be a table: ratings are ATProto records")
	}

	// Composite primary key on moves per Appendix A: pk(game_uri, ply).
	if !strings.Contains(up, "PRIMARY KEY (game_uri, ply)") {
		t.Error("moves must have composite primary key (game_uri, ply)")
	}
}

func TestOpenPoolIsLazy(t *testing.T) {
	// A well-formed URL must produce a pool without dialing: there is no
	// Postgres in this environment.
	url := "postgres://playsbot:playsbot@127.0.0.1:54320/playsbot?sslmode=disable"
	pool, err := OpenPool(context.Background(), url)
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	pool.Close() // must not block on a dead database
}

func TestOpenPoolRejectsMalformedURL(t *testing.T) {
	if _, err := OpenPool(context.Background(), "not a url\x00"); err == nil {
		t.Fatal("expected error for malformed database URL")
	}
}
