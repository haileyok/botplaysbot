package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBCursorStore persists a stream cursor in the cursors table (migration
// 0004 — the minimal documented extension to the Appendix A sketch). One
// row per source name; the indexer resumes from the stored sequence on
// boot, so restarts neither skip events (stale cursor is rejected upstream)
// nor replay them (no cursor).
type DBCursorStore struct {
	pool *pgxpool.Pool
	name string
}

// NewDBCursorStore returns a CursorStore backed by row name (e.g.
// "firehose").
func NewDBCursorStore(pool *pgxpool.Pool, name string) *DBCursorStore {
	return &DBCursorStore{pool: pool, name: name}
}

// LoadCursor reads the stored sequence; 0 when none exists.
func (s *DBCursorStore) LoadCursor(ctx context.Context) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT seq FROM cursors WHERE name=$1`, s.name).Scan(&seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("indexer: load cursor %s: %w", s.name, err)
	}
	return seq, nil
}

// SaveCursor upserts the sequence.
func (s *DBCursorStore) SaveCursor(ctx context.Context, cursor int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO cursors (name, seq, updated_at) VALUES ($1, $2, now())
		 ON CONFLICT (name) DO UPDATE SET seq=EXCLUDED.seq, updated_at=now()`,
		s.name, cursor)
	if err != nil {
		return fmt.Errorf("indexer: save cursor %s: %w", s.name, err)
	}
	return nil
}
