package match

// The pairing loop pass (spec §9a.3): reap expired challenges, expire
// stale standing seeks off the presence registry, group active seeks into
// pools, run the pure pairing core per pool, and materialize each pair
// into a game with matchmaking provenance and a grace-anchored first clock.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// PairOnce runs one full pass and returns how many games it created.
func (m *Matcher) PairOnce(ctx context.Context) (int, error) {
	now := m.now()

	// 1. Expired challenges flip to expired (the reaper rides this loop).
	if _, err := m.repos.Challenges.ExpireDue(ctx, now); err != nil {
		m.logger.Error("match: challenge reaper failed", "err", err)
	}

	// 2. Standing seeks whose DID's match.subscribe connection has been
	// closed longer than StandingExpire expire (spec §9a.3).
	m.expireStaleStanding(ctx, now)

	// 3. Group the active seeks into pools and pair each.
	seeks, err := m.repos.Seeks.ListAllActive(ctx)
	if err != nil {
		return 0, err
	}
	byPool := map[string][]repo.Seek{}
	for _, s := range seeks {
		byPool[s.Pool] = append(byPool[s.Pool], s)
	}

	created := 0
	for pool, group := range byPool {
		pairs, err := m.pairPool(ctx, pool, group, now)
		if err != nil {
			m.logger.Error("match: pool pairing failed", "pool", pool, "err", err)
			continue
		}
		for i := range pairs {
			if err := m.materializePair(ctx, pool, pairs[i], now); err != nil {
				if games.IsCode(err, "GameNotActive") || games.IsCode(err, "InvalidRequest") {
					m.logger.Error("match: pair materialization rejected", "pool", pool, "err", err)
					continue
				}
				return created, err
			}
			created++
		}
	}
	return created, nil
}

// expireStaleStanding deactivates standing seeks whose owner has been
// disconnected from match.subscribe for longer than StandingExpire.
func (m *Matcher) expireStaleStanding(ctx context.Context, now time.Time) {
	seeks, err := m.repos.Seeks.ListAllActive(ctx)
	if err != nil {
		m.logger.Error("match: standing expiry scan failed", "err", err)
		return
	}
	cutoff := now.Add(-m.cfg.Tunables.StandingExpire)
	for _, s := range seeks {
		if s.Mode != SeekModeStanding {
			continue
		}
		if m.hub.Presence().GoneSince(s.DID, cutoff) {
			if err := m.repos.Seeks.SetActive(ctx, s.ID, false); err != nil {
				m.logger.Error("match: standing expiry failed", "seek", s.ID, "err", err)
			}
		}
	}
}

// pairPool builds candidates for one pool and runs the pure pairing core.
// extraGames tracks games created earlier in this pass so a DID cannot
// exceed maxConcurrent within one tick.
func (m *Matcher) pairPool(ctx context.Context, pool string, group []repo.Seek, now time.Time) ([]Pairing, error) {
	if len(group) < 2 {
		return nil, nil
	}
	gameType := group[0].GameType
	variant := ""
	cooldownIgnores := len(group) <= 3

	// The engine's turn order names the seats; the alternation rule and
	// the pairing output speak in these names.
	seats, err := m.games.SeatOrder(gameType, variant)
	if err != nil {
		return nil, err
	}
	if len(seats) != 2 {
		return nil, matchError(CodeInvalidRequest, "game type %q does not have exactly two seats", gameType)
	}

	cands := make([]Candidate, 0, len(group))
	extra := map[string]int{}
	for i := range group {
		s := group[i]
		rating, err := m.rating.Rating(ctx, s.DID, gameType, derefStr(s.Variant))
		if err != nil {
			return nil, err
		}
		active, err := m.repos.Games.CountActiveByPlayer(ctx, s.DID)
		if err != nil {
			return nil, err
		}
		operator := ""
		if actor, err := m.repos.Actors.Get(ctx, s.DID); err == nil && actor.OperatorDID != nil {
			operator = *actor.OperatorDID
		}
		lastSeat := ""
		if seat, ok, err := m.repos.Seats.LastSeat(ctx, s.DID, gameType); err == nil && ok {
			lastSeat = seat
		} else if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return nil, err
		}
		if variant == "" {
			variant = derefStr(s.Variant)
		}
		cands = append(cands, Candidate{
			SeekID:        s.ID,
			DID:           s.DID,
			Rating:        rating,
			RatingWindow:  int64(derefInt(s.RatingWindow)),
			MaxConcurrent: int64(derefInt(s.MaxConcurrent)),
			ActiveGames:   active + extra[s.DID],
			CreatedAt:     derefTime(s.CreatedAt, now),
			OperatorDID:   operator,
			LastSeat:      lastSeat,
		})
	}

	var recent map[string]struct{}
	if !cooldownIgnores {
		pairs, err := m.repos.Pairings.RecentPairs(ctx, gameType, now.Add(-m.cfg.Tunables.RepeatCooldown))
		if err != nil {
			return nil, err
		}
		recent = make(map[string]struct{}, len(pairs))
		for _, p := range pairs {
			recent[cooldownKey(p[0], p[1])] = struct{}{}
		}
	}

	params := PairParams{
		Now:               now,
		Widen:             m.cfg.Tunables.WindowWiden,
		WidenInterval:     m.cfg.Tunables.WindowWidenInterval,
		WindowCap:         int64(m.cfg.Tunables.WindowCap),
		RepeatCooldown:    m.cfg.Tunables.RepeatCooldown,
		DistinctOperators: m.cfg.Tunables.DistinctOperators,
		RecentPairs:       recent,
		FirstSeat:         seats[0],
		SecondSeat:        seats[1],
		Rand:              m.rand,
	}
	pairs := Pair(cands, params)

	// Record in-pass concurrency so the same pass cannot double-book a DID
	// past maxConcurrent.
	for _, pr := range pairs {
		extra[pr.A.DID]++
		extra[pr.B.DID]++
	}
	return pairs, nil
}

