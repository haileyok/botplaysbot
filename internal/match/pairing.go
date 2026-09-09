package match

// The pure pairing core (spec §9a.3 steps 1–5). Everything here is
// deterministic given its inputs: the matcher loads candidate data
// (ratings, cooldown set, seat history) from the database, runs Pair, and
// materializes the result. Keeping the rules pure makes the spec's edge
// cases — widening, the cap, provisional infinities, the ≤3-seeker cooldown
// override, greedy ordering, seeded seat ties — unit-testable without a
// database.

import (
	"math/rand"
	"sort"
	"time"
)

// Candidate is one seeker, projected for the pairing core.
type Candidate struct {
	SeekID string
	DID    string
	Rating Rating
	// RatingWindow is the seeker's requested initial window (seek param,
	// spec default 200).
	RatingWindow int64
	// MaxConcurrent caps the seeker's simultaneous games (seek param,
	// default 5 — per-seek, distinct from the global §10 cap of 20).
	MaxConcurrent int64
	// ActiveGames is the seeker's active game count at pass start.
	ActiveGames int
	// CreatedAt is when the seeker entered the pool (the wait clock).
	CreatedAt time.Time
	// OperatorDID is the agent profile's verified operator, "" when no
	// indexed profile or no operator is set (Phase E fills this).
	OperatorDID string
	// LastSeat is the seat (engine seat name) the agent played most
	// recently in this game type, "" when never.
	LastSeat string
}

// PairParams are the pool-level inputs to Pair.
type PairParams struct {
	// Now is the pass time; wait times derive from it.
	Now time.Time
	// Widen is the window increment per WidenInterval (spec: 50 per 30s).
	Widen         int
	WidenInterval time.Duration
	// WindowCap is the maximum effective window (spec: 800).
	WindowCap int64
	// RepeatCooldown is the minimum gap between games for one pair (10m).
	RepeatCooldown time.Duration
	// DistinctOperators blocks pairing two bots sharing a verified
	// operator when both have indexed profiles (§9a.3; no-op until the
	// Phase E profile indexer runs).
	DistinctOperators bool
	// RecentPairs is the set of DID pairs (sorted, "a|b") that played this
	// game type within the cooldown window.
	RecentPairs map[string]struct{}
	// FirstSeat/SecondSeat are the engine's seat names in turn order
	// (e.g. white/black). The alternation rule needs to recognize the
	// first-mover seat in a candidate's LastSeat; empty values fall back
	// to the abstract "first"/"second" markers.
	FirstSeat  string
	SecondSeat string
	// Rand is the seeded source for seat tie-breaks. Pair is deterministic
	// given Rand's state.
	Rand *rand.Rand
}

// Pairing is one decided pair. A is the longer-waiting seeker; SeatA/
// SeatB are engine seat names, and SeatA always belongs to the first mover.
type Pairing struct {
	A     Candidate
	B     Candidate
	SeatA string // A's seat
	SeatB string // B's seat
	// APlaysFirst reports whether A takes the engine's first seat
	// (the first mover position), which the matcher maps to SeatOrder[0].
	APlaysFirst bool
	// RatingGap is |rA − rB| at pairing time.
	RatingGap int64
	// WaitMs is how long the longer-waiting side waited.
	WaitMs int64
}

// EffectiveWindow computes the widened rating window (spec §9a.3 step 1):
// W = ratingWindow + widen × floor(waitSeconds / interval), capped. The cap
// is applied only when positive (cap ≤ 0 disables capping).
func EffectiveWindow(ratingWindow int64, wait time.Duration, widen int, interval time.Duration, cap int64) int64 {
	w := ratingWindow
	if interval > 0 && wait > 0 {
		w += int64(widen) * int64(wait/interval)
	}
	if cap > 0 && w > cap {
		w = cap
	}
	if w < 0 {
		w = 0
	}
	return w
}

// cooldownKey builds the sorted-pair set key.
func cooldownKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "|" + b
}

