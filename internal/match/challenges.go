package match

// Challenge lifecycle (spec §9a.1, §9a.2, §5.5): direct and open
// challenges, with the §10 rate limit (one open challenge per DID per game
// type), capability eligibility, the global concurrent-games cap on
// accept, seat preference, and a clock anchored at acceptance.
//
// Repo records: per spec §9a.5 challenges are normally ALSO repo records
// written by the challenger (bot.plays.bot.game.challenge, indexed in
// Phase E). XRPC-created challenges have no strongRef available, so their
// rows carry repo_uri = NULL, and a game accepted from one carries no
// challenge strongRef on its record. When Phase E's indexer starts filling
// repo_uri (+cid), AcceptChallenge will pass the strongRef to CreateGame
// unchanged — the plumbing is already in place.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// ChallengeParams mirrors the game.createChallenge input.
type ChallengeParams struct {
	// Opponent is the challenged DID; empty = open challenge.
	Opponent string
	GameType string
	Variant  string
	// TimeControl must carry a kind (perMove in v1); there is no
	// cross-kind default because the clock is contractual.
	TimeControl clock.TimeControl
	// CommentaryDelay is optional (AppView default applies when nil).
	CommentaryDelay *config.CommentaryDelay
	// SeatPreference: first/second/random ("" = random).
	SeatPreference string
	// Rated defaults to true when set false explicitly by the caller; the
	// XRPC handler normalizes.
	Rated bool
	// ExpiresAt is the absolute expiry (caller-provided or now + TTL,
	// capped at 24h — normalized by the XRPC handler before this point).
	ExpiresAt time.Time
}

// CreateChallenge persists a pending (direct) or open challenge, enforcing
// the §10 limit, and notifies a direct challenge's opponent over
// match.subscribe (#challengeReceived, spec §5.5a).
func (m *Matcher) CreateChallenge(ctx context.Context, challengerDID string, p ChallengeParams) (*repo.Challenge, error) {
	if !m.games.SupportsGameType(p.GameType) {
		return nil, matchError(CodeInvalidRequest, "unknown gameType %q", p.GameType)
	}
	if p.TimeControl.Kind == "" {
		return nil, matchError(CodeInvalidRequest, "timeControl.kind is required")
	}
	if _, err := clock.DeadlineFor(p.TimeControl, m.now()); err != nil {
		return nil, matchError(CodeInvalidRequest, "time control: %v", err)
	}
	pref := p.SeatPreference
	if pref == "" {
		pref = SeatPrefRandom
	}
	if pref != SeatPrefFirst && pref != SeatPrefSecond && pref != SeatPrefRandom {
		return nil, matchError(CodeInvalidRequest, "seatPreference must be first, second, or random")
	}
	if p.Opponent != "" && p.Opponent == challengerDID {
		return nil, matchError(CodeInvalidRequest, "cannot challenge yourself")
	}

	now := m.now()
	// §10: at most one open challenge per DID per game type. Direct
	// challenges are addressed, not open, and are not limited.
	if p.Opponent == "" {
		n, err := m.repos.Challenges.CountOpenBy(ctx, challengerDID, p.GameType, now)
		if err != nil {
			return nil, err
		}
		if n >= 1 {
			return nil, matchError(CodeRateLimitExceeded,
				"at most one open challenge per DID per game type (spec §10)")
		}
	}

	var variant *string
	if p.Variant != "" {
		variant = &p.Variant
	} else {
		v := engine.ChessVariantStandard
		variant = &v
	}
	tcJSON, err := json.Marshal(p.TimeControl)
	if err != nil {
		return nil, err
	}
	var delayJSON json.RawMessage
	if p.CommentaryDelay != nil {
		delayJSON, err = json.Marshal(*p.CommentaryDelay)
		if err != nil {
			return nil, err
		}
	}

	status := repo.ChallengeOpen
	var opponent *string
	if p.Opponent != "" {
		status = repo.ChallengePending
		opponent = &p.Opponent
	}
	c := &repo.Challenge{
		ID:              tid.Next().String(),
		ChallengerDID:   challengerDID,
		OpponentDID:     opponent,
		GameType:        p.GameType,
		Variant:         variant,
		TimeControl:     tcJSON,
		CommentaryDelay: delayJSON,
		SeatPreference:  pref,
		Rated:           p.Rated,
		Status:          status,
		ExpiresAt:       &p.ExpiresAt,
		// RepoURI stays NULL: XRPC-created challenges have no repo record
		// (spec §9a.5; the Phase E indexer will attach indexed ones).
	}
	if err := m.repos.Challenges.Insert(ctx, c); err != nil {
		return nil, fmt.Errorf("match: insert challenge: %w", err)
	}

	// The addressed opponent learns of a direct challenge immediately over
	// match.subscribe. Open challenges have no specific recipient: agents
	// watch game.listChallenges (spec §9a.2).
	if opponent != nil {
		m.publishChallengeReceived(*opponent, c)
	}
	return c, nil
}