// materializePair turns one Pairing into a game: seat alternation mapped
// onto the engine's seat order, matchmaking provenance on the record, the
// first clock anchored at matchedAt + grace (spec §9a.3 step 6), cooldown
// and seat-history persistence, seek bookkeeping, and the #matched
// notifications.
func (m *Matcher) materializePair(ctx context.Context, pool string, pr Pairing, now time.Time) error {
	seekA, err := m.repos.Seeks.Get(ctx, pr.A.SeekID)
	if err != nil {
		return err
	}
	seekB, err := m.repos.Seeks.Get(ctx, pr.B.SeekID)
	if err != nil {
		return err
	}
	gameType := seekA.GameType
	variant := derefStr(seekA.Variant)
	seats, err := m.games.SeatOrder(gameType, variant)
	if err != nil {
		return err
	}
	if len(seats) != 2 {
		return matchError(CodeInvalidRequest, "game type %q does not have exactly two seats", gameType)
	}

	// Map the pairing's engine-seat names onto turn order.
	var firstDID, secondDID string
	if pr.SeatA == seats[0] {
		firstDID, secondDID = pr.A.DID, pr.B.DID
	} else {
		firstDID, secondDID = pr.B.DID, pr.A.DID
	}
	firstSeat, secondSeat := seats[0], seats[1]

	tc, err := decodeSeekTimeControl(seekA.TimeControl)
	if err != nil {
		return err
	}
	// Grace (spec §9a.3 step 6): the first mover's clock starts PLAYSBOT_
	// MATCH_GRACE after the match; startedAt is that moment, and CreateGame
	// anchors the clock there (manager.go: clockAnchor = startedAt).
	startedAt := now.Add(m.cfg.Tunables.MatchGrace)

	created, err := m.games.CreateGame(ctx, games.CreateParams{
		GameType: gameType,
		Variant:  variant,
		Players: []games.PlayerSpec{
			{DID: firstDID, Seat: firstSeat},
			{DID: secondDID, Seat: secondSeat},
		},
		TimeControl: tc,
		StartedAt:   startedAt,
		Matchmaking: &games.MatchmakingInfo{Pool: pool, WaitMs: pr.WaitMs, RatingGap: pr.RatingGap},
	})
	if err != nil {
		return err
	}

	// Persist the social bookkeeping: cooldown history, seat alternation.
	if err := m.repos.Pairings.Add(ctx, firstDID, secondDID, gameType, created.URI); err != nil {
		m.logger.Error("match: pairing history write failed", "err", err)
	}
	if err := m.repos.Seats.SetLastSeat(ctx, firstDID, gameType, firstSeat); err != nil {
		m.logger.Error("match: seat history write failed", "err", err)
	}
	if err := m.repos.Seats.SetLastSeat(ctx, secondDID, gameType, secondSeat); err != nil {
		m.logger.Error("match: seat history write failed", "err", err)
	}

	// Seek bookkeeping: once seeks leave the pool; standing seeks stay
	// active (they re-enter on the next pass, gated by maxConcurrent) and
	// get their last-matched stamp.
	for _, seek := range []*repo.Seek{seekA, seekB} {
		if seek.Mode == SeekModeOnce {
			if err := m.repos.Seeks.SetActive(ctx, seek.ID, false); err != nil {
				m.logger.Error("match: seek deactivation failed", "seek", seek.ID, "err", err)
			}
		} else if err := m.repos.Seeks.TouchMatched(ctx, seek.ID, now); err != nil {
			m.logger.Error("match: seek touch failed", "seek", seek.ID, "err", err)
		}
	}

	// #matched to each side over their match.subscribe connections, with
	// each agent's own seek id, seat, and opponent (spec §5.5a).
	m.hub.Publish(seekA.DID, HubEvent{
		Kind: KindMatched,
		Matched: MatchedEvent{
			SeekID:   seekA.ID,
			Game:     created.URI,
			Opponent: seekB.DID,
			Seat:     seatForDID(seekA.DID, firstDID, secondDID, firstSeat, secondSeat),
		},
	})
	m.hub.Publish(seekB.DID, HubEvent{
		Kind: KindMatched,
		Matched: MatchedEvent{
			SeekID:   seekB.ID,
			Game:     created.URI,
			Opponent: seekA.DID,
			Seat:     seatForDID(seekB.DID, firstDID, secondDID, firstSeat, secondSeat),
		},
	})
	return nil
}

