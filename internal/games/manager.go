package games

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"crypto/ed25519"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/escrow"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot/comatproto"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/servicerepo"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// ---------------------------------------------------------------------------
// Errors

// Error codes returned by the manager. Submit-move codes match the
// generated lexicon error constants (spec §5.1); the draw/resign endpoints
// reuse GameNotActive/NotYourTurn plus the standard XRPC names.
const (
	CodeGameNotActive    = "GameNotActive"
	CodeNotYourTurn      = "NotYourTurn"
	CodePlyMismatch      = "PlyMismatch"
	CodeIllegalMove      = "IllegalMove"
	CodeClockExpired     = "ClockExpired"
	CodeMalformedPayload = "MalformedPayload"
	CodeNotFound         = "NotFound"
	CodeInvalidRequest   = "InvalidRequest"
	CodeForbidden        = "Forbidden"
)

// Error is a manager-domain failure. The XRPC layer maps Code to the
// response envelope's error name.
type Error struct {
	Code    string
	Message string
	// Result is set on ClockExpired: the timeout result the game was
	// finished with, so the rejected caller learns the outcome (spec §5.1).
	Result *events.Result
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func gameError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// Views

// GamePlayer is one seat holder with the profile snapshot taken at game
// start (spec §4.3/§4.4 playerSnapshots; absent profile → nil fields).
type GamePlayer struct {
	DID             string  `json:"did"`
	Seat            string  `json:"seat"`
	ProfileRevision *int64  `json:"profileRevision,omitempty"`
	ProfileHash     *string `json:"profileHash,omitempty"`
}

// MoveEntry is one accepted move in a game's history.
type MoveEntry struct {
	Ply              int64           `json:"ply"`
	Player           string          `json:"player"`
	Payload          json.RawMessage `json:"payload"`
	ReceivedAt       time.Time       `json:"receivedAt"`
	SAN              string          `json:"san,omitempty"`
	ClockRemainingMs *int64          `json:"clockRemainingMs,omitempty"`
	Token            string          `json:"-"`
}

// GameView is the assembled authoritative state of one game. The XRPC layer
// renders it into the getState/submitMove/listGames output types.
type GameView struct {
	URI             string
	CID             *string
	Rkey            string
	GameType        string
	Variant         *string
	Status          string
	Players         []GamePlayer
	TimeControl     clock.TimeControl
	CommentaryDelay *config.CommentaryDelay
	Seed            []byte
	Ply             int64
	TurnDID         string
	Result          *events.Result
	CreatedAt       *time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	Position        engine.Position
	LegalMoves      []engine.Payload
	Clocks          []events.Clock
	History         []MoveEntry
	// Commentary holds the getState commentary summaries (spec §5.2):
	// text only when revealed, reveal bounds on unrevealed delayed items.
	Commentary []CommentaryEntry
	// DrawOfferDID is the pending draw offer holder, if any.
	DrawOfferDID string
	// ServerTime is the AppView time the view was assembled at.
	ServerTime time.Time
}

// ---------------------------------------------------------------------------
// Token minter

// TokenMinter signs moveTokens (spec Appendix B). Production wires the
// service Ed25519 key; tests can substitute.
type TokenMinter interface {
	Mint(playerDID, gameURI string, ply int64, payload json.RawMessage, receivedAt time.Time) (string, error)
}

// FinishInfo describes one game just finished, for observers registered via
// SetFinishHook (Phase D: no-show detection rides this; Phase E: ratings).
type FinishInfo struct {
	GameURI  string
	GameType string
	Variant  string
	Players  []GamePlayer
	// Ply is the number of accepted moves when the game ended.
	Ply int
	// TurnDID is the player who was on the move at the end (for a timeout,
	// the player who lost on time).
	TurnDID string
	// Matched reports whether the game came from the seek pool (it carries
	// matchmaking provenance, spec §9a.5).
	Matched bool
	// Result is the terminal result.
	Result *events.Result
}

// FinishHook observes finished games. It runs inline with the finish path,
// so it must be fast and must not call back into the Manager.
type FinishHook func(FinishInfo)

type ed25519Minter struct {
	serviceDID string
	priv       ed25519.PrivateKey
}

// NewTokenMinter returns the production Ed25519 moveToken minter.
func NewTokenMinter(serviceDID string, priv ed25519.PrivateKey) TokenMinter {
	return &ed25519Minter{serviceDID: serviceDID, priv: priv}
}

func (m *ed25519Minter) Mint(playerDID, gameURI string, ply int64, payload json.RawMessage, receivedAt time.Time) (string, error) {
	return mintMoveToken(m.priv, m.serviceDID, playerDID, gameURI, ply, payload, receivedAt)
}

// MintReceipt signs a postCommentary receiptToken (spec §5.7) with the
// same service key as moveTokens.
func (m *ed25519Minter) MintReceipt(playerDID, gameURI string, ply int64, digest string, receivedAt time.Time) (string, error) {
	return mintReceiptToken(m.priv, m.serviceDID, playerDID, gameURI, ply, digest, receivedAt)
}

// ReceiptMinter is the optional receiptToken-signing capability of a
// TokenMinter. Production Ed25519 minter implements it; test fakes need
// it only when they exercise postCommentary.
type ReceiptMinter interface {
	MintReceipt(playerDID, gameURI string, ply int64, digest string, receivedAt time.Time) (string, error)
}

// ---------------------------------------------------------------------------
// Manager

// Manager owns game lifecycle: creation, move submission, draws, resign,
// timeouts, repo record publication, and event emission (spec §2.2, §5.1–
// §5.3, §5.6, §7). Construct with NewManager; call Close to stop the
// background sweeper.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger
	repos  *repo.Pool
	pool   *pgxpool.Pool

	engines engine.Registry
	minter  TokenMinter
	writer  *servicerepo.Writer
	bus     *events.Bus
	// escrow resolves AppView escrow rotations for postCommentary
	// validation and the reveal scheduler (spec §8.1). Optional until
	// wired via WithEscrowKeys; nil disables unwrap-dependent behavior.
	escrow keys.EscrowKeyDirectory

	// finishHook observes game finishes (set via SetFinishHook before any
	// game can finish; read without a lock because registration is boot-time).
	finishHook FinishHook

	now func() time.Time

	sweepStop  chan struct{}
	sweepDone  chan struct{}
	revealStop chan struct{}
	revealDone chan struct{}
}

