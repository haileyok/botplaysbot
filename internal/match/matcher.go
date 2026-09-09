package match

// Matcher orchestrates Phase D: seek and challenge lifecycle, the pairing
// loop pass, expired-challenge reaping, standing-seek expiry off the
// presence registry, and no-show suspension. The matchmaking rules live in
// pairing.go (pure); this file is the wiring and the database transactions.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// Error codes returned by the matcher. Challenge/seek codes match the
// lexicon-declared error names; the rest follow the existing convention.
const (
	CodeChallengeNotFound    = "ChallengeNotFound"
	CodeChallengeUnavailable = "ChallengeUnavailable"
	CodeRateLimitExceeded    = "RateLimitExceeded" // >1 open challenge per DID per gameType (§10)
	CodeSeekNotFound         = "SeekNotFound"
	CodeForbidden            = "Forbidden"
	CodeInvalidRequest       = "InvalidRequest"
	CodeSuspended            = "SuspendedFromPool"
	CodeMaxConcurrentGames   = "MaxConcurrentGames"
)

// Seek modes and seat preferences (lexicon knownValues).
const (
	SeekModeOnce     = "once"
	SeekModeStanding = "standing"

	SeatPrefFirst  = "first"
	SeatPrefSecond = "second"
	SeatPrefRandom = "random"
)

// DefaultSeekMaxConcurrent is the per-seek concurrency default (lexicon:
// "Default 5") — distinct from the global §10 cap (config, default 20).
const DefaultSeekMaxConcurrent = 5

// NoShowThreshold is the consecutive ply-1/ply-2 timeout count that
// suspends a DID from the pool (spec §9a.3).
const NoShowThreshold = 3

// Flag kinds and severities produced here (spec §10).
const (
	FlagTimingAnomaly = "timingAnomaly"
	FlagSeverityInfo  = "info"
)

// Error is a matcher-domain failure; the XRPC layer maps Code to the error
// envelope name.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func matchError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Matcher owns matchmaking state and background loops. Construct with
// NewMatcher, wire games.Manager.SetFinishHook to OnFinish, call
// StartPairing, and Close when done.
type Matcher struct {
	cfg    *config.Config
	logger *slog.Logger
	pool   *pgxpool.Pool
	repos  *repo.Pool
	games  *games.Manager
	rating RatingSource
	hub    *Hub

	now  func() time.Time
	rand *rand.Rand

	stop chan struct{}
	done chan struct{}

	// noShow counts consecutive ply-1/ply-2 timeouts per DID in matched
	// games. In-memory (single-process AppView, like presence); the
	// suspension itself and the timingAnomaly flag row are durable rows.
	mu     sync.Mutex
	noShow map[string]int
}

// MatcherOption customizes construction (tests).
type MatcherOption func(*matcherOptions)

type matcherOptions struct {
	now  func() time.Time
	rand *rand.Rand
}

// WithNow overrides the matcher clock (tests).
func WithNow(now func() time.Time) MatcherOption {
	return func(o *matcherOptions) { o.now = now }
}

// WithRand overrides the seat tie-break source (tests: deterministic seeds).
func WithRand(r *rand.Rand) MatcherOption {
	return func(o *matcherOptions) { o.rand = r }
}

// NewMatcher assembles the matcher.
func NewMatcher(cfg *config.Config, pool *pgxpool.Pool, repos *repo.Pool, gm *games.Manager,
	rating RatingSource, logger *slog.Logger, opts ...MatcherOption) *Matcher {
	o := &matcherOptions{now: time.Now, rand: rand.New(rand.NewSource(time.Now().UnixNano()))}
	for _, opt := range opts {
		opt(o)
	}
	m := &Matcher{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		repos:  repos,
		games:  gm,
		rating: rating,
		hub:    NewHub(NewPresence()),
		now:    o.now,
		rand:   o.rand,
		noShow: map[string]int{},
	}
	return m
}

// Hub exposes the notification hub (match.subscribe attaches to it).
func (m *Matcher) Hub() *Hub { return m.hub }

// Close stops the pairing loop, if started.
func (m *Matcher) Close() {
	if m.stop != nil {
		close(m.stop)
		<-m.done
		m.stop = nil
	}
}

// StartPairing runs the pairing loop until Close: every interval it reaps
// expired challenges, expires stale standing seeks, and pairs pools.
func (m *Matcher) StartPairing(interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), interval)
				if _, err := m.PairOnce(ctx); err != nil {
					m.logger.Error("match: pairing pass failed", "err", err)
				}
				cancel()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Seeks (spec §9a.3)

// SeekParams mirrors the match.seek input (converted from lexicon types).
type SeekParams struct {
	GameType      string
	Variant       string
	TimeControl   clock.TimeControl
	Rated         bool
	RatingWindow  int64
	MaxConcurrent int64
	Mode          string
}