func seatForDID(did, firstDID, secondDID, firstSeat, secondSeat string) string {
	if did == firstDID {
		return firstSeat
	}
	return secondSeat
}

// ---------------------------------------------------------------------------
// No-show suspension (spec §9a.3)

// OnFinish is the games.Manager finish hook: it maintains the consecutive
// no-show streak for matched games. A timeout where the loser was on move
// at ply 1 or 2 (fewer than 2 accepted plies) counts; the third consecutive
// one suspends the DID from the pool and writes a timingAnomaly flag row
// (severity info). Any other finish for a matched game resets both
// players' streaks.
func (m *Matcher) OnFinish(info games.FinishInfo) {
	if !info.Matched || info.Result == nil {
		return
	}
	ctx := context.Background()

	if info.Result.Reason == "timeout" && info.Ply < 2 {
		loser := info.TurnDID
		if loser == "" {
			return
		}
		m.mu.Lock()
		m.noShow[loser]++
		streak := m.noShow[loser]
		if streak < NoShowThreshold {
			m.mu.Unlock()
			return
		}
		delete(m.noShow, loser)
		m.mu.Unlock()

		suspend := m.cfg.Tunables.NoShowSuspend
		if suspend <= 0 {
			suspend = time.Hour
		}
		until := m.now().Add(suspend)
		if err := m.repos.Suspensions.Suspend(ctx, loser, until, "noShow"); err != nil {
			m.logger.Error("match: suspension write failed", "did", loser, "err", err)
		}
		detail := fmt.Sprintf("%d consecutive ply-1/ply-2 timeouts in matched games", streak)
		if m.flags != nil {
			// Phase E: the flag record goes to the service repo alongside
			// the row (spec §10). Emission is exactly-once per
			// (subject, kind, game) inside the emitter.
			if _, err := m.flags.EmitFlag(ctx, loser, &info.GameURI, FlagTimingAnomaly, FlagSeverityInfo, detail); err != nil {
				m.logger.Error("match: timingAnomaly flag emission failed", "did", loser, "err", err)
			}
		} else {
			if err := m.repos.Flags.Insert(ctx, &repo.Flag{
				SubjectDID: loser,
				GameURI:    &info.GameURI,
				Kind:       FlagTimingAnomaly,
				Severity:   FlagSeverityInfo,
				Detail:     &detail,
			}); err != nil {
				m.logger.Error("match: timingAnomaly flag write failed", "did", loser, "err", err)
			}
		}
		m.logger.Warn("match: DID suspended from seek pool", "did", loser, "until", until)
		return
	}

	// Non-no-show finish: both players' streaks reset.
	m.mu.Lock()
	for _, p := range info.Players {
		delete(m.noShow, p.DID)
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// small helpers

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefTime(p *time.Time, fallback time.Time) time.Time {
	if p == nil {
		return fallback
	}
	return *p
}

func decodeSeekTimeControl(raw json.RawMessage) (clock.TimeControl, error) {
	var tc clock.TimeControl
	if len(raw) == 0 {
		return tc, errors.New("seek has no time control")
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return tc, err
	}
	return tc, nil
}