// ManagerOption customizes construction (tests).
type ManagerOption func(*managerOptions)

type managerOptions struct {
	now    func() time.Time
	bus    *events.Bus
	escrow keys.EscrowKeyDirectory
}

// WithNow overrides the manager's clock (tests).
func WithNow(now func() time.Time) ManagerOption {
	return func(o *managerOptions) { o.now = now }
}

// WithBus injects a bus instead of creating one (tests that inspect events).
func WithBus(bus *events.Bus) ManagerOption {
	return func(o *managerOptions) { o.bus = bus }
}

// WithEscrowKeys injects the escrow key directory (postCommentary unwrap,
// reveal scheduler, game-end reveal; spec §8.1–§8.2).
func WithEscrowKeys(dir keys.EscrowKeyDirectory) ManagerOption {
	return func(o *managerOptions) { o.escrow = dir }
}

// NewManager assembles the game manager.
func NewManager(cfg *config.Config, pool *pgxpool.Pool, repos *repo.Pool, engines engine.Registry,
	minter TokenMinter, writer *servicerepo.Writer, logger *slog.Logger, opts ...ManagerOption) *Manager {
	o := &managerOptions{now: time.Now}
	for _, opt := range opts {
		opt(o)
	}
	bus := o.bus
	if bus == nil {
		bus = events.NewBus()
	}
	return &Manager{
		cfg:     cfg,
		logger:  logger,
		repos:   repos,
		pool:    pool,
		engines: engines,
		minter:  minter,
		writer:  writer,
		bus:     bus,
		escrow:  o.escrow,
		now:     o.now,
	}
}

// Bus exposes the event bus (Phase D attaches WS subscriptions).
func (m *Manager) Bus() *events.Bus { return m.bus }

// SetFinishHook registers an observer for finished games (Phase D no-show
// detection; Phase E ratings). Register once at boot, before games can
// finish; the hook runs inline with finish paths (submit-expiry, sweeper,
// resign, draw) and must not call back into the Manager.
func (m *Manager) SetFinishHook(h FinishHook) { m.finishHook = h }

// SupportsGameType reports whether an engine is registered for gameType
// (matchmaking validates seeks and challenges against it).
func (m *Manager) SupportsGameType(gameType string) bool {
	_, err := m.engines.Get(gameType)
	return err == nil
}

// SeatOrder returns the engine's seats in turn order for the variant:
// SeatOrder[0] is the first mover's seat.
func (m *Manager) SeatOrder(gameType, variant string) ([]string, error) {
	eng, err := m.engines.Get(gameType)
	if err != nil {
		return nil, err
	}
	if variant == "" {
		variant = engine.ChessVariantStandard
	}
	return eng.Seats(variant)
}

// Close stops the background loops (clock sweeper, reveal scheduler), if
// started.
func (m *Manager) Close() {
	if m.sweepStop != nil {
		close(m.sweepStop)
		<-m.sweepDone
		m.sweepStop = nil
	}
	if m.revealStop != nil {
		close(m.revealStop)
		<-m.revealDone
		m.revealStop = nil
	}
}

// ---------------------------------------------------------------------------
// Game creation

// CreateParams describes a game to create. Challenge acceptance and
// matchmaking (Phase D) both land here.
type CreateParams struct {
	GameType        string
	Variant         string
	Players         []PlayerSpec
	TimeControl     clock.TimeControl
	CommentaryDelay *config.CommentaryDelay
	Seed            []byte
	// StartedAt is the clock start: the receipt time of the event that
	// started the game (spec §7). The first mover's deadline derives from it.
	StartedAt time.Time
	// Challenge is the challenge strongRef carried on the game record when
	// the game came from a repo-backed challenge (spec §9a.5). XRPC-created
	// challenges have no indexed record (the Phase E indexer will index
	// bot.plays.bot.game.challenge), so challenge-acceptance passes nil and
	// the record carries no strongRef until a future backfill.
	Challenge *ChallengeRef
	// Matchmaking carries the seek-pool provenance written onto the record
	// for matchmade games (spec §9a.5). Nil for challenge/direct games.
	Matchmaking *MatchmakingInfo
}

// ChallengeRef is the com.atproto.repo.strongRef for the challenge record a
// game was created from.
type ChallengeRef struct {
	URI string
	CID string
}

// MatchmakingInfo is the game record's matchmaking object (spec §9a.5):
// which pool paired the game, how long the longer-waiting side waited, and
// the rating gap between the players.
type MatchmakingInfo struct {
	Pool      string
	WaitMs    int64
	RatingGap int64
}

// PlayerSpec pairs a DID with a seat.
type PlayerSpec struct {
	DID  string
	Seat string
}

// CreatedGame is a freshly created game.
type CreatedGame struct {
	URI  string
	CID  string
	Rkey string
	View *GameView
}

