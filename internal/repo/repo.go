// Package repo provides plain pgx CRUD repositories for the AppView tables.
// No business logic lives here: callers own validation, sequencing, and
// policy. Nullables are pointers, jsonb columns are json.RawMessage, bytea
// columns are []byte.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// Pool wraps the pgx pool shared by all repositories and exposes the
// per-table repositories.
type Pool struct {
	*pgxpool.Pool
	Games       *GamesRepo
	Moves       *MovesRepo
	Challenges  *ChallengesRepo
	Seeks       *SeeksRepo
	Pairings    *PairingsRepo
	Seats       *SeatHistoryRepo
	Suspensions *SuspensionsRepo
	Flags       *FlagsRepo
	Commentary  *CommentaryRepo
	Actors      *ActorsRepo
}

// New wraps a pgx pool.
func New(pool *pgxpool.Pool) *Pool {
	return &Pool{
		Pool:        pool,
		Games:       NewGamesRepo(pool),
		Moves:       NewMovesRepo(pool),
		Challenges:  NewChallengesRepo(pool),
		Seeks:       NewSeeksRepo(pool),
		Pairings:    NewPairingsRepo(pool),
		Seats:       NewSeatHistoryRepo(pool),
		Suspensions: NewSuspensionsRepo(pool),
		Flags:       NewFlagsRepo(pool),
		Commentary:  NewCommentaryRepo(pool),
		Actors:      NewActorsRepo(pool),
	}
}

// Begin starts a transaction on the shared pool.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.Pool.Begin(ctx)
}

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
	// ClockAnchor is the receipt time the running clock derives from
	// (startedAt at creation, each accepted move's receivedAt after).
	ClockAnchor *time.Time
	// DrawOfferDID/DrawOfferPly are the pending draw offer, if any
	// (spec §5.6: expired when the offerer's next move is accepted).
	DrawOfferDID *string
	DrawOfferPly *int
	// Matchmaking carries the seek-pool provenance {pool, waitMs, ratingGap}
	// for matched games (spec §9a.5); nil for challenge-created games.
	Matchmaking json.RawMessage
	// ChallengeURI/ChallengeCID are the challenge strongRef for games
	// created from a repo-backed challenge (spec §9a.5); nil otherwise.
	ChallengeURI *string
	ChallengeCID *string
}

const gameColumns = `uri, cid, game_type, variant, status, players, time_control,
	commentary_delay, seed, state, ply, turn_did, result, created_at, started_at,
	finished_at, clock_anchor, draw_offer_did, draw_offer_ply, matchmaking,
	challenge_uri, challenge_cid`

func scanGame(row pgx.Row) (*Game, error) {
	var g Game
	err := row.Scan(&g.URI, &g.CID, &g.GameType, &g.Variant, &g.Status, &g.Players,
		&g.TimeControl, &g.CommentaryDelay, &g.Seed, &g.State, &g.Ply, &g.TurnDID,
		&g.Result, &g.CreatedAt, &g.StartedAt, &g.FinishedAt, &g.ClockAnchor,
		&g.DrawOfferDID, &g.DrawOfferPly, &g.Matchmaking, &g.ChallengeURI, &g.ChallengeCID)
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
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		g.URI, g.CID, g.GameType, g.Variant, g.Status, g.Players, g.TimeControl,
		g.CommentaryDelay, g.Seed, g.State, g.Ply, g.TurnDID, g.Result,
		g.CreatedAt, g.StartedAt, g.FinishedAt, g.ClockAnchor,
		g.DrawOfferDID, g.DrawOfferPly, g.Matchmaking, g.ChallengeURI, g.ChallengeCID)
	return err
}

// Get returns the game at uri.
func (r *GamesRepo) Get(ctx context.Context, uri string) (*Game, error) {
	return scanGame(r.pool.QueryRow(ctx,
		`SELECT `+gameColumns+` FROM games WHERE uri = $1`, uri))
}

// Update overwrites the mutable columns of the game at uri.
func (r *GamesRepo) Update(ctx context.Context, g *Game) error {
	return r.update(ctx, r.pool.Exec, g)
}

// execFunc abstracts pool vs transaction execution.
type execFunc = func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)