// Pair runs one greedy pass over one pool's candidates and returns the
// pairings (spec §9a.3):
//
//  1. seekers at their maxConcurrent are excluded;
//  2. pairs require |rA − rB| ≤ min(WA, WB) with per-candidate widened
//     windows, where any provisional rating makes the window infinite in
//     both directions;
//  3. pairs require distinct DIDs and (optionally) distinct operators when
//     both operators are known, and no shared game within RepeatCooldown;
//  4. greedy order: longest wait first, smallest rating gap as partner
//     preference;
//  5. seats alternate: whoever played the first-mover seat more recently
//     takes the second seat; ties break on the seeded coin.
func Pair(cands []Candidate, p PairParams) []Pairing {
	// Exclude seekers already at their per-seek concurrency cap.
	pool := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.MaxConcurrent > 0 && c.ActiveGames >= int(c.MaxConcurrent) {
			continue
		}
		pool = append(pool, c)
	}
	if len(pool) < 2 {
		return nil
	}

	// Greedy order: longest wait first; DID breaks exact ties for
	// determinism.
	sort.SliceStable(pool, func(i, j int) bool {
		wi, wj := p.Now.Sub(pool[i].CreatedAt), p.Now.Sub(pool[j].CreatedAt)
		if wi != wj {
			return wi > wj
		}
		return pool[i].DID < pool[j].DID
	})

	paired := make(map[int]bool, len(pool))
	var out []Pairing
	// The repeat cooldown applies only when the pool has more than 3
	// seekers (spec §9a.3 step 3: "ignored if the pool has ≤ 3 seekers") —
	// with a tiny pool the alternative is nobody plays.
	cooldownActive := len(pool) > 3 && p.RepeatCooldown > 0
	for i := range pool {
		if paired[i] {
			continue
		}
		a := pool[i]
		best := -1
		bestGap := int64(0)
		for j := range pool {
			if j == i || paired[j] {
				continue
			}
			b := pool[j]
			if !compatible(a, b, cooldownActive, p) {
				continue
			}
			gap := abs64(a.Rating.Value - b.Rating.Value)
			if best == -1 || gap < bestGap ||
				(gap == bestGap && pool[j].CreatedAt.Before(pool[best].CreatedAt)) ||
				(gap == bestGap && pool[j].CreatedAt.Equal(pool[best].CreatedAt) && pool[j].DID < pool[best].DID) {
				best, bestGap = j, gap
			}
		}
		if best == -1 {
			continue
		}
		paired[i], paired[best] = true, true
		out = append(out, newPairing(a, pool[best], bestGap, p))
	}
	return out
}

// compatible reports whether a and b may pair under the window, operator,
// and cooldown rules.
func compatible(a, b Candidate, cooldownActive bool, p PairParams) bool {
	// Distinct DIDs: two seeks from one DID never pair with each other.
	if a.DID == b.DID {
		return false
	}
	// Windows: a provisional rating on either side widens the window to
	// infinite in both directions (spec §9a.3 step 2).
	if !a.Rating.Provisional() && !b.Rating.Provisional() {
		wa := EffectiveWindow(a.RatingWindow, p.Now.Sub(a.CreatedAt), p.Widen, p.WidenInterval, p.WindowCap)
		wb := EffectiveWindow(b.RatingWindow, p.Now.Sub(b.CreatedAt), p.Widen, p.WidenInterval, p.WindowCap)
		if abs64(a.Rating.Value-b.Rating.Value) > min64(wa, wb) {
			return false
		}
	}
	// Distinct operators when both are known (an operator's bots do not
	// farm each other, spec §9a.3 step 3).
	if p.DistinctOperators && a.OperatorDID != "" && a.OperatorDID == b.OperatorDID {
		return false
	}
	// Repeat cooldown (spec §9a.3 step 3), active only when the pool has
	// more than 3 seekers.
	if cooldownActive {
		if _, blocked := p.RecentPairs[cooldownKey(a.DID, b.DID)]; blocked {
			return false
		}
	}
	return true
}

// newPairing assigns seats for one pair (spec §9a.3 step 5): the agent who
// played the first-mover seat most recently gets the second seat; a tie
// (both, neither, or no history) breaks on the seeded coin.
func newPairing(a, b Candidate, gap int64, p PairParams) Pairing {
	first, second := p.FirstSeat, p.SecondSeat
	if first == "" {
		first = firstSeatMarker
	}
	if second == "" {
		second = secondSeatMarker
	}
	aWasFirst := a.LastSeat == first
	bWasFirst := b.LastSeat == first
	aPlaysFirst := false
	switch {
	case aWasFirst && !bWasFirst:
		aPlaysFirst = false // A moved first most recently → A takes the second seat
	case bWasFirst && !aWasFirst:
		aPlaysFirst = true // B moved first most recently → A takes the first seat
	default:
		aPlaysFirst = p.Rand.Intn(2) == 0
	}
	pr := Pairing{A: a, B: b, RatingGap: gap, APlaysFirst: aPlaysFirst}
	if aPlaysFirst {
		pr.SeatA, pr.SeatB = first, second
	} else {
		pr.SeatA, pr.SeatB = second, first
	}
	wait := p.Now.Sub(a.CreatedAt)
	if wb := p.Now.Sub(b.CreatedAt); wb > wait {
		wait = wb
	}
	pr.WaitMs = wait.Milliseconds()
	return pr
}

// firstSeatMarker/secondSeatMarker are the pairing core's abstract seats;
// the matcher maps "first" to engine SeatOrder[0].
const (
	firstSeatMarker  = "first"
	secondSeatMarker = "second"
)

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
