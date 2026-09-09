// Package repo provides plain pgx CRUD repositories for the AppView tables.
// No business logic lives here: callers own validation, sequencing, and
// policy. Nullables are pointers, jsonb columns are json.RawMessage, bytea
// columns are []byte.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("repo: not found")

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Pool wraps the pgx pool shared by all repositories.
type Pool struct {
	*pgxpool.Pool
}

// New wraps a pgx pool.
func New(pool *pgxpool.Pool) *Pool { return &Pool{pool} }

// Game mirrors the games table (spec App. A). URI is the AT URI of the
// bot.plays.bot.game record; state is the authoritative game state.
type Game struct {
	URI             string
	CID             *string
	GameType        string
	Variant         *string
	Status          string
	Players         json.RawMessage
	TimeControl     json.RawMessage
	CommentaryDelay json.RawMessage
	Seed            []byte
	State           json.RawMessage
	Ply             int
	TurnDID         *string
	Result          json.RawMessage
	CreatedAt       *time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

const gameColumns = `uri, cid, game_type, variant, status, players, time_control,
	commentary_delay, seed, state, ply, turn_did, result, created_at, started_at, finished_at`

func scanGame(row pgx.Row) (*Game, error) {
	var g Game
	err := row.Scan(&g.URI, &g.CID, &g.GameType, &g.Variant, &g.Status, &g.Players,
		&g.TimeControl, &g.CommentaryDelay, &g.Seed, &g.State, &g.Ply, &g.TurnDID,
		&g.Result, &g.CreatedAt, &g.StartedAt, &g.FinishedAt)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &g, nil
}

// GamesRepo is CRUD for games.
type GamesRepo struct{ pool *pgxpool.Pool }

// NewGamesRepo returns a games repository.
func NewGamesRepo(pool *pgxpool.Pool) *GamesRepo { return &GamesRepo{pool} }

// Insert stores a new game.
func (r *GamesRepo) Insert(ctx context.Context, g *Game) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO games (`+gameColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		g.URI, g.CID, g.GameType, g.Variant, g.Status, g.Players, g.TimeControl,
		g.CommentaryDelay, g.Seed, g.State, g.Ply, g.TurnDID, g.Result,
		g.CreatedAt, g.StartedAt, g.FinishedAt)
	return err
}

// Get returns the game at uri.
func (r *GamesRepo) Get(ctx context.Context, uri string) (*Game, error) {
	return scanGame(r.pool.QueryRow(ctx,
		`SELECT `+gameColumns+` FROM games WHERE uri = $1`, uri))
}

// Update overwrites the mutable columns of the game at uri.
func (r *GamesRepo) Update(ctx context.Context, g *Game) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE games SET cid=$2, status=$3, players=$4, time_control=$5,
		   commentary_delay=$6, seed=$7, state=$8, ply=$9, turn_did=$10,
		   result=$11, started_at=$12, finished_at=$13
		 WHERE uri=$1`,
		g.URI, g.CID, g.Status, g.Players, g.TimeControl, g.CommentaryDelay,
		g.Seed, g.State, g.Ply, g.TurnDID, g.Result, g.StartedAt, g.FinishedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Move mirrors the moves table: one accepted move per (game, ply).
type Move struct {
	GameURI          string
	Ply              int
	PlayerDID        *string
	Payload          json.RawMessage
	Notation         *string
	ReceivedAt       time.Time
	ClockRemainingMs *int64
	Token            *string
	RepoURI          *string
	RepoCID          *string
	Verified         bool
}

// MovesRepo is CRUD for moves.
type MovesRepo struct{ pool *pgxpool.Pool }

// NewMovesRepo returns a moves repository.
func NewMovesRepo(pool *pgxpool.Pool) *MovesRepo { return &MovesRepo{pool} }

// Insert stores an accepted move.
func (r *MovesRepo) Insert(ctx context.Context, m *Move) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO moves (game_uri, ply, player_did, payload, notation, received_at,
		    clock_remaining_ms, token, repo_uri, repo_cid, verified)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		m.GameURI, m.Ply, m.PlayerDID, m.Payload, m.Notation, m.ReceivedAt,
		m.ClockRemainingMs, m.Token, m.RepoURI, m.RepoCID, m.Verified)
	return err
}