// CreateGame creates an active game: the service-repo game record (status
// active, spec §4.4) and the DB row, snapshotting player profiles and
// starting the clock at StartedAt.
func (m *Manager) CreateGame(ctx context.Context, p CreateParams) (*CreatedGame, error) {
	eng, err := m.engines.Get(p.GameType)
	if err != nil {
		return nil, gameError(CodeInvalidRequest, "%v", err)
	}
	variant := p.Variant
	if variant == "" {
		variant = engine.ChessVariantStandard
	}
	seats, err := eng.Seats(variant)
	if err != nil {
		return nil, gameError(CodeInvalidRequest, "%v", err)
	}
	if len(p.Players) != len(seats) {
		return nil, gameError(CodeInvalidRequest, "want %d players, got %d", len(seats), len(p.Players))
	}
	for i, ps := range p.Players {
		if ps.Seat != seats[i] {
			return nil, gameError(CodeInvalidRequest, "player %d seat %q, want %q", i, ps.Seat, seats[i])
		}
	}
	// The clock must be constructible: v1 accepts perMove only (spec §7).
	if _, err := clock.DeadlineFor(p.TimeControl, p.StartedAt); err != nil {
		return nil, gameError(CodeInvalidRequest, "time control: %v", err)
	}

	st, err := eng.InitialState(variant, p.Seed)
	if err != nil {
		return nil, gameError(CodeInvalidRequest, "%v", err)
	}
	snap, err := m.snapshot(eng, st)
	if err != nil {
		return nil, err
	}

	now := m.now()
	players := make([]GamePlayer, 0, len(p.Players))
	for _, ps := range p.Players {
		gp := GamePlayer{DID: ps.DID, Seat: ps.Seat}
		// Profile snapshot (spec §4.3): absent profile → NULL fields.
		actor, err := m.repos.Actors.Get(ctx, ps.DID)
		if err == nil {
			if actor.Revision != nil {
				rev := int64(*actor.Revision)
				gp.ProfileRevision = &rev
			}
			gp.ProfileHash = actor.ProfileHash
		} else if !errors.Is(err, repo.ErrNotFound) {
			return nil, fmt.Errorf("games: profile snapshot %s: %w", ps.DID, err)
		}
		players = append(players, gp)
	}

	tcJSON, err := json.Marshal(p.TimeControl)
	if err != nil {
		return nil, fmt.Errorf("games: marshal time control: %w", err)
	}
	var delayJSON json.RawMessage
	if p.CommentaryDelay != nil {
		delayJSON, err = json.Marshal(*p.CommentaryDelay)
		if err != nil {
			return nil, fmt.Errorf("games: marshal commentary delay: %w", err)
		}
	}
	playersJSON, err := json.Marshal(players)
	if err != nil {
		return nil, fmt.Errorf("games: marshal players: %w", err)
	}
	var mmJSON json.RawMessage
	if p.Matchmaking != nil {
		mmJSON, err = json.Marshal(p.Matchmaking)
		if err != nil {
			return nil, fmt.Errorf("games: marshal matchmaking: %w", err)
		}
	}

	// Mint the record key so the game URI is known before the PDS write.
	rkey := tid.Next().String()
	uri := gameURI(m.serviceDID(), rkey)

	row := &repo.Game{
		URI:             uri,
		GameType:        p.GameType,
		Variant:         &variant,
		Status:          "active",
		Players:         playersJSON,
		TimeControl:     tcJSON,
		CommentaryDelay: delayJSON,
		Seed:            p.Seed,
		State:           snap,
		Ply:             0,
		TurnDID:         &p.Players[0].DID,
		CreatedAt:       &now,
		StartedAt:       &p.StartedAt,
		ClockAnchor:     &p.StartedAt,
		Matchmaking:     mmJSON,
	}
	if p.Challenge != nil {
		ref := *p.Challenge
		row.ChallengeURI, row.ChallengeCID = &ref.URI, &ref.CID
	}

	// Write the game record to the service repo. An inert service account is
	// fine (local dev); a PDS error aborts creation before the row exists.
	rec := m.gameRecord(row, players, nil, nil)
	if _, err := m.writer.WriteGameRecordAtRkey(ctx, rkey, rec); err != nil {
		if errors.Is(err, servicerepo.ErrNotConfigured) {
			m.logger.Warn("games: service repo not configured; game record not written", "game", uri)
		} else {
			return nil, fmt.Errorf("games: write game record: %w", err)
		}
	}

	if err := m.repos.Games.Insert(ctx, row); err != nil {
		return nil, fmt.Errorf("games: insert game: %w", err)
	}

	view, err := m.assemble(ctx, row, eng, st, now)
	if err != nil {
		return nil, err
	}

	m.bus.Publish(events.Event{Kind: events.KindGameStarted, GameURI: uri})

	return &CreatedGame{URI: uri, CID: deref(row.CID), Rkey: rkey, View: view}, nil
}

// ---------------------------------------------------------------------------
// Move submission (spec §2.2)

// SubmitMoveParams carries one move submission. ReceivedAt must be stamped
// by the HTTP handler at request-parse completion (spec §2.2 step 2, §7).
type SubmitMoveParams struct {
	GameURI    string
	PlayerDID  string
	Ply        int64
	Payload    engine.Payload
	ReceivedAt time.Time
}

// Acceptance is the verbatim record of an accepted move. Idempotent replays
// return the original acceptance unchanged (same token, receivedAt, clock).
type Acceptance struct {
	GameURI          string
	Ply              int64
	PlayerDID        string
	Payload          json.RawMessage
	SAN              string
	ReceivedAt       time.Time
	ClockRemainingMs int64
	MoveToken        string
	// Terminal is set when the move ended the game.
	Terminal *events.Result
}

