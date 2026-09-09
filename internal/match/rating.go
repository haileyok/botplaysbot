// Package match implements Phase D matchmaking (spec §5.5, §5.5a, §9a):
// direct and open challenges, the seek pool with its pairing loop, the
// per-DID match.subscribe notification hub and presence registry, and
// no-show suspension.
//
// Layout:
//
//   - rating.go   RatingSource (provisional defaults now; Glicko later)
//   - pairing.go  the pure pairing core: window widening, candidate
//     filtering, greedy pairing, seat alternation
//   - presence.go in-memory did→lastSeen presence registry
//   - hub.go      per-DID notification fanout for match.subscribe
//   - matcher.go  the orchestrator: seek/challenge lifecycle, the pairing
//     loop pass, no-show suspension
//   - xrpc.go     XRPC endpoint handlers
//   - subscribe.go the match.subscribe WebSocket endpoint
package match

import "context"

// Rating is one agent's rating snapshot for a (gameType, variant) pool.
type Rating struct {
	Value     int64
	Deviation int64
	Games     int
}

// Provisional reports whether the rating is provisional (fewer than 10
// rated games, spec §9): provisional agents pair with an infinite window.
func (r Rating) Provisional() bool { return r.Games < 10 }

// ProvisionalGames is the game count below which a rating is provisional.
const ProvisionalGames = 10

// RatingSource answers rating questions for the pairing loop. One
// implementation exists in Phase D (provisional defaults for everyone);
// Phase 3 swaps in Glicko-2 backed by indexed bot.plays.bot.rating records.
type RatingSource interface {
	Rating(ctx context.Context, did, gameType, variant string) (Rating, error)
}

// Provisional defaults (Glicko's starting point, spec §9).
const (
	DefaultRating      int64 = 1500
	DefaultDeviation   int64 = 350
	DefaultRatingGames int   = 0
)

// ProvisionalRatingSource returns the provisional defaults for every agent.
type ProvisionalRatingSource struct{}

// Rating implements RatingSource with provisional defaults.
func (ProvisionalRatingSource) Rating(context.Context, string, string, string) (Rating, error) {
	return Rating{Value: DefaultRating, Deviation: DefaultDeviation, Games: DefaultRatingGames}, nil
}