// Get returns the move at (gameURI, ply).
func (r *MovesRepo) Get(ctx context.Context, gameURI string, ply int) (*Move, error) {
	var m Move
	err := r.pool.QueryRow(ctx,
		`SELECT game_uri, ply, player_did, payload, notation, received_at,
		        clock_remaining_ms, token, repo_uri, repo_cid, verified
		 FROM moves WHERE game_uri=$1 AND ply=$2`, gameURI, ply).
		Scan(&m.GameURI, &m.Ply, &m.PlayerDID, &m.Payload, &m.Notation, &m.ReceivedAt,
			&m.ClockRemainingMs, &m.Token, &m.RepoURI, &m.RepoCID, &m.Verified)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &m, nil
}

// LinkRepoRecord attaches the indexed repo record to an accepted move.
func (r *MovesRepo) LinkRepoRecord(ctx context.Context, gameURI string, ply int, repoURI, repoCID string, verified bool) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE moves SET repo_uri=$3, repo_cid=$4, verified=$5
		 WHERE game_uri=$1 AND ply=$2`,
		gameURI, ply, repoURI, repoCID, verified)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByGame returns a game's moves ordered by ply.
func (r *MovesRepo) ListByGame(ctx context.Context, gameURI string) ([]Move, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT game_uri, ply, player_did, payload, notation, received_at,
		        clock_remaining_ms, token, repo_uri, repo_cid, verified
		 FROM moves WHERE game_uri=$1 ORDER BY ply`, gameURI)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Move
	for rows.Next() {
		var m Move
		if err := rows.Scan(&m.GameURI, &m.Ply, &m.PlayerDID, &m.Payload, &m.Notation,
			&m.ReceivedAt, &m.ClockRemainingMs, &m.Token, &m.RepoURI, &m.RepoCID, &m.Verified); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Challenge mirrors the challenges table (spec §9a.1/9a.2).
type Challenge struct {
	ID           string
	ChallengerDID string
	OpponentDID  *string
	GameType     string
	Variant      *string
	TimeControl  json.RawMessage
	Rated        bool
	Status       string
	ExpiresAt    *time.Time
	RepoURI      *string
	CreatedAt    *time.Time
}

// ChallengesRepo is CRUD for challenges.
type ChallengesRepo struct{ pool *pgxpool.Pool }

// NewChallengesRepo returns a challenges repository.
func NewChallengesRepo(pool *pgxpool.Pool) *ChallengesRepo { return &ChallengesRepo{pool} }

// Insert stores a new challenge.
func (r *ChallengesRepo) Insert(ctx context.Context, c *Challenge) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO challenges (id, challenger_did, opponent_did, game_type, variant,
		    time_control, rated, status, expires_at, repo_uri, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())`,
		c.ID, c.ChallengerDID, c.OpponentDID, c.GameType, c.Variant, c.TimeControl,
		c.Rated, c.Status, c.ExpiresAt, c.RepoURI)
	return err
}

// Get returns the challenge with id.
func (r *ChallengesRepo) Get(ctx context.Context, id string) (*Challenge, error) {
	var c Challenge
	err := r.pool.QueryRow(ctx,
		`SELECT id, challenger_did, opponent_did, game_type, variant, time_control,
		        rated, status, expires_at, repo_uri, created_at
		 FROM challenges WHERE id=$1`, id).
		Scan(&c.ID, &c.ChallengerDID, &c.OpponentDID, &c.GameType, &c.Variant, &c.TimeControl,
			&c.Rated, &c.Status, &c.ExpiresAt, &c.RepoURI, &c.CreatedAt)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &c, nil
}

// UpdateStatus moves a challenge through its lifecycle
// (open/pending/accepted/declined/cancelled/expired).
func (r *ChallengesRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE challenges SET status=$2 WHERE id=$1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetOpponent records the accepting opponent and pending status.
func (r *ChallengesRepo) SetOpponent(ctx context.Context, id string, opponentDID string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE challenges SET opponent_did=$2, status='pending' WHERE id=$1`, id, opponentDID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Seek mirrors the seeks table (spec §9a.3).
type Seek struct {
	ID            string
	DID           string
	Pool          string
	GameType      string
	Variant       *string
	TimeControl   json.RawMessage
	Rated         bool
	RatingWindow  *int
	MaxConcurrent *int
	Mode          string
	CreatedAt     *time.Time
	LastMatchedAt *time.Time
	Active        bool
}

// SeeksRepo is CRUD for seeks.
type SeeksRepo struct{ pool *pgxpool.Pool }

// NewSeeksRepo returns a seeks repository.
func NewSeeksRepo(pool *pgxpool.Pool) *SeeksRepo { return &SeeksRepo{pool} }

// Insert stores a new seek.
func (r *SeeksRepo) Insert(ctx context.Context, s *Seek) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO seeks (id, did, pool, game_type, variant, time_control, rated,
		    rating_window, max_concurrent, mode, created_at, last_matched_at, active)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),$11,$12)`,
		s.ID, s.DID, s.Pool, s.GameType, s.Variant, s.TimeControl, s.Rated,
		s.RatingWindow, s.MaxConcurrent, s.Mode, s.LastMatchedAt, s.Active)
	return err
}

// Get returns the seek with id.
func (r *SeeksRepo) Get(ctx context.Context, id string) (*Seek, error) {
	return scanSeek(r.pool.QueryRow(ctx, seekSelect+` WHERE id=$1`, id))
}

const seekSelect = `SELECT id, did, pool, game_type, variant, time_control, rated,
	rating_window, max_concurrent, mode, created_at, last_matched_at, active FROM seeks`

func scanSeek(row pgx.Row) (*Seek, error) {
	var s Seek
	err := row.Scan(&s.ID, &s.DID, &s.Pool, &s.GameType, &s.Variant, &s.TimeControl,
		&s.Rated, &s.RatingWindow, &s.MaxConcurrent, &s.Mode, &s.CreatedAt,
		&s.LastMatchedAt, &s.Active)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &s, nil
}

// SetActive toggles a seek's active flag (expiry, cancel, match).
func (r *SeeksRepo) SetActive(ctx context.Context, id string, active bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE seeks SET active=$2 WHERE id=$1`, id, active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchMatched stamps last_matched_at (spec §9a.3 repeat handling).
func (r *SeeksRepo) TouchMatched(ctx context.Context, id string, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE seeks SET last_matched_at=$2 WHERE id=$1`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListActive returns all active seeks in a pool.
func (r *SeeksRepo) ListActive(ctx context.Context, pool string) ([]Seek, error) {
	rows, err := r.pool.Query(ctx, seekSelect+` WHERE active AND pool=$1 ORDER BY created_at`, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Seek
	for rows.Next() {
		s, err := scanSeek(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// Flag mirrors the flags table (spec §10).
type Flag struct {
	ID         int64
	SubjectDID string
	GameURI    *string
	Kind       string
	Severity   string
	Detail     *string
	CreatedAt  *time.Time
	RepoURI    *string
}

// FlagsRepo is CRUD for flags.
type FlagsRepo struct{ pool *pgxpool.Pool }

// NewFlagsRepo returns a flags repository.
func NewFlagsRepo(pool *pgxpool.Pool) *FlagsRepo { return &FlagsRepo{pool} }

// Insert records a flag.
func (r *FlagsRepo) Insert(ctx context.Context, f *Flag) error {
	return r.pool.QueryRow(ctx,
		`INSERT INTO flags (subject_did, game_uri, kind, severity, detail, repo_uri)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
		f.SubjectDID, f.GameURI, f.Kind, f.Severity, f.Detail, f.RepoURI).
		Scan(&f.ID, &f.CreatedAt)
}