// SubmitMove validates and applies a move per spec §2.2: existence and
// activity, player, idempotent replay, turn, ply, clock, engine legality;
// then one transaction writes the move row and the new game state. A move
// that ends the game finishes it in the same transaction and performs the
// game-record update and event publication afterwards.
func (m *Manager) SubmitMove(ctx context.Context, p SubmitMoveParams) (*Acceptance, error) {
	g, err := m.repos.Games.Get(ctx, p.GameURI)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, gameError(CodeGameNotActive, "no such game")
	}
	if err != nil {
		return nil, err
	}
	if g.Status != "active" {
		return nil, gameError(CodeGameNotActive, "game is %s", g.Status)
	}
	if p.Ply < 1 {
		return nil, gameError(CodePlyMismatch, "ply must be >= 1")
	}

	eng, st, moves, players, err := m.loadGame(ctx, g)
	if err != nil {
		return nil, err
	}
	seat, err := seatOf(players, p.PlayerDID)
	if err != nil {
		// Not a player: they are never the player to move. Spec §5.1
		// defines no separate error name, so NotYourTurn carries it.
		return nil, gameError(CodeNotYourTurn, "caller is not a player in this game")
	}

	// Idempotency (spec §5.1): a retried submission for an already-accepted
	// ply replays the original acceptance verbatim; the same ply with a
	// different payload is a PlyMismatch. This precedes the turn check so a
	// retry never fails with NotYourTurn.
	if p.Ply <= int64(len(moves)) {
		mv := moves[p.Ply-1]
		eq, err := payloadsEqual(mv.Payload, p.Payload)
		if err != nil {
			return nil, gameError(CodeMalformedPayload, "%v", err)
		}
		if !eq {
			return nil, gameError(CodePlyMismatch, "ply %d was accepted with a different payload", p.Ply)
		}
		acc := &Acceptance{
			GameURI:          g.URI,
			Ply:              p.Ply,
			PlayerDID:        deref(mv.PlayerDID),
			Payload:          mv.Payload,
			SAN:              deref(mv.Notation),
			ReceivedAt:       mv.ReceivedAt,
			ClockRemainingMs: derefI64(mv.ClockRemainingMs),
			MoveToken:        deref(mv.Token),
		}
		if g.Result != nil && p.Ply == int64(g.Ply) {
			var r events.Result
			if err := json.Unmarshal(g.Result, &r); err == nil {
				acc.Terminal = &r
			}
		}
		return acc, nil
	}

	// Turn check: only the on-move player may move.
	if g.TurnDID == nil || *g.TurnDID != p.PlayerDID {
		return nil, gameError(CodeNotYourTurn, "it is not your turn")
	}
	if p.Ply != int64(g.Ply)+1 {
		return nil, gameError(CodePlyMismatch, "expected ply %d, got %d", g.Ply+1, p.Ply)
	}

	// Clock (spec §7): deadline = clock anchor + perMoveSeconds. A late move
	// is rejected with ClockExpired and the game is finished by timeout.
	tc, err := decodeTimeControl(g.TimeControl)
	if err != nil {
		return nil, gameError(CodeInvalidRequest, "%v", err)
	}
	anchor := g.ClockAnchor
	if anchor == nil {
		anchor = g.StartedAt
	}
	if anchor == nil {
		return nil, gameError(CodeInvalidRequest, "game has no clock anchor")
	}
	remaining, err := clock.RemainingMs(tc, *anchor, p.ReceivedAt)
	if err != nil {
		return nil, gameError(CodeInvalidRequest, "%v", err)
	}
	if remaining <= 0 {
		result := m.timeoutResult(g, players)
		if err := m.finishTimeout(ctx, g, st, result, p.ReceivedAt); err != nil {
			return nil, err
		}
		return nil, &Error{
			Code:    CodeClockExpired,
			Message: "clock expired; game finished: " + resultSummary(result),
			Result:  result,
		}
	}

	// Engine validation: shape violations are MalformedPayload, legality
	// violations IllegalMove with detail (spec §5.1).
	if err := eng.Validate(st, seat, p.Payload); err != nil {
		return nil, engineError(err)
	}
	san, err := eng.CanonicalNotation(st, p.Payload)
	if err != nil {
		return nil, engineError(err)
	}
	st2, err := eng.Apply(st, p.Payload)
	if err != nil {
		return nil, engineError(err)
	}
	term, err := eng.Terminal(st2)
	if err != nil {
		return nil, err
	}

	// Canonical payload form for storage and the moveToken claim.
	canonicalRaw, err := engine.CanonicalJSON(p.Payload)
	if err != nil {
		return nil, gameError(CodeMalformedPayload, "%v", err)
	}
	canonical := json.RawMessage(canonicalRaw)

	token, err := m.minter.Mint(p.PlayerDID, g.URI, p.Ply, canonical, p.ReceivedAt)
	if err != nil {
		return nil, fmt.Errorf("games: mint moveToken: %w", err)
	}

	// Next seat + draw-offer expiry (spec §5.6: an unanswered offer expires
	// when the offerer's next move is accepted).
	nextDID := nextSeatDID(players, seat)
	offerCleared := g.DrawOfferDID != nil && *g.DrawOfferDID == p.PlayerDID

	var result *events.Result
	if term != nil {
		result = resultFromTerminal(term, players)
	}

	// One transaction: the move row and the new game state commit together
	// (spec §2.2 step 4).
	snap2, err := m.snapshot(eng, st2)
	if err != nil {
		return nil, err
	}
	tx, err := m.repos.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	if err := m.repos.Moves.InsertTx(ctx, tx, &repo.Move{
		GameURI:          g.URI,
		Ply:              int(p.Ply),
		PlayerDID:        &p.PlayerDID,
		Payload:          canonical,
		Notation:         &san,
		ReceivedAt:       p.ReceivedAt,
		ClockRemainingMs: &remaining,
		Token:            &token,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// A concurrent writer accepted this ply first: treat this call
			// as the idempotent retry it syntactically is.
			tx.Rollback(context.WithoutCancel(ctx))
			return m.SubmitMove(ctx, p)
		}
		return nil, fmt.Errorf("games: insert move: %w", err)
	}

	g.Ply = int(p.Ply)
	g.TurnDID = &nextDID
	g.State = snap2
	g.ClockAnchor = &p.ReceivedAt
	if offerCleared {
		g.DrawOfferDID = nil
		g.DrawOfferPly = nil
	}
	if result != nil {
		resJSON, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		g.Status = "finished"
		g.Result = resJSON
		g.FinishedAt = &p.ReceivedAt
	}
	if err := m.repos.Games.UpdateTx(ctx, tx, g); err != nil {
		return nil, fmt.Errorf("games: update game: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("games: commit move: %w", err)
	}

	acc := &Acceptance{
		GameURI:          g.URI,
		Ply:              p.Ply,
		PlayerDID:        p.PlayerDID,
		Payload:          canonical,
		SAN:              san,
		ReceivedAt:       p.ReceivedAt,
		ClockRemainingMs: remaining,
		MoveToken:        token,
		Terminal:         result,
	}

	// Post-commit: the move event, then game-finished side effects.
	pos, perr := eng.Position(st2)
	if perr != nil {
		return nil, perr
	}
	clockNext, _ := m.clockFor(tc, &p.ReceivedAt, nextDID, m.now())
	ev := events.Event{Kind: events.KindMove, GameURI: g.URI}
	ev.Move.Ply = p.Ply
	ev.Move.Player = p.PlayerDID
	ev.Move.Payload = canonical
	ev.Move.San = san
	ev.Move.Position = pos
	ev.Move.Clocks = []events.Clock{clockNext}
	ev.Move.ReceivedAt = p.ReceivedAt
	m.bus.Publish(ev)

	// Reveal scheduler, move branch (spec §8.2): delayed commentary whose
	// ply bound (or already-passed time bound) is satisfied by this
	// acceptance reveals now. A game-ending move goes through afterFinish's
	// reveal-all instead.
	if result == nil {
		if err := m.RevealPlyPass(ctx, g.URI, p.Ply); err != nil {
			m.logger.Error("games: reveal ply pass failed", "game", g.URI, "err", err)
		}
	}

	if result != nil {
		m.afterFinish(ctx, g, st2, result)
	}

	return acc, nil
}