// Seek enters the caller into a pool, rejecting suspended DIDs.
func (m *Matcher) Seek(ctx context.Context, did string, p SeekParams) (*repo.Seek, error) {
	now := m.now()
	if p.Mode != SeekModeOnce && p.Mode != SeekModeStanding {
		return nil, matchError(CodeInvalidRequest, "mode must be %q or %q", SeekModeOnce, SeekModeStanding)
	}
	if !m.games.SupportsGameType(p.GameType) {
		return nil, matchError(CodeInvalidRequest, "unknown gameType %q", p.GameType)
	}
	if until, err := m.repos.Suspensions.ActiveUntil(ctx, did, now); err != nil {
		if !errors.Is(err, repo.ErrNotFound) {
			return nil, err
		}
	} else if until != nil {
		return nil, matchError(CodeSuspended, "suspended from the seek pool until %s", until.UTC().Format(time.RFC3339))
	}
	if p.MaxConcurrent < 0 {
		return nil, matchError(CodeInvalidRequest, "maxConcurrent must be >= 0")
	}

	variant := p.Variant
	if variant == "" {
		variant = engine.ChessVariantStandard
	}
	tc := p.TimeControl
	if tc.Kind == "" {
		tc = clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: int64(m.cfg.Tunables.PerMoveSeconds)}
	}
	if _, err := clock.DeadlineFor(tc, now); err != nil {
		return nil, matchError(CodeInvalidRequest, "time control: %v", err)
	}
	window := int(p.RatingWindow)
	if window == 0 {
		window = m.cfg.Tunables.RatingWindow
	}
	maxC := int(p.MaxConcurrent)
	if maxC == 0 {
		maxC = DefaultSeekMaxConcurrent
	}

	tcJSON, err := json.Marshal(tc)
	if err != nil {
		return nil, err
	}
	seek := &repo.Seek{
		ID:            tid.Next().String(),
		DID:           did,
		Pool:          PoolKey(p.GameType, variant, tc, p.Rated),
		GameType:      p.GameType,
		Variant:       &variant,
		TimeControl:   tcJSON,
		Rated:         p.Rated,
		RatingWindow:  &window,
		MaxConcurrent: &maxC,
		Mode:          p.Mode,
		Active:        true,
	}
	if err := m.repos.Seeks.Insert(ctx, seek); err != nil {
		return nil, fmt.Errorf("match: insert seek: %w", err)
	}
	return seek, nil
}

// CancelSeek removes the caller's seek; another DID's seek is SeekNotFound
// (does not leak existence).
func (m *Matcher) CancelSeek(ctx context.Context, did, seekID string) error {
	seek, err := m.repos.Seeks.Get(ctx, seekID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return matchError(CodeSeekNotFound, "no such seek")
		}
		return err
	}
	if seek.DID != did {
		return matchError(CodeSeekNotFound, "no such seek")
	}
	return m.repos.Seeks.SetActive(ctx, seekID, false)
}

// SeekStatus pairs a seek with its queue position (getSeeks output).
type SeekStatus struct {
	Seek          repo.Seek
	QueuePosition int
}

// GetSeeks returns the caller's active seeks with pool queue positions.
func (m *Matcher) GetSeeks(ctx context.Context, did string) ([]SeekStatus, error) {
	seeks, err := m.repos.Seeks.ListByDID(ctx, did, true)
	if err != nil {
		return nil, err
	}
	out := make([]SeekStatus, 0, len(seeks))
	for _, s := range seeks {
		pos := 1
		if poolmates, err := m.repos.Seeks.ListActive(ctx, s.Pool); err == nil {
			for _, other := range poolmates {
				if other.ID != s.ID && other.CreatedAt != nil && s.CreatedAt != nil &&
					other.CreatedAt.Before(*s.CreatedAt) {
					pos++
				}
			}
		}
		out = append(out, SeekStatus{Seek: s, QueuePosition: pos})
	}
	return out, nil
}

// PoolKey builds the pool identity (gameType:variant:timeControl:rated). It
// is also the game record's matchmaking.pool string (spec §9a.5).
func PoolKey(gameType, variant string, tc clock.TimeControl, rated bool) string {
	rating := "casual"
	if rated {
		rating = "rated"
	}
	return fmt.Sprintf("%s:%s:%s:%s", gameType, variant, normalizeTimeControl(tc), rating)
}

// normalizeTimeControl buckets a time control for pool membership: perMove
// seeks meet only on the same per-move budget (the lexicon default 300s
// makes that the common bucket), and other kinds key on the kind name until
// later phases add them.
func normalizeTimeControl(tc clock.TimeControl) string {
	switch tc.Kind {
	case clock.KindPerMove:
		return fmt.Sprintf("perMove%d", tc.PerMoveSeconds)
	default:
		return string(tc.Kind)
	}
}