// ListBySubject returns a DID's flags, newest first.
func (r *FlagsRepo) ListBySubject(ctx context.Context, did string, limit int) ([]Flag, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, subject_did, game_uri, kind, severity, detail, created_at, repo_uri
		 FROM flags WHERE subject_did=$1 ORDER BY created_at DESC LIMIT $2`, did, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Flag
	for rows.Next() {
		var f Flag
		if err := rows.Scan(&f.ID, &f.SubjectDID, &f.GameURI, &f.Kind, &f.Severity,
			&f.Detail, &f.CreatedAt, &f.RepoURI); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Commentary mirrors the commentary table (spec §8).
type Commentary struct {
	URI           string
	CID           *string
	GameURI       string
	Ply           *int
	PlayerDID     *string
	Visibility    string
	Text          *string
	Ciphertext    []byte
	Nonce         []byte
	KeyID         *string
	ContentKey    []byte
	EscrowStatus  *string
	ReceivedAt    *time.Time
	RevealsAtPly  *int
	RevealsAt     *time.Time
	RevealedAt    *time.Time
}

// CommentaryRepo is CRUD for commentary.
type CommentaryRepo struct{ pool *pgxpool.Pool }

// NewCommentaryRepo returns a commentary repository.
func NewCommentaryRepo(pool *pgxpool.Pool) *CommentaryRepo { return &CommentaryRepo{pool} }

const commentaryColumns = `uri, cid, game_uri, ply, player_did, visibility, text,
	ciphertext, nonce, key_id, content_key, escrow_status, received_at, reveals_at_ply,
	reveals_at, revealed_at`

func scanCommentary(row pgx.Row) (*Commentary, error) {
	var c Commentary
	err := row.Scan(&c.URI, &c.CID, &c.GameURI, &c.Ply, &c.PlayerDID, &c.Visibility,
		&c.Text, &c.Ciphertext, &c.Nonce, &c.KeyID, &c.ContentKey, &c.EscrowStatus,
		&c.ReceivedAt, &c.RevealsAtPly, &c.RevealsAt, &c.RevealedAt)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &c, nil
}

// Insert stores an indexed commentary record.
func (r *CommentaryRepo) Insert(ctx context.Context, c *Commentary) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO commentary (`+commentaryColumns+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		c.URI, c.CID, c.GameURI, c.Ply, c.PlayerDID, c.Visibility, c.Text,
		c.Ciphertext, c.Nonce, c.KeyID, c.ContentKey, c.EscrowStatus,
		c.ReceivedAt, c.RevealsAtPly, c.RevealsAt, c.RevealedAt)
	return err
}