// engineError maps engine validation failures to manager errors.
func engineError(err error) *Error {
	var malformed *engine.MalformedError
	if errors.As(err, &malformed) {
		return gameError(CodeMalformedPayload, "%s", malformed.Detail)
	}
	var illegal *engine.IllegalError
	if errors.As(err, &illegal) {
		return gameError(CodeIllegalMove, "%s", illegal.Detail)
	}
	return gameError(CodeInvalidRequest, "%v", err)
}

// timeoutResult is the timeout outcome: the on-move player loses (spec §7).
func (m *Manager) timeoutResult(g *repo.Game, players []GamePlayer) *events.Result {
	turn := deref(g.TurnDID)
	winner := ""
	for _, pl := range players {
		if pl.DID != turn {
			winner = pl.DID
		}
	}
	return &events.Result{Outcome: "win", Winner: winner, Reason: "timeout"}
}

// finishTimeout finishes an expired or conceded game: DB row (conditional
// on still-active, so concurrent finishers lose cleanly), repo record,
// reveal hook, #gameFinished (spec §7).
func (m *Manager) finishTimeout(ctx context.Context, g *repo.Game, st engine.State, result *events.Result, finishedAt time.Time) error {
	resJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	g.Status = "finished"
	g.Result = resJSON
	g.FinishedAt = &finishedAt
	tag, err := m.pool.Exec(ctx,
		`UPDATE games SET status='finished', result=$2, finished_at=$3
		 WHERE uri=$1 AND status='active'`, g.URI, resJSON, finishedAt)
	if err != nil {
		return fmt.Errorf("games: finish game: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return gameError(CodeGameNotActive, "game already finished")
	}
	m.afterFinish(ctx, g, st, result)
	return nil
}

// afterFinish performs the post-commit side effects of a game ending: the
// service-repo record update (status finished + result + finalPosition,
// spec §2.2 step 7), the reveal-all hook (Phase F), the finish observers
// (Phase D no-show detection), and the #gameFinished event.
func (m *Manager) afterFinish(ctx context.Context, g *repo.Game, st engine.State, result *events.Result) {
	var finalPos engine.Position
	if eng, err := m.engines.Get(g.GameType); err == nil && st != nil {
		finalPos, _ = eng.Position(st)
	}
	rec := m.gameRecord(g, nil, result, finalPos)
	if _, err := m.writer.UpdateGameRecord(ctx, rkeyOf(g.URI), rec); err != nil {
		if !errors.Is(err, servicerepo.ErrNotConfigured) {
			m.logger.Error("games: update game record", "game", g.URI, "err", err)
		}
	}
	m.revealAll(ctx, g.URI)
	if m.finishHook != nil {
		players, err := decodePlayers(g.Players)
		if err == nil {
			m.finishHook(FinishInfo{
				GameURI:  g.URI,
				GameType: g.GameType,
				Variant:  deref(g.Variant),
				Players:  players,
				Ply:      g.Ply,
				TurnDID:  deref(g.TurnDID),
				Matched:  len(g.Matchmaking) > 0,
				Result:   result,
			})
		} else {
			m.logger.Error("games: decode players for finish hook", "game", g.URI, "err", err)
		}
	}
	m.bus.Publish(events.Event{Kind: events.KindGameFinished, GameURI: g.URI, Result: result})
}

// revealAll is the game-end reveal pass (spec §8.2, §8.3 policy 3: nothing
// stays private): decrypt every still-hidden escrowed commentary (delayed
// not yet revealed + sealed), push #commentaryRevealed for each, and write
// bot.plays.bot.game.reveal with every key the server holds unwrapped.
//
// Records that can never be revealed (escrowFailed, sealed without an
// escrowKey, decryptFailed) stay unrevealed with their escrow_status —
// getState surfaces them as never-revealed entries without text. The
// reveal record carries only recoverable keys; a game whose keys are all
// unrecoverable writes no record.
//
// Reason mapping (judgment call, documented): result reason timeout or
// abandonment → "abandonment" (the game did not reach a natural end, so
// the reveal is an abandonment cleanup); adjudication → "adjudication";
// everything else → "gameEnd".
func (m *Manager) revealAll(ctx context.Context, gameURI string) {
	g, err := m.repos.Games.Get(ctx, gameURI)
	if err != nil {
		m.logger.Error("games: reveal-all: game lookup failed", "game", gameURI, "err", err)
		return
	}
	now := m.now()

	// 1. Reveal everything decryptable that is still hidden.
	hidden, err := m.repos.Commentary.ListEscrowedByGame(ctx, gameURI)
	if err != nil {
		m.logger.Error("games: reveal-all: commentary lookup failed", "game", gameURI, "err", err)
	}
	for i := range hidden {
		m.revealOne(ctx, hidden[i], now)
	}

	// 2. Collect the reveal record's keys: every (player, keyId) the server
	// holds an unwrapped content key for, with its publication marks.
	rows, err := m.repos.Commentary.ListByGame(ctx, gameURI)
	if err != nil {
		m.logger.Error("games: reveal-all: key collection failed", "game", gameURI, "err", err)
		return
	}
	type keyID struct{ player, keyID string }
	agg := map[keyID]*playsbot.GameReveal_RevealedKey{}
	var order []keyID
	for _, c := range rows {
		if len(c.ContentKey) != escrow.KeySize || c.KeyID == nil || c.PlayerDID == nil {
			continue
		}
		k := keyID{player: *c.PlayerDID, keyID: *c.KeyID}
		entry, ok := agg[k]
		if !ok {
			keyBytes := append([]byte(nil), c.ContentKey...)
			entry = &playsbot.GameReveal_RevealedKey{
				Player: k.player,
				KeyId:  k.keyID,
				Key:    keyBytes,
			}
			agg[k] = entry
			order = append(order, k)
		}
		entry.AgentPublished = gt.Some(entry.AgentPublished.ValOr(false) || c.AgentPublished)
		entry.Mismatch = gt.Some(entry.Mismatch.ValOr(false) || c.KeyMismatch)
	}

	// 3. Write the record. The strongRef needs the game record's CID; when
	// the service account never round-tripped a CID (inert PDS), rows still
	// revealed but no public record exists.
	if len(order) == 0 || g.CID == nil {
		if len(order) > 0 {
			m.logger.Warn("games: reveal-all: no game CID; reveal record skipped",
				"game", gameURI, "keys", len(order))
		}
		return
	}
	ref := comatproto.RepoStrongRef{URI: gameURI, CID: *g.CID}
	var result events.Result
	if len(g.Result) > 0 {
		_ = json.Unmarshal(g.Result, &result)
	}
	rec := &playsbot.GameReveal{
		Game:      ref,
		Reason:    revealReason(&result),
		CreatedAt: RFC3339Millis(now),
	}
	for _, k := range order {
		rec.Keys = append(rec.Keys, *agg[k])
	}
	if _, err := m.writer.WriteRevealRecord(ctx, rec); err != nil {
		if !errors.Is(err, servicerepo.ErrNotConfigured) {
			m.logger.Error("games: reveal record write failed", "game", gameURI, "err", err)
		}
	}
}

// revealReason maps a finished game's result reason to the reveal record's
// reason (spec §4.7 knownValues; see revealAll for the mapping rationale).
func revealReason(result *events.Result) string {
	if result != nil {
		switch result.Reason {
		case "timeout", "abandonment":
			return "abandonment"
		case "adjudication":
			return "adjudication"
		}
	}
	return "gameEnd"
}

// ---------------------------------------------------------------------------
// Reveal scheduler (spec §8.2, §8.4 step 4)

// StartRevealScheduler launches the wall-clock reveal ticker (spec: 1s).
// Safe to call once; stop via Close.
func (m *Manager) StartRevealScheduler(interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	m.revealStop = make(chan struct{})
	m.revealDone = make(chan struct{})
	go func() {
		defer close(m.revealDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.revealStop:
				return
			case <-ticker.C:
				if err := m.RevealTimerPass(context.Background()); err != nil {
					m.logger.Error("games: reveal timer pass failed", "err", err)
				}
			}
		}
	}()
}

