package indexer

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// Stats are the indexer's ingest counters (observability + tests).
type Stats struct {
	// EventsHandled counts RepoEvents that reached the pipeline.
	EventsHandled atomic.Uint64
	// IgnoredForeign counts records dropped by the §4.2 ignore rule
	// (verdict records outside the service repo) and out-of-band
	// service-DID agent records.
	IgnoredForeign atomic.Uint64
	// MovesLinked counts move rows linked to a verified repo record.
	MovesLinked atomic.Uint64
	// UnverifiedMoves counts move records rejected by §4.5 validation.
	UnverifiedMoves atomic.Uint64
	// MissingMoves counts missingMoveRecord assertions emitted by the
	// missing-record sweep (§10).
	MissingMoves atomic.Uint64
	// CommentaryIndexed counts commentary records stored (any escrow
	// status; spec §8.4).
	CommentaryIndexed atomic.Uint64
	// KeyMismatches counts keyMismatch flags raised by publication
	// detection (§10).
	KeyMismatches atomic.Uint64
}

// snapshot copies the counters under a mutex-free consistent-enough view
// (individual counters are atomics; the tuple is not a transaction).
type StatsSnapshot struct {
	EventsHandled     uint64
	IgnoredForeign    uint64
	MovesLinked       uint64
	UnverifiedMoves   uint64
	MissingMoves      uint64
	CommentaryIndexed uint64
	KeyMismatches     uint64
}

// IdentityCache maps DIDs to handles, updated from #identity events on
// either transport. It feeds actors.handle on profile ingest and the §5.8
// operator verification of DID-typed operator fields. DID→PDS resolution
// is not needed by the v1 ingest rules, so only handles are cached.
type IdentityCache struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewIdentityCache returns an empty cache.
func NewIdentityCache() *IdentityCache { return &IdentityCache{m: map[string]string{}} }

// Set records a DID→handle binding.
func (c *IdentityCache) Set(did, handle string) {
	if did == "" || handle == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[did] = handle
}

// Handle returns the cached handle for did.
func (c *IdentityCache) Handle(did string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.m[did]
	return h, ok
}

// Ingestor applies the plays.bot ingest rules to normalized RepoEvents
// (spec §4.2 design rules, §4.5 move validation, §9a.5 challenge records,
// §4.3 profiles). It is transport-agnostic: both the firehose and the
// Jetstream source feed the same Handle, which is what the contract test
// asserts.
type Ingestor struct {
	cfg     *config.Config
	repos   *repo.Pool
	pub     ed25519.PublicKey
	emitter *FlagEmitter
	identity *IdentityCache
	log     *slog.Logger
	now     func() time.Time

	// escrow resolves AppView escrow rotations for commentary unwrap
	// (spec §8.4 step 2). Wired via SetEscrowDirectory; nil disables
	// unwrapping (every escrowKey then indexes escrowFailed).
	escrow keys.EscrowKeyDirectory
	// bus carries #commentaryPosted events on ingest (spec §5.4). Wired
	// via SetBus; nil skips publication.
	bus *events.Bus

	stats Stats
}

// NewIngestor assembles the ingest pipeline. pub is the service Ed25519
// public key (moveToken verification, spec Appendix B).
func NewIngestor(cfg *config.Config, repos *repo.Pool, pub ed25519.PublicKey, emitter *FlagEmitter, logger *slog.Logger) *Ingestor {
	return &Ingestor{
		cfg:      cfg,
		repos:    repos,
		pub:      pub,
		emitter:  emitter,
		identity: NewIdentityCache(),
		log:      logger,
		now:      time.Now,
	}
}

// Identities exposes the DID→handle cache (operator verification of
// DID-typed operator fields; tests seed it).
func (in *Ingestor) Identities() *IdentityCache { return in.identity }

// SnapshotStats reads the ingest counters.
func (in *Ingestor) SnapshotStats() StatsSnapshot {
	return StatsSnapshot{
		EventsHandled:     in.stats.EventsHandled.Load(),
		IgnoredForeign:    in.stats.IgnoredForeign.Load(),
		MovesLinked:       in.stats.MovesLinked.Load(),
		UnverifiedMoves:   in.stats.UnverifiedMoves.Load(),
		MissingMoves:      in.stats.MissingMoves.Load(),
		CommentaryIndexed: in.stats.CommentaryIndexed.Load(),
		KeyMismatches:     in.stats.KeyMismatches.Load(),
	}
}

// SetEscrowDirectory wires the escrow key directory (commentary unwrap,
// spec §8.4). Call before any commentary event is processed.
func (in *Ingestor) SetEscrowDirectory(dir keys.EscrowKeyDirectory) { in.escrow = dir }

// SetBus wires the event bus for #commentaryPosted emission (spec §5.4).
func (in *Ingestor) SetBus(bus *events.Bus) { in.bus = bus }