// Get returns the commentary record at uri.
func (r *CommentaryRepo) Get(ctx context.Context, uri string) (*Commentary, error) {
	return scanCommentary(r.pool.QueryRow(ctx,
		`SELECT `+commentaryColumns+` FROM commentary WHERE uri=$1`, uri))
}

// Reveal marks a record revealed.
func (r *CommentaryRepo) Reveal(ctx context.Context, uri string, revealedAt time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE commentary SET revealed_at=$2 WHERE uri=$1`, uri, revealedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListDue returns unrevealed records whose reveal time has come.
func (r *CommentaryRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]Commentary, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+commentaryColumns+` FROM commentary
		 WHERE revealed_at IS NULL AND reveals_at IS NOT NULL AND reveals_at <= $1
		 ORDER BY reveals_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Commentary
	for rows.Next() {
		c, err := scanCommentary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Actor mirrors the actors table.
type Actor struct {
	DID              string
	Handle           *string
	Profile          json.RawMessage
	Revision         *int
	ProfileHash      *string
	OperatorDID      *string
	OperatorVerified bool
	IndexedAt        *time.Time
}

// ActorsRepo is CRUD for actors.
type ActorsRepo struct{ pool *pgxpool.Pool }

// NewActorsRepo returns an actors repository.
func NewActorsRepo(pool *pgxpool.Pool) *ActorsRepo { return &ActorsRepo{pool} }

// Upsert inserts or refreshes an actor's handle/profile.
func (r *ActorsRepo) Upsert(ctx context.Context, a *Actor) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO actors (did, handle, profile, revision, profile_hash, operator_did, operator_verified, indexed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		 ON CONFLICT (did) DO UPDATE SET
		   handle=EXCLUDED.handle, profile=EXCLUDED.profile, revision=EXCLUDED.revision,
		   profile_hash=EXCLUDED.profile_hash, operator_did=EXCLUDED.operator_did,
		   operator_verified=EXCLUDED.operator_verified, indexed_at=now()`,
		a.DID, a.Handle, a.Profile, a.Revision, a.ProfileHash, a.OperatorDID, a.OperatorVerified)
	return err
}

// Get returns the actor by DID.
func (r *ActorsRepo) Get(ctx context.Context, did string) (*Actor, error) {
	var a Actor
	err := r.pool.QueryRow(ctx,
		`SELECT did, handle, profile, revision, profile_hash, operator_did, operator_verified, indexed_at
		 FROM actors WHERE did=$1`, did).
		Scan(&a.DID, &a.Handle, &a.Profile, &a.Revision, &a.ProfileHash,
			&a.OperatorDID, &a.OperatorVerified, &a.IndexedAt)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &a, nil
}