// RevealPlyPass reveals every delayed commentary of one game whose reveal
// bound (ply N+P or wall-clock) is satisfied at currentPly. Called on each
// accepted move — "whichever comes first" (spec §8.2).
func (m *Manager) RevealPlyPass(ctx context.Context, gameURI string, currentPly int64) error {
	due, err := m.repos.Commentary.ListRevealableByGame(ctx, gameURI, currentPly, m.now(), 200)
	if err != nil {
		return err
	}
	now := m.now()
	for i := range due {
		m.revealOne(ctx, due[i], now)
	}
	return nil
}

// RevealTimerPass reveals every delayed commentary whose wall-clock bound
// (ingest receivedAt + S) has passed, across all games. The ticker branch
// keeps stalled games broadcasting (spec §8.2 rationale).
func (m *Manager) RevealTimerPass(ctx context.Context) error {
	now := m.now()
	due, err := m.repos.Commentary.ListDue(ctx, now, 200)
	if err != nil {
		return err
	}
	for i := range due {
		m.revealOne(ctx, due[i], now)
	}
	return nil
}

// revealOne decrypts and reveals one escrowed commentary row: content key
// + AAD check (spec §8.1), text + revealed_at persisted, #commentaryRevealed
// pushed. Undecryptable content downgrades the row to decryptFailed so the
// scheduler stops retrying it (it stays visible as never-revealed).
func (m *Manager) revealOne(ctx context.Context, c repo.Commentary, now time.Time) {
	player := deref(c.PlayerDID)
	ply := int64(0)
	if c.Ply != nil {
		ply = int64(*c.Ply)
	}
	if len(c.ContentKey) != escrow.KeySize || len(c.Nonce) != escrow.NonceSize {
		m.failReveal(ctx, c, "stored key material is unusable")
		return
	}
	var key escrow.ContentKey
	copy(key[:], c.ContentKey)
	var nonce [escrow.NonceSize]byte
	copy(nonce[:], c.Nonce)
	text, err := escrow.Decrypt(key, nonce, c.Ciphertext, escrow.AAD(c.GameURI, ply, player))
	if err != nil {
		m.failReveal(ctx, c, "decryption failed (key or AAD mismatch)")
		return
	}
	if err := m.repos.Commentary.RevealWithText(ctx, c.URI, string(text), now); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return // revealed concurrently; skip the duplicate event
		}
		m.logger.Error("games: reveal persist failed", "uri", c.URI, "err", err)
		return
	}
	ev := events.Event{Kind: events.KindCommentaryRevealed, GameURI: c.GameURI}
	ev.CommentaryRevealed.Ply = ply
	ev.CommentaryRevealed.Player = player
	ev.CommentaryRevealed.Text = string(text)
	m.bus.Publish(ev)
}