// Handle applies the ingest rules to one event. Never fails: problems are
// counted and logged (ingest must never take down the stream loop).
func (in *Ingestor) Handle(ev RepoEvent) {
	in.HandleCtx(context.Background(), ev)
}

// RepoEvent implements Handler (Source.Run dispatches here).
func (in *Ingestor) RepoEvent(ev RepoEvent) { in.Handle(ev) }

// Identity implements Handler: it records DID→handle bindings from
// #identity events (feeds actors.handle and §5.8 operator resolution).
func (in *Ingestor) Identity(did, handle string) { in.identity.Set(did, handle) }

// HandleCtx is Handle with an explicit context (tests).
func (in *Ingestor) HandleCtx(ctx context.Context, ev RepoEvent) {
	in.stats.EventsHandled.Add(1)

	if !strings.HasPrefix(ev.Collection, NamespacePrefix) {
		return
	}

	switch ev.Collection {
	// Verdict records (spec §4.2: written only by the AppView service
	// DID; anything else is ignored). Service-authored rows are already
	// indexed at write time — except the game record CID, which exists
	// only after the PDS write completes and reaches us here via the
	// stream (Appendix A games.cid).
	case playsbot.NSIDBotGame, playsbot.NSIDGameReveal, playsbot.NSIDBotRating, playsbot.NSIDBotFlag:
		if ev.DID != in.cfg.ServiceDID {
			in.stats.IgnoredForeign.Add(1)
			in.log.Debug("indexer: ignoring foreign verdict record (spec §4.2)",
				"collection", ev.Collection, "did", ev.DID, "rkey", ev.Rkey)
			return
		}
		if ev.Collection == playsbot.NSIDBotGame && ev.Kind != KindDelete && ev.CID != "" {
			if err := in.repos.Games.SetCIDIfUnset(ctx, gameURIFromServiceRecord(ev), ev.CID); err != nil {
				in.log.Debug("indexer: game cid backfill skipped", "rkey", ev.Rkey, "err", err)
			}
		}
		in.log.Debug("indexer: service verdict record processed",
			"collection", ev.Collection, "rkey", ev.Rkey)
		return

	case playsbot.NSIDGameMove:
		in.ingestMove(ctx, ev)
	case playsbot.NSIDGameChallenge:
		in.ingestChallenge(ctx, ev)
	case playsbot.NSIDActorProfile:
		in.ingestProfile(ctx, ev)

	case playsbot.NSIDGameCommentary:
		in.ingestCommentary(ctx, ev)

	default:
		in.log.Debug("indexer: no rule for collection", "collection", ev.Collection)
	}
}

// gameURIFromServiceRecord rebuilds the at:// URI of a service-authored
// game record from the event (the service repo is the only legitimate
// home of bot.plays.bot.game records, spec §4.2).
func gameURIFromServiceRecord(ev RepoEvent) string {
	return fmt.Sprintf("at://%s/%s/%s", ev.DID, playsbot.NSIDBotGame, ev.Rkey)
}

// ---------------------------------------------------------------------------
// game.move (spec §4.5 indexer validation, §2.2 step 6)

func (in *Ingestor) ingestMove(ctx context.Context, ev RepoEvent) {
	if ev.Kind == KindDelete {
		// The agent withdrew their move record. The AppView state is
		// authoritative and unaffected; the linked row keeps its last
		// verified state (spec §1.3 principle 3). Phase F may add an
		// explicit flag for retractions.
		in.log.Debug("indexer: move record deleted; link retained", "uri", ev.repoURI())
		return
	}
	if in.cfg.ServiceDID != "" && ev.DID == in.cfg.ServiceDID {
		// The service repo never holds move records; treat as foreign
		// noise rather than a verification candidate.
		in.stats.IgnoredForeign.Add(1)
		return
	}

	var rec playsbot.GameMove
	if err := json.Unmarshal(ev.Record, &rec); err != nil {
		in.log.Debug("indexer: undecodable move record", "uri", ev.repoURI(), "err", err)
		return
	}
	if rec.Game.URI == "" || rec.Ply < 1 {
		in.log.Debug("indexer: move record missing game strongRef or ply", "uri", ev.repoURI())
		return
	}

	// The link target is the *accepted* move row (spec §2.2 step 6: the
	// indexer links the repo record to the accepted move). No row →
	// index nothing.
	mv, err := in.repos.Moves.Get(ctx, rec.Game.URI, int(rec.Ply))
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			in.log.Debug("indexer: no accepted move for repo record",
				"game", rec.Game.URI, "ply", rec.Ply, "did", ev.DID)
			return
		}
		in.log.Error("indexer: move lookup failed", "game", rec.Game.URI, "ply", rec.Ply, "err", err)
		return
	}

	verified, why := in.verifyMove(mv, ev.DID, &rec)
	if err := in.repos.Moves.LinkRepoRecord(ctx, rec.Game.URI, int(rec.Ply), ev.repoURI(), ev.CID, verified); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			in.log.Debug("indexer: accepted move vanished before linking", "game", rec.Game.URI, "ply", rec.Ply)
			return
		}
		in.log.Error("indexer: move link failed", "game", rec.Game.URI, "ply", rec.Ply, "err", err)
		return
	}

	if verified {
		in.stats.MovesLinked.Add(1)
		in.log.Debug("indexer: move record linked", "game", rec.Game.URI, "ply", rec.Ply, "did", ev.DID)
		return
	}

	in.stats.UnverifiedMoves.Add(1)
	in.log.Warn("indexer: unverified move record",
		"game", rec.Game.URI, "ply", rec.Ply, "did", ev.DID, "why", why)
	if in.emitter != nil {
		detail := fmt.Sprintf("repo move record %s rejected: %s", ev.repoURI(), why)
		if _, err := in.emitter.EmitFlag(ctx, ev.DID, &rec.Game.URI, FlagUnverifiedMoveRecord, SeverityWarning, detail); err != nil {
			in.log.Error("indexer: unverifiedMoveRecord flag failed", "game", rec.Game.URI, "err", err)
		}
	}
}