func (m *Matcher) publishChallengeReceived(did string, c *repo.Challenge) {
	m.hub.Publish(did, HubEvent{
		Kind: KindChallengeReceived,
		Challenge: ChallengeEvent{
			ChallengeID:     c.ID,
			Challenger:      c.ChallengerDID,
			GameType:        c.GameType,
			Variant:         derefStr(c.Variant),
			TimeControlJSON: c.TimeControl,
			Rated:           c.Rated,
			ExpiresAt:       derefTime(c.ExpiresAt, time.Time{}),
		},
	})
}

// AcceptOutcome is a successful acceptance: the created game plus the
// acceptor's seat.
type AcceptOutcome struct {
	Challenge *repo.Challenge
	Created   *games.CreatedGame
	Seat      string
}

// AcceptChallenge validates eligibility, wins the accept race with a
// conditional status transition, and creates the game with the acceptor's
// clock anchored at the acceptance time (spec §5.5: "the game starts
// immediately; first mover's clock starts at acceptance time").
func (m *Matcher) AcceptChallenge(ctx context.Context, acceptorDID, challengeID string) (*AcceptOutcome, error) {
	now := m.now()
	c, err := m.repos.Challenges.Get(ctx, challengeID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, matchError(CodeChallengeNotFound, "no such challenge")
		}
		return nil, err
	}
	if c.Status != repo.ChallengeOpen && c.Status != repo.ChallengePending {
		return nil, matchError(CodeChallengeUnavailable, "challenge is %s", c.Status)
	}
	if c.ExpiresAt != nil && now.After(*c.ExpiresAt) {
		return nil, matchError(CodeChallengeUnavailable, "challenge expired")
	}
	if c.OpponentDID != nil {
		// Direct: only the addressed opponent may accept.
		if *c.OpponentDID != acceptorDID {
			return nil, matchError(CodeForbidden, "this challenge is addressed to another agent")
		}
	} else if acceptorDID == c.ChallengerDID {
		return nil, matchError(CodeInvalidRequest, "cannot accept your own open challenge")
	}

	// Eligibility: an indexed profile with capabilities must list the game
	// type (a payload NSID). No indexed profile → allowed (Phase E feeds
	// profiles via the indexer).
	if err := m.checkEligibility(ctx, acceptorDID, c.GameType); err != nil {
		return nil, err
	}

	// §10 global concurrency cap applies to accepting.
	active, err := m.repos.Games.CountActiveByPlayer(ctx, acceptorDID)
	if err != nil {
		return nil, err
	}
	// The accept path enforces the §10 cap; a zero cap (partially-
	// populated config) would reject everyone, so fall back to the default.
	maxGames := m.cfg.Tunables.MaxConcurrentGames
	if maxGames <= 0 {
		maxGames = 20
	}
	if active >= maxGames {
		return nil, matchError(CodeMaxConcurrentGames,
			"at most %d concurrent games per DID (spec §10)", maxGames)
	}

	// Race arbiter: exactly one concurrent acceptor flips the status.
	prevStatus := c.Status
	ok, err := m.repos.Challenges.TransitionStatus(ctx, c.ID,
		[]string{repo.ChallengeOpen, repo.ChallengePending}, repo.ChallengeAccepted)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, matchError(CodeChallengeUnavailable, "challenge was already taken")
	}

	created, seat, err := m.createChallengeGame(ctx, c, acceptorDID, now)
	if err != nil {
		// Give the challenge back so it can still be accepted.
		if reverted, rerr := m.repos.Challenges.TransitionStatus(ctx, c.ID,
			[]string{repo.ChallengeAccepted}, prevStatus); rerr != nil {
			m.logger.Error("match: challenge status revert failed", "challenge", c.ID, "err", rerr)
		} else if !reverted {
			m.logger.Error("match: challenge status revert was a no-op", "challenge", c.ID)
		}
		return nil, err
	}
	return &AcceptOutcome{Challenge: c, Created: created, Seat: seat}, nil
}