// failReveal downgrades a row that cannot be decrypted so the scheduler
// stops retrying, and logs (the row stays a never-revealed entry).
func (m *Manager) failReveal(ctx context.Context, c repo.Commentary, why string) {
	if err := m.repos.Commentary.SetEscrowStatus(ctx, c.URI, repo.EscrowDecryptFailed); err != nil {
		m.logger.Error("games: decryptFailed mark failed", "uri", c.URI, "err", err)
	}
	m.logger.Warn("games: commentary never revealed", "uri", c.URI, "game", c.GameURI, "why", why)
}

// ---------------------------------------------------------------------------
// Resign and draws (spec §5.6)

// Resign finishes the game immediately with the resigning player's loss.
func (m *Manager) Resign(ctx context.Context, gameURI, playerDID string) (*events.Result, error) {
	g, _, st, players, err := m.loadActivePlayer(ctx, gameURI, playerDID)
	if err != nil {
		return nil, err
	}
	winner := ""
	for _, pl := range players {
		if pl.DID != playerDID {
			winner = pl.DID
		}
	}
	result := &events.Result{Outcome: "win", Winner: winner, Reason: "resignation"}
	if err := m.finishTimeout(ctx, g, st, result, m.now()); err != nil {
		return nil, err
	}
	return result, nil
}

// OfferDraw records a pending draw offer (spec §5.6). A new offer replaces
// any outstanding one.
func (m *Manager) OfferDraw(ctx context.Context, gameURI, playerDID string) error {
	g, _, _, _, err := m.loadActivePlayer(ctx, gameURI, playerDID)
	if err != nil {
		return err
	}
	ply := g.Ply
	g.DrawOfferDID = &playerDID
	g.DrawOfferPly = &ply
	if err := m.repos.Games.Update(ctx, g); err != nil {
		return fmt.Errorf("games: save draw offer: %w", err)
	}
	m.bus.Publish(events.Event{Kind: events.KindDrawOffered, GameURI: gameURI, OfferedBy: playerDID})
	return nil
}

// AcceptDraw accepts the opponent's pending offer: agreement draw (spec
// §5.6).
func (m *Manager) AcceptDraw(ctx context.Context, gameURI, playerDID string) (*events.Result, error) {
	g, _, st, _, err := m.loadActivePlayer(ctx, gameURI, playerDID)
	if err != nil {
		return nil, err
	}
	if g.DrawOfferDID == nil || *g.DrawOfferDID == playerDID {
		return nil, gameError(CodeInvalidRequest, "no pending draw offer to accept")
	}
	result := &events.Result{Outcome: "draw", Reason: "agreement"}
	if err := m.finishTimeout(ctx, g, st, result, m.now()); err != nil {
		return nil, err
	}
	return result, nil
}

// DeclineDraw rejects a pending offer (the offerer declining is a
// withdraw). No event: the spec's stream has no drawDeclined kind.
func (m *Manager) DeclineDraw(ctx context.Context, gameURI, playerDID string) error {
	g, _, _, _, err := m.loadActivePlayer(ctx, gameURI, playerDID)
	if err != nil {
		return err
	}
	if g.DrawOfferDID == nil {
		return gameError(CodeInvalidRequest, "no pending draw offer")
	}
	g.DrawOfferDID = nil
	g.DrawOfferPly = nil
	if err := m.repos.Games.Update(ctx, g); err != nil {
		return fmt.Errorf("games: clear draw offer: %w", err)
	}
	return nil
}

// loadActivePlayer fetches the game, asserts it is active and the DID is a
// player, and loads engine state + players.
func (m *Manager) loadActivePlayer(ctx context.Context, gameURI, playerDID string) (*repo.Game, engine.GameEngine, engine.State, []GamePlayer, error) {
	g, err := m.repos.Games.Get(ctx, gameURI)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil, nil, nil, gameError(CodeNotFound, "no such game")
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if g.Status != "active" {
		return nil, nil, nil, nil, gameError(CodeGameNotActive, "game is %s", g.Status)
	}
	players, err := decodePlayers(g.Players)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := seatOf(players, playerDID); err != nil {
		return nil, nil, nil, nil, gameError(CodeForbidden, "caller is not a player in this game")
	}
	eng, st, _, _, err := m.loadGame(ctx, g)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return g, eng, st, players, nil
}

// ---------------------------------------------------------------------------
// State queries

// StateQuery threads the requester/audience context through state assembly
// (spec §5.2). Chess is perfect information — every audience sees the same
// projection — but hidden-info engines project per-seat visible state via
// the RequesterDID in a later phase; the parameterization exists from day
// one.
type StateQuery struct {
	GameURI string
	// RequesterDID is the authenticated caller, "" when anonymous.
	RequesterDID string
}

// GetState assembles the authoritative state (spec §5.2): envelope,
// position, ply, turn, clocks, legal moves, history, commentary summaries,
// server time.
func (m *Manager) GetState(ctx context.Context, q StateQuery) (*GameView, error) {
	g, err := m.repos.Games.Get(ctx, q.GameURI)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, gameError(CodeNotFound, "no such game")
	}
	if err != nil {
		return nil, err
	}
	eng, st, rows, _, err := m.loadGame(ctx, g)
	if err != nil {
		return nil, err
	}
	view, err := m.assemble(ctx, g, eng, st, m.now())
	if err != nil {
		return nil, err
	}
	view.History = moveEntries(rows)
	commentary, err := m.repos.Commentary.ListByGame(ctx, g.URI)
	if err != nil {
		return nil, err
	}
	view.Commentary = buildCommentaryEntries(commentary)
	return view, nil
}

// ListParams filters listGames (spec §5.3).
type ListParams struct {
	Status   string
	GameType string
	Player   string
	Limit    int
	Cursor   string
}