// verifyMove implements spec §4.5: token signature valid, token fields
// match the record (game strongRef URI, ply, payload incl. $type,
// receivedAt == token rat), record repo DID matches the token subject, and
// the record was produced from this exact acceptance (row token equality).
func (in *Ingestor) verifyMove(mv *repo.Move, repoDID string, rec *playsbot.GameMove) (bool, string) {
	claims, err := games.VerifyMoveToken(in.pub, rec.MoveToken)
	if err != nil {
		return false, "moveToken signature invalid"
	}
	if claims.Game != rec.Game.URI {
		return false, fmt.Sprintf("token game %q != record game %q", claims.Game, rec.Game.URI)
	}
	if claims.Ply != rec.Ply {
		return false, fmt.Sprintf("token ply %d != record ply %d", claims.Ply, rec.Ply)
	}
	if claims.Subject != repoDID {
		return false, fmt.Sprintf("token subject %q != repo did %q", claims.Subject, repoDID)
	}
	if claims.ReceivedAt != rec.ReceivedAt {
		return false, fmt.Sprintf("token receivedAt %q != record receivedAt %q", claims.ReceivedAt, rec.ReceivedAt)
	}
	recPayload := marshalJSON(rec.Payload)
	eq, err := payloadsEqualJSON(claims.Payload, recPayload)
	if err != nil || !eq {
		return false, "token payload != record payload"
	}
	// Defense in depth: the record must be the artifact of *this*
	// acceptance, not a verbatim replay of some other ply's token that
	// happens to name the same game/ply.
	if mv.Token == nil || *mv.Token != rec.MoveToken {
		return false, "record moveToken does not match the accepted move"
	}
	if mv.PlayerDID == nil || *mv.PlayerDID != repoDID {
		return false, "accepted move belongs to another player"
	}
	return true, ""
}