// createChallengeGame assigns seats from the challenge's seatPreference and
// creates the game. Repo-backed challenges (repo_uri set by the Phase E
// indexer) will pass the strongRef; XRPC-created ones cannot (documented
// at CreateChallenge).
func (m *Matcher) createChallengeGame(ctx context.Context, c *repo.Challenge, acceptorDID string, acceptedAt time.Time) (*games.CreatedGame, string, error) {
	variant := derefStr(c.Variant)
	seats, err := m.games.SeatOrder(c.GameType, variant)
	if err != nil {
		return nil, "", matchError(CodeInvalidRequest, "%v", err)
	}
	if len(seats) != 2 {
		return nil, "", matchError(CodeInvalidRequest, "game type %q does not have exactly two seats", c.GameType)
	}

	// Seat assignment (spec §9a.1): first → challenger takes the first
	// seat; second → acceptor takes it; random → seeded coin.
	challengerFirst := false
	switch c.SeatPreference {
	case SeatPrefFirst:
		challengerFirst = true
	case SeatPrefSecond:
		challengerFirst = false
	default: // random
		challengerFirst = m.rand.Intn(2) == 0
	}
	var players []games.PlayerSpec
	if challengerFirst {
		players = []games.PlayerSpec{
			{DID: c.ChallengerDID, Seat: seats[0]},
			{DID: acceptorDID, Seat: seats[1]},
		}
	} else {
		players = []games.PlayerSpec{
			{DID: acceptorDID, Seat: seats[0]},
			{DID: c.ChallengerDID, Seat: seats[1]},
		}
	}
	acceptorSeat := seats[1]
	if !challengerFirst {
		acceptorSeat = seats[0]
	}

	var tc clock.TimeControl
	if err := json.Unmarshal(c.TimeControl, &tc); err != nil {
		return nil, "", fmt.Errorf("match: corrupt challenge time control: %w", err)
	}
	var delay *config.CommentaryDelay
	if len(c.CommentaryDelay) > 0 {
		delay = &config.CommentaryDelay{}
		if err := json.Unmarshal(c.CommentaryDelay, delay); err != nil {
			return nil, "", fmt.Errorf("match: corrupt challenge commentary delay: %w", err)
		}
	}

	var challengeRef *games.ChallengeRef
	if c.RepoURI != nil && c.RepoCID != nil {
		// Repo-backed (the Phase E indexer filled repo_uri + repo_cid from
		// the bot.plays.bot.game.challenge record): attach the strongRef.
		// Both parts are required for a valid com.atproto.repo.strongRef;
		// XRPC-created challenges (repo_uri NULL) carry none.
		challengeRef = &games.ChallengeRef{URI: *c.RepoURI, CID: *c.RepoCID}
	}

	created, err := m.games.CreateGame(ctx, games.CreateParams{
		GameType:        c.GameType,
		Variant:         variant,
		Players:         players,
		TimeControl:     tc,
		CommentaryDelay: delay,
		StartedAt:       acceptedAt,
		Challenge:       challengeRef,
	})
	if err != nil {
		return nil, "", err
	}
	return created, acceptorSeat, nil
}