// ListGames returns matching game views (envelope + position + ply), plus
// the next cursor when more remain.
func (m *Manager) ListGames(ctx context.Context, p ListParams) ([]*GameView, string, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := m.repos.Games.ListFiltered(ctx, repo.GamesFilter{
		Status:    p.Status,
		GameType:  p.GameType,
		Player:    p.Player,
		BeforeURI: p.Cursor,
		Limit:     limit,
	})
	if err != nil {
		return nil, "", err
	}
	views := make([]*GameView, 0, len(rows))
	for _, g := range rows {
		eng, st, _, _, err := m.loadGame(ctx, g)
		if err != nil {
			return nil, "", err
		}
		view, err := m.assemble(ctx, g, eng, st, m.now())
		if err != nil {
			return nil, "", err
		}
		views = append(views, view)
	}
	var cursor string
	if len(rows) == limit {
		cursor = rows[len(rows)-1].URI
	}
	return views, cursor, nil
}

// ---------------------------------------------------------------------------
// internal helpers

// loadGame loads the engine and rehydrates state: correctness-critical
// paths replay the stored move payloads from the moves table (the engine is
// pure, so replay is deterministic); the persisted games.state snapshot is
// the quick-load cache for display paths.
func (m *Manager) loadGame(ctx context.Context, g *repo.Game) (engine.GameEngine, engine.State, []repo.Move, []GamePlayer, error) {
	eng, err := m.engines.Get(g.GameType)
	if err != nil {
		return nil, nil, nil, nil, gameError(CodeInvalidRequest, "%v", err)
	}
	variant := deref(g.Variant)
	rows, err := m.repos.Moves.ListByGame(ctx, g.URI)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	payloads := make([]engine.Payload, 0, len(rows))
	for i := range rows {
		payloads = append(payloads, engine.Payload(rows[i].Payload))
	}
	var st engine.State
	switch {
	case len(payloads) > 0:
		st, err = engine.Replay(eng, variant, g.Seed, payloads)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("games: replay %s: %w", g.URI, err)
		}
	case len(g.State) > 0:
		snap, ok := eng.(engine.Snapshotter)
		if ok {
			st, err = snap.Restore(g.State)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("games: restore %s: %w", g.URI, err)
			}
		}
	default:
		st, err = eng.InitialState(variant, g.Seed)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	players, err := decodePlayers(g.Players)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return eng, st, rows, players, nil
}

// assemble builds a GameView from the row and engine state (without
// history; getState attaches moveEntries(rows)).
func (m *Manager) assemble(ctx context.Context, g *repo.Game, eng engine.GameEngine, st engine.State, now time.Time) (*GameView, error) {
	players, err := decodePlayers(g.Players)
	if err != nil {
		return nil, err
	}
	view := &GameView{
		URI:          g.URI,
		GameType:     g.GameType,
		Variant:      g.Variant,
		Status:       g.Status,
		Players:      players,
		Ply:          int64(g.Ply),
		TurnDID:      deref(g.TurnDID),
		CreatedAt:    g.CreatedAt,
		StartedAt:    g.StartedAt,
		FinishedAt:   g.FinishedAt,
		Seed:         g.Seed,
		ServerTime:   now,
		DrawOfferDID: deref(g.DrawOfferDID),
	}
	if g.CID != nil {
		cid := *g.CID
		view.CID = &cid
	}
	view.Rkey = rkeyOf(g.URI)
	if tc, err := decodeTimeControl(g.TimeControl); err == nil {
		view.TimeControl = tc
	}
	if len(g.CommentaryDelay) > 0 {
		var d config.CommentaryDelay
		if err := json.Unmarshal(g.CommentaryDelay, &d); err == nil {
			view.CommentaryDelay = &d
		}
	}
	if len(g.Result) > 0 {
		var r events.Result
		if err := json.Unmarshal(g.Result, &r); err == nil {
			view.Result = &r
		}
	}

	pos, err := eng.Position(st)
	if err != nil {
		return nil, err
	}
	view.Position = pos

	if g.Status == "active" {
		if moves, err := eng.LegalMoves(st); err == nil {
			view.LegalMoves = moves
		}
		if anchor := g.ClockAnchor; anchor != nil {
			if c, _ := m.clockFor(view.TimeControl, anchor, view.TurnDID, now); c.DID != "" || c.RemainingMs > 0 {
				view.Clocks = []events.Clock{c}
			}
		}
	}
	return view, nil
}

// moveEntries converts stored move rows into history entries.
func moveEntries(rows []repo.Move) []MoveEntry {
	out := make([]MoveEntry, 0, len(rows))
	for i := range rows {
		out = append(out, MoveEntry{
			Ply:              int64(rows[i].Ply),
			Player:           deref(rows[i].PlayerDID),
			Payload:          rows[i].Payload,
			ReceivedAt:       rows[i].ReceivedAt,
			SAN:              deref(rows[i].Notation),
			ClockRemainingMs: rows[i].ClockRemainingMs,
			Token:            deref(rows[i].Token),
		})
	}
	return out
}

// clockFor computes the on-move clock snapshot for events and views.
func (m *Manager) clockFor(tc clock.TimeControl, anchor *time.Time, did string, now time.Time) (events.Clock, time.Time) {
	if anchor == nil {
		return events.Clock{}, time.Time{}
	}
	deadline, err := clock.DeadlineFor(tc, *anchor)
	if err != nil {
		return events.Clock{}, time.Time{}
	}
	remaining, err := clock.RemainingMs(tc, *anchor, now)
	if err != nil {
		return events.Clock{}, time.Time{}
	}
	return events.Clock{
		DID:         did,
		RemainingMs: remaining,
		Deadline:    RFC3339Millis(deadline),
	}, deadline
}

// snapshot serializes engine state via Snapshotter.
func (m *Manager) snapshot(eng engine.GameEngine, st engine.State) (json.RawMessage, error) {
	snap, ok := eng.(engine.Snapshotter)
	if !ok {
		return nil, nil // engines without snapshots rely on replay alone
	}
	raw, err := snap.Snapshot(st)
	if err != nil {
		return nil, fmt.Errorf("games: snapshot: %w", err)
	}
	return raw, nil
}

func (m *Manager) serviceDID() string {
	if m.cfg != nil && m.cfg.ServiceDID != "" {
		return m.cfg.ServiceDID
	}
	if m.writer != nil {
		return m.writer.DID()
	}
	return ""
}