func (r *GamesRepo) update(ctx context.Context, ex execFunc, g *Game) error {
	tag, err := ex(ctx,
		`UPDATE games SET cid=$2, status=$3, players=$4, time_control=$5,
		   commentary_delay=$6, seed=$7, state=$8, ply=$9, turn_did=$10,
		   result=$11, started_at=$12, finished_at=$13, clock_anchor=$14,
		   draw_offer_did=$15, draw_offer_ply=$16
		 WHERE uri=$1`,
		g.URI, g.CID, g.Status, g.Players, g.TimeControl, g.CommentaryDelay,
		g.Seed, g.State, g.Ply, g.TurnDID, g.Result, g.StartedAt, g.FinishedAt,
		g.ClockAnchor, g.DrawOfferDID, g.DrawOfferPly)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateTx runs Update inside a transaction.
func (r *GamesRepo) UpdateTx(ctx context.Context, tx pgx.Tx, g *Game) error {
	return r.update(ctx, tx.Exec, g)
}

// GamesFilter selects games for ListFiltered.
type GamesFilter struct {
	Status   string // empty = any
	GameType string // empty = any
	Player   string // DID; empty = any
	// BeforeURI is the cursor: return games with uri < BeforeURI (list is
	// newest-first by uri). Empty starts from the newest.
	BeforeURI string
	Limit     int
}

// ListFiltered returns games matching the filter, newest-first by uri.
func (r *GamesRepo) ListFiltered(ctx context.Context, f GamesFilter) ([]*Game, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	sql := `SELECT ` + gameColumns + ` FROM games WHERE true`
	args := []any{}
	if f.Status != "" {
		args = append(args, f.Status)
		sql += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	if f.GameType != "" {
		args = append(args, f.GameType)
		sql += fmt.Sprintf(` AND game_type = $%d`, len(args))
	}
	if f.Player != "" {
		args = append(args, f.Player)
		sql += fmt.Sprintf(` AND players @> $%d::jsonb`, len(args))
		args[len(args)-1] = []byte(`[{"did":"` + f.Player + `"}]`)
	}
	if f.BeforeURI != "" {
		args = append(args, f.BeforeURI)
		sql += fmt.Sprintf(` AND uri < $%d`, len(args))
	}
	args = append(args, f.Limit)
	sql += fmt.Sprintf(` ORDER BY uri DESC LIMIT $%d`, len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Game
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListExpired returns active games whose perMove clock has passed their
// deadline at now (spec §7: the sweeper finishes these so an absent
// opponent does not need to poll). Only perMove games exist while active.
func (r *GamesRepo) ListExpired(ctx context.Context, now time.Time, limit int) ([]*Game, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+gameColumns+` FROM games
		 WHERE status = 'active' AND clock_anchor IS NOT NULL
		   AND time_control->>'kind' = 'perMove'
		   AND clock_anchor + make_interval(secs => (time_control->>'perMoveSeconds')::float8) <= $1
		 ORDER BY clock_anchor
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Game
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountActiveByPlayer counts the active games a DID appears in (spec §10
// concurrent-games cap). Players is the games.players jsonb array.
func (r *GamesRepo) CountActiveByPlayer(ctx context.Context, did string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM games WHERE status = 'active' AND players @> $1::jsonb`,
		[]byte(`[{"did":"`+did+`"}]`)).Scan(&n)
	return n, err
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
	return r.insert(ctx, r.pool.Exec, m)
}

// InsertTx runs Insert inside a transaction.
func (r *MovesRepo) InsertTx(ctx context.Context, tx pgx.Tx, m *Move) error {
	return r.insert(ctx, tx.Exec, m)
}

func (r *MovesRepo) insert(ctx context.Context, ex execFunc, m *Move) error {
	_, err := ex(ctx,
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
	ID              string
	ChallengerDID   string
	OpponentDID     *string
	GameType        string
	Variant         *string
	TimeControl     json.RawMessage
	CommentaryDelay json.RawMessage
	SeatPreference  string
	Rated           bool
	Status          string
	ExpiresAt       *time.Time
	RepoURI         *string
	CreatedAt       *time.Time
}

// Challenge statuses (spec §9a.1/9a.2 lifecycle).
const (
	ChallengeOpen      = "open"
	ChallengePending   = "pending" // direct challenge awaiting its opponent
	ChallengeAccepted  = "accepted"
	ChallengeDeclined  = "declined"
	ChallengeCancelled = "cancelled"
	ChallengeExpired   = "expired"
)

// ChallengesRepo is CRUD for challenges.
type ChallengesRepo struct{ pool *pgxpool.Pool }

// NewChallengesRepo returns a challenges repository.
func NewChallengesRepo(pool *pgxpool.Pool) *ChallengesRepo { return &ChallengesRepo{pool} }

const challengeColumns = `id, challenger_did, opponent_did, game_type, variant,
	time_control, commentary_delay, seat_preference, rated, status, expires_at,
	repo_uri, created_at`

func scanChallenge(row pgx.Row) (*Challenge, error) {
	var c Challenge
	err := row.Scan(&c.ID, &c.ChallengerDID, &c.OpponentDID, &c.GameType, &c.Variant,
		&c.TimeControl, &c.CommentaryDelay, &c.SeatPreference, &c.Rated, &c.Status,
		&c.ExpiresAt, &c.RepoURI, &c.CreatedAt)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &c, nil
}

// Insert stores a new challenge.
func (r *ChallengesRepo) Insert(ctx context.Context, c *Challenge) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO challenges (id, challenger_did, opponent_did, game_type, variant,
		    time_control, commentary_delay, seat_preference, rated, status, expires_at,
		    repo_uri, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now())`,
		c.ID, c.ChallengerDID, c.OpponentDID, c.GameType, c.Variant, c.TimeControl,
		c.CommentaryDelay, c.SeatPreference, c.Rated, c.Status, c.ExpiresAt, c.RepoURI)
	return err
}

// Get returns the challenge with id.
func (r *ChallengesRepo) Get(ctx context.Context, id string) (*Challenge, error) {
	return scanChallenge(r.pool.QueryRow(ctx,
		`SELECT `+challengeColumns+` FROM challenges WHERE id=$1`, id))
}

// CountOpenBy counts a challenger's still-live open challenges for one game
// type (spec §10: at most one open challenge per DID per game type).
func (r *ChallengesRepo) CountOpenBy(ctx context.Context, challengerDID, gameType string, now time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM challenges
		 WHERE challenger_did=$1 AND game_type=$2 AND opponent_did IS NULL
		   AND status=$3 AND (expires_at IS NULL OR expires_at > $4)`,
		challengerDID, gameType, ChallengeOpen, now).Scan(&n)
	return n, err
}

// TransitionStatus moves a challenge from any of the from-statuses to the
// target status, reporting whether the row was updated. The conditional
// update is the accept race arbiter: exactly one concurrent acceptor wins.
func (r *ChallengesRepo) TransitionStatus(ctx context.Context, id string, from []string, to string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE challenges SET status=$3 WHERE id=$1 AND status = ANY($2)`, id, from, to)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateStatus moves a challenge through its lifecycle unconditionally.
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

// ChallengesFilter selects rows for List.
type ChallengesFilter struct {
	GameType string // empty = any
	Opponent string // DID; empty = any
	Open     *bool  // nil = any; true = open only, false = non-open only
	BeforeID string // cursor: id < BeforeID (ids are k-ordered TIDs)
	Limit    int
}

// List returns live (open or pending, unexpired) challenges matching the
// filter, newest first (spec §5.5: listChallenges excludes expired).
func (r *ChallengesRepo) List(ctx context.Context, f ChallengesFilter, now time.Time) ([]*Challenge, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	sql := `SELECT ` + challengeColumns + ` FROM challenges
		WHERE status IN ($1,$2) AND (expires_at IS NULL OR expires_at > $3)`
	args := []any{ChallengeOpen, ChallengePending, now}
	if f.GameType != "" {
		args = append(args, f.GameType)
		sql += fmt.Sprintf(` AND game_type = $%d`, len(args))
	}
	if f.Opponent != "" {
		args = append(args, f.Opponent)
		sql += fmt.Sprintf(` AND opponent_did = $%d`, len(args))
	}
	if f.Open != nil {
		if *f.Open {
			args = append(args, ChallengeOpen)
			sql += fmt.Sprintf(` AND status = $%d`, len(args))
		} else {
			args = append(args, ChallengeOpen)
			sql += fmt.Sprintf(` AND status <> $%d`, len(args))
		}
	}
	if f.BeforeID != "" {
		args = append(args, f.BeforeID)
		sql += fmt.Sprintf(` AND id < $%d`, len(args))
	}
	args = append(args, f.Limit)
	sql += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Challenge
	for rows.Next() {
		c, err := scanChallenge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ExpireDue flips every live challenge past its expiry to expired, returning
// how many rows changed. Rides the pairing loop (Phase D).
func (r *ChallengesRepo) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE challenges SET status=$2
		 WHERE status IN ($3,$4) AND expires_at IS NOT NULL AND expires_at <= $1`,
		now, ChallengeExpired, ChallengeOpen, ChallengePending)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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

// ListAllActive returns every active seek across all pools (the pairing loop
// groups by pool itself).
func (r *SeeksRepo) ListAllActive(ctx context.Context) ([]Seek, error) {
	rows, err := r.pool.Query(ctx, seekSelect+` WHERE active ORDER BY created_at`)
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

// ListByDID returns a DID's seeks, newest first, optionally active-only.
func (r *SeeksRepo) ListByDID(ctx context.Context, did string, activeOnly bool) ([]Seek, error) {
	sql := seekSelect + ` WHERE did=$1`
	if activeOnly {
		sql += ` AND active`
	}
	sql += ` ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, sql, did)
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

// CountActiveByDID counts a DID's active seeks (a duplicate-seek guard).
func (r *SeeksRepo) CountActiveByDID(ctx context.Context, did string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM seeks WHERE did=$1 AND active`, did).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Pairing history (spec §9a.3 repeat cooldown) and seat history
// (seat alternation)

// PairingsRepo records matchmade pairings for the repeat-cooldown check.
type PairingsRepo struct{ pool *pgxpool.Pool }

// NewPairingsRepo returns a pairing-history repository.
func NewPairingsRepo(pool *pgxpool.Pool) *PairingsRepo { return &PairingsRepo{pool} }

// Add records one matched pairing. didA/didB are stored in sorted order so
// lookups need not consider both orientations.
func (r *PairingsRepo) Add(ctx context.Context, didA, didB, gameType, gameURI string) error {
	if didB < didA {
		didA, didB = didB, didA
	}
	_, err := r.pool.Exec(ctx,
		`INSERT INTO pairing_history (did_a, did_b, game_type, game_uri)
		 VALUES ($1,$2,$3,$4)`, didA, didB, gameType, gameURI)
	return err
}

// RecentPairs returns the unordered DID pairs that played the game type
// within the window ending at now (the repeat-cooldown set, spec §9a.3).
// Each pair is [didA, didB] in sorted order.
func (r *PairingsRepo) RecentPairs(ctx context.Context, gameType string, since time.Time) ([][2]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT did_a, did_b FROM pairing_history
		 WHERE game_type=$1 AND created_at > $2`, gameType, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// SeatHistoryRepo tracks each agent's most recent seat per game type so
// matchmaking can alternate first move (spec §9a.3 step 5).
type SeatHistoryRepo struct{ pool *pgxpool.Pool }

// NewSeatHistoryRepo returns a seat-history repository.
func NewSeatHistoryRepo(pool *pgxpool.Pool) *SeatHistoryRepo { return &SeatHistoryRepo{pool} }

// LastSeat returns the DID's most recent seat for the game type; ok is false
// when the DID has no history yet.
func (r *SeatHistoryRepo) LastSeat(ctx context.Context, did, gameType string) (seat string, ok bool, err error) {
	err = r.pool.QueryRow(ctx,
		`SELECT last_seat FROM seat_history WHERE did=$1 AND game_type=$2`, did, gameType).
		Scan(&seat)
	if err != nil {
		return "", false, mapNotFound(err)
	}
	return seat, true, nil
}

// SetLastSeat persists the seat a DID just played, with the update time.
func (r *SeatHistoryRepo) SetLastSeat(ctx context.Context, did, gameType, seat string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO seat_history (did, game_type, last_seat, updated_at)
		 VALUES ($1,$2,$3,now())
		 ON CONFLICT (did, game_type) DO UPDATE SET last_seat=$3, updated_at=now()`,
		did, gameType, seat)
	return err
}

// SuspensionsRepo tracks DIDs barred from the seek pool (spec §9a.3 no-show
// suspension). One row per DID; a new suspension overwrites an old one.
type SuspensionsRepo struct{ pool *pgxpool.Pool }

// NewSuspensionsRepo returns a suspensions repository.
func NewSuspensionsRepo(pool *pgxpool.Pool) *SuspensionsRepo { return &SuspensionsRepo{pool} }

// Suspend bars the DID from the seek pool until until.
func (r *SuspensionsRepo) Suspend(ctx context.Context, did string, until time.Time, reason string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO seek_suspensions (did, until, reason, updated_at)
		 VALUES ($1,$2,$3,now())
		 ON CONFLICT (did) DO UPDATE SET until=$2, reason=$3, updated_at=now()`,
		did, until, reason)
	return err
}

// ActiveUntil reports the DID's active suspension end, if any.
func (r *SuspensionsRepo) ActiveUntil(ctx context.Context, did string, now time.Time) (*time.Time, error) {
	var until *time.Time
	err := r.pool.QueryRow(ctx,
		`SELECT until FROM seek_suspensions WHERE did=$1 AND until > $2`, did, now).
		Scan(&until)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return until, nil
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
	URI          string
	CID          *string
	GameURI      string
	Ply          *int
	PlayerDID    *string
	Visibility   string
	Text         *string
	Ciphertext   []byte
	Nonce        []byte
	KeyID        *string
	ContentKey   []byte
	EscrowStatus *string
	ReceivedAt   *time.Time
	RevealsAtPly *int
	RevealsAt    *time.Time
	RevealedAt   *time.Time
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