// payloadsEqualJSON compares two JSON objects semantically (key order and
// whitespace insensitive), including the $type discriminator.
func payloadsEqualJSON(a, b json.RawMessage) (bool, error) {
	ca, err := engine.CanonicalJSON(a)
	if err != nil {
		return false, err
	}
	cb, err := engine.CanonicalJSON(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

// ---------------------------------------------------------------------------
// game.challenge (spec §4.8, §9a.5)

func (in *Ingestor) ingestChallenge(ctx context.Context, ev RepoEvent) {
	if ev.Kind == KindDelete {
		// A withdrawn challenge: cancel it if it is still live (the repo
		// record is the challenger's voice, spec §9a.5). Terminal
		// statuses stay untouched.
		if _, err := in.repos.Challenges.TransitionStatus(ctx, ev.Rkey,
			[]string{repo.ChallengeOpen, repo.ChallengePending}, repo.ChallengeCancelled); err != nil {
			in.log.Debug("indexer: challenge delete handling", "id", ev.Rkey, "err", err)
		}
		return
	}

	var rec playsbot.GameChallenge
	if err := json.Unmarshal(ev.Record, &rec); err != nil {
		in.log.Debug("indexer: undecodable challenge record", "uri", ev.repoURI(), "err", err)
		return
	}

	// Out-of-band: the AppView does not issue challenges from its own
	// repo (spec §4.8 says agents write these).
	if in.cfg.ServiceDID != "" && ev.DID == in.cfg.ServiceDID {
		in.stats.IgnoredForeign.Add(1)
		return
	}

	status := repo.ChallengeOpen
	var opponent *string
	if rec.Opponent.HasVal() && rec.Opponent.Val() != "" {
		opponentVal := rec.Opponent.Val()
		opponent = &opponentVal
		status = repo.ChallengePending
	}

	tc, err := timeControlFromRecord(rec.TimeControl)
	if err != nil {
		in.log.Debug("indexer: challenge record has unusable time control", "uri", ev.repoURI(), "err", err)
		return
	}

	var delayJSON json.RawMessage
	if rec.CommentaryDelay.HasVal() {
		delayJSON = marshalJSON(rec.CommentaryDelay.Val())
	}

	expiresAt := in.now().Add(in.cfg.Tunables.ChallengeTTL)
	if rec.ExpiresAt.HasVal() {
		if parsed, perr := time.Parse(time.RFC3339, rec.ExpiresAt.Val()); perr == nil {
			expiresAt = parsed
		}
	}

	seatPref := rec.SeatPreference.ValOr("")
	if seatPref == "" {
		seatPref = "random"
	}

	var variant *string
	if rec.Variant.HasVal() && rec.Variant.Val() != "" {
		v := rec.Variant.Val()
		variant = &v
	}

	uri := ev.repoURI()
	cid := ev.CID
	if err := in.repos.Challenges.UpsertIndexed(ctx, &repo.Challenge{
		ID:              ev.Rkey,
		ChallengerDID:   ev.DID,
		OpponentDID:     opponent,
		GameType:        rec.GameType,
		Variant:         variant,
		TimeControl:     marshalJSON(tc),
		CommentaryDelay: delayJSON,
		SeatPreference:  seatPref,
		Rated:           rec.Rated.ValOr(true), // spec §4.8 default true
		Status:          status,
		ExpiresAt:       &expiresAt,
		RepoURI:         &uri,
		RepoCID:         &cid,
	}); err != nil {
		in.log.Error("indexer: challenge upsert failed", "uri", ev.repoURI(), "err", err)
		return
	}
	in.log.Debug("indexer: challenge record indexed",
		"id", ev.Rkey, "challenger", ev.DID, "status", status)
}

// timeControlFromRecord converts the record's timeControl object to the
// storage shape; v1 accepts perMove (spec §7).
func timeControlFromRecord(tc playsbot.BotGame_TimeControl) (clock.TimeControl, error) {
	out := clock.TimeControl{Kind: clock.Kind(tc.Kind)}
	if tc.PerMoveSeconds.HasVal() {
		out.PerMoveSeconds = tc.PerMoveSeconds.Val()
	}
	if tc.InitialSeconds.HasVal() {
		out.InitialSeconds = tc.InitialSeconds.Val()
	}
	if tc.IncrementSeconds.HasVal() {
		out.IncrementSeconds = tc.IncrementSeconds.Val()
	}
	if _, err := clock.DeadlineFor(out, time.Now()); err != nil {
		return out, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// actor.profile (spec §4.3, §5.8)

func (in *Ingestor) ingestProfile(ctx context.Context, ev RepoEvent) {
	if ev.Kind == KindDelete {
		// Profiles are literal:self — a delete without a following create
		// is an account wipe. v1 retains the last indexed profile (the
		// AppView state is a cache of self-declared data, spec §1.3).
		in.log.Debug("indexer: profile deleted; last indexed retained", "did", ev.DID)
		return
	}

	var rec playsbot.ActorProfile
	if err := json.Unmarshal(ev.Record, &rec); err != nil {
		in.log.Debug("indexer: undecodable profile record", "uri", ev.repoURI(), "err", err)
		return
	}

	hash, err := profileHash(rec.Model, rec.Harness)
	if err != nil {
		in.log.Warn("indexer: profile hash failed", "did", ev.DID, "err", err)
		return
	}

	revision := int(rec.Revision)
	op := operatorFor(rec.Operator.ValOr(""), in.identity.Handle)

	var handle *string
	if h, ok := in.identity.Handle(ev.DID); ok {
		handle = &h
	}

	if err := in.repos.Actors.Upsert(ctx, &repo.Actor{
		DID:              ev.DID,
		Handle:           handle,
		Profile:          marshalJSON(rec),
		Revision:         &revision,
		ProfileHash:      &hash,
		OperatorDID:      op.OperatorDID,
		OperatorVerified: op.OperatorVerified,
	}); err != nil {
		in.log.Error("indexer: profile upsert failed", "did", ev.DID, "err", err)
		return
	}
	in.log.Debug("indexer: profile indexed",
		"did", ev.DID, "revision", revision,
		"operator", rec.Operator.ValOr(""), "operatorVerified", op.OperatorVerified)
}

// marshalJSON panics-free json.Marshal for record shapes that cannot fail.
func marshalJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