// checkEligibility enforces profile capabilities when an indexed profile
// declares them.
func (m *Matcher) checkEligibility(ctx context.Context, did, gameType string) error {
	actor, err := m.repos.Actors.Get(ctx, did)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil // no indexed profile: allow (Phase E feeds profiles)
		}
		return err
	}
	if len(actor.Profile) == 0 {
		return nil
	}
	var profile struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(actor.Profile, &profile); err != nil || len(profile.Capabilities) == 0 {
		return nil // undeclared or unparsable capabilities: allow
	}
	for _, cap := range profile.Capabilities {
		if cap == gameType {
			return nil
		}
	}
	return matchError(CodeForbidden, "profile capabilities do not include %s", gameType)
}

// DeclineChallenge marks the challenge declined. A direct challenge may
// only be declined by its addressed opponent; an open challenge may be
// declined by anyone but its challenger (the social signal "not picking
// this up" — there is no single opponent to require).
func (m *Matcher) DeclineChallenge(ctx context.Context, did, challengeID string) error {
	c, err := m.repos.Challenges.Get(ctx, challengeID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return matchError(CodeChallengeNotFound, "no such challenge")
		}
		return err
	}
	if c.Status != repo.ChallengeOpen && c.Status != repo.ChallengePending {
		return matchError(CodeChallengeUnavailable, "challenge is %s", c.Status)
	}
	if c.OpponentDID != nil {
		if *c.OpponentDID != did {
			return matchError(CodeForbidden, "only the challenged opponent may decline")
		}
	} else if did == c.ChallengerDID {
		return matchError(CodeForbidden, "cancel your own challenge instead")
	}
	ok, err := m.repos.Challenges.TransitionStatus(ctx, c.ID,
		[]string{repo.ChallengeOpen, repo.ChallengePending}, repo.ChallengeDeclined)
	if err != nil {
		return err
	}
	if !ok {
		return matchError(CodeChallengeUnavailable, "challenge is no longer live")
	}
	return nil
}

// CancelChallenge withdraws the caller's own challenge.
func (m *Matcher) CancelChallenge(ctx context.Context, did, challengeID string) error {
	c, err := m.repos.Challenges.Get(ctx, challengeID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return matchError(CodeChallengeNotFound, "no such challenge")
		}
		return err
	}
	if c.ChallengerDID != did {
		return matchError(CodeForbidden, "only the challenger may cancel")
	}
	if c.Status != repo.ChallengeOpen && c.Status != repo.ChallengePending {
		return matchError(CodeChallengeUnavailable, "challenge is %s", c.Status)
	}
	ok, err := m.repos.Challenges.TransitionStatus(ctx, c.ID,
		[]string{repo.ChallengeOpen, repo.ChallengePending}, repo.ChallengeCancelled)
	if err != nil {
		return err
	}
	if !ok {
		return matchError(CodeChallengeUnavailable, "challenge is no longer live")
	}
	return nil
}

// ChallengeFilter selects rows for ListChallenges.
type ChallengeFilter struct {
	GameType string
	// Opponent filters to challenges addressed to this DID; the literal
	// "me" resolves to the authenticated caller.
	Opponent string
	// Open filters open (true) or non-open (false) challenges; nil = any.
	Open   *bool
	Limit  int
	Cursor string
}

// ListChallenges returns live challenges matching the filter (expired ones
// excluded, spec §5.5) plus the next cursor.
func (m *Matcher) ListChallenges(ctx context.Context, callerDID string, f ChallengeFilter) ([]*repo.Challenge, string, error) {
	opponent := f.Opponent
	if opponent == "me" {
		if callerDID == "" {
			return nil, "", matchError(CodeInvalidRequest, "opponent=me requires authentication")
		}
		opponent = callerDID
	}
	challenges, err := m.repos.Challenges.List(ctx, repo.ChallengesFilter{
		GameType: f.GameType,
		Opponent: opponent,
		Open:     f.Open,
		BeforeID: f.Cursor,
		Limit:    f.Limit,
	}, m.now())
	if err != nil {
		return nil, "", err
	}
	var cursor string
	if len(challenges) == f.Limit && f.Limit > 0 {
		cursor = challenges[len(challenges)-1].ID
	}
	return challenges, cursor, nil
}
