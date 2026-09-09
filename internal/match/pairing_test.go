package match

// Unit tests for the pure pairing core (spec §9a.3): window widening and
// its cap, provisional infinite windows, cooldown (with the ≤3-seeker
// override), maxConcurrent exclusion, distinct-DID, greedy ordering, and
// seat alternation with seeded tie-breaks. No database: every input is a
// plain value.

import (
	"math/rand"
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func defaultParams(now time.Time) PairParams {
	return PairParams{
		Now:               now,
		Widen:             50,
		WidenInterval:     30 * time.Second,
		WindowCap:         800,
		RepeatCooldown:    10 * time.Minute,
		DistinctOperators: true,
		RecentPairs:       map[string]struct{}{},
		FirstSeat:         "white",
		SecondSeat:        "black",
		Rand:              rand.New(rand.NewSource(1)),
	}
}

func estab(seekID, did string, rating, window int64, created time.Time) Candidate {
	return Candidate{
		SeekID: seekID, DID: did,
		Rating:        Rating{Value: rating, Deviation: 50, Games: 25},
		RatingWindow:  window,
		MaxConcurrent: 5,
		CreatedAt:     created,
	}
}

// 1. Window widening: W = ratingWindow + 50·floor(wait/30), capped at 800.

func TestEffectiveWindow(t *testing.T) {
	cases := []struct {
		name   string
		window int64
		wait   time.Duration
		want   int64
	}{
		{"t=0s no widening", 200, 0, 200},
		{"t=29s still one window", 200, 29 * time.Second, 200},
		{"t=30s one step (spec interval boundary)", 200, 30 * time.Second, 250},
		{"t=31s one step", 200, 31 * time.Second, 250},
		{"t=61s two steps", 200, 61 * time.Second, 300},
		{"cap at 800", 200, 10 * time.Minute, 800},
		{"already above cap stays capped", 750, 61 * time.Second, 800},
		{"window above cap immediately", 900, 0, 800},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveWindow(tc.window, tc.wait, 50, 30*time.Second, 800); got != tc.want {
				t.Fatalf("EffectiveWindow(%d, %v) = %d, want %d", tc.window, tc.wait, got, tc.want)
			}
		})
	}
}

// The spec's exact examples: W at t=0s/31s/61s and the cap, exercised
// through Pair so a rating-gap exactly at the widened boundary is the
// observable.
func TestWidenedWindowPairsAtBoundary(t *testing.T) {
	p := defaultParams(base)
	a := estab("s1", "did:a", 1500, 200, base.Add(-31*time.Second)) // W = 250
	b := estab("s2", "did:b", 1740, 200, base.Add(-31*time.Second)) // gap 240 ≤ 250
	pairs := Pair([]Candidate{a, b}, p)
	if len(pairs) != 1 {
		t.Fatalf("gap 240 at t=31s (W=250) did not pair: %d pairs", len(pairs))
	}

	// At t=0s the window is 200: the same 240 gap is out of reach.
	p = defaultParams(base)
	a.CreatedAt = base
	b.CreatedAt = base
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 0 {
		t.Fatalf("gap 240 at t=0s (W=200) paired: %d pairs", len(pairs))
	}

	// But the min(WA, WB) rule: only the wider window widens, the pair is
	// still blocked (a waited, b just arrived).
	p = defaultParams(base)
	a.CreatedAt = base.Add(-61 * time.Second) // WA = 300
	b.CreatedAt = base                        // WB = 200
	b.Rating.Value = 1660                     // gap 160 ≤ 200: pairs
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 1 {
		t.Fatal("gap 160 with min(W)=200 did not pair")
	}
	p = defaultParams(base)
	b.Rating.Value = 1740 // gap 240 > WB=200 despite WA=300: blocked
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 0 {
		t.Fatal("gap 240 paired despite the fresh candidate's narrow window")
	}
}

// 2. Provisional raters pair with anyone: the window is infinite in both
// directions, and established agents' windows do not exclude them.
func TestProvisionalInfiniteWindow(t *testing.T) {
	p := defaultParams(base)
	provisional := estab("s1", "did:new", DefaultRating, 200, base)
	provisional.Rating = Rating{Value: DefaultRating, Deviation: 350, Games: 0}
	champ := estab("s2", "did:champ", 1500+3000, 50, base) // gap 3000, tiny window
	if pairs := Pair([]Candidate{provisional, champ}, p); len(pairs) != 1 {
		t.Fatal("provisional vs established (gap 3000) did not pair")
	}

	// Two provisional agents pair trivially.
	p = defaultParams(base)
	other := champ
	other.DID, other.SeekID, other.Rating = "did:new2", "s3", Rating{Value: 500, Deviation: 350, Games: 3}
	provisional.Rating.Games = 2
	if pairs := Pair([]Candidate{provisional, other}, p); len(pairs) != 1 {
		t.Fatal("two provisional agents did not pair")
	}

	// But two established agents at the same gap stay apart.
	p = defaultParams(base)
	x := estab("s4", "did:x", 1500, 200, base)
	y := estab("s5", "did:y", 1500+3000, 200, base)
	if pairs := Pair([]Candidate{x, y}, p); len(pairs) != 0 {
		t.Fatal("two established agents with gap 3000 paired")
	}
}

// 3. Cooldown blocks a repeat pair when the pool has >3 seekers, and is
// ignored at ≤3.
func TestCooldownRules(t *testing.T) {
	// ≤3 seekers: the pair in the cooldown set still pairs.
	p := defaultParams(base)
	p.RepeatCooldown = 10 * time.Minute
	p.RecentPairs = map[string]struct{}{cooldownKey("did:a", "did:b"): {}}
	a := estab("s1", "did:a", 1500, 200, base)
	b := estab("s2", "did:b", 1500, 200, base.Add(-time.Second))
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 1 {
		t.Fatal("cooldown blocked a pair in a 2-seeker pool (must be ignored at ≤3)")
	}

	// >3 seekers: the same cooldown set blocks a↔b. a is the longest
	// waiter, so it takes c; b then pairs with d; a↔b never forms.
	a = estab("s1", "did:a", 1500, 200, base.Add(-3*time.Second))
	b = estab("s2", "did:b", 1500, 200, base.Add(-2*time.Second))
	c := estab("s3", "did:c", 1500, 200, base.Add(-time.Second))
	d := estab("s4", "did:d", 1500, 200, base)
	p = defaultParams(base)
	p.RecentPairs = map[string]struct{}{cooldownKey("did:a", "did:b"): {}}
	pairs := Pair([]Candidate{a, b, c, d}, p)
	if len(pairs) != 2 {
		t.Fatalf("4-seeker pool produced %d pairs, want 2", len(pairs))
	}
	for _, pr := range pairs {
		key := cooldownKey(pr.A.DID, pr.B.DID)
		if _, blocked := p.RecentPairs[key]; blocked {
			t.Fatalf("pair %s formed despite cooldown", key)
		}
	}
}

// 4. Seekers at maxConcurrent are excluded from pairing.
func TestMaxConcurrentExclusion(t *testing.T) {
	p := defaultParams(base)
	busy := estab("s1", "did:busy", 1500, 200, base.Add(-time.Minute))
	busy.ActiveGames, busy.MaxConcurrent = 5, 5
	free := estab("s2", "did:free", 1500, 200, base.Add(-time.Second))
	if pairs := Pair([]Candidate{busy, free}, p); len(pairs) != 0 {
		t.Fatal("paired a seeker already at maxConcurrent")
	}

	// One game under the cap still pairs.
	busy.ActiveGames = 4
	if pairs := Pair([]Candidate{busy, free}, p); len(pairs) != 1 {
		t.Fatal("did not pair a seeker one game under maxConcurrent")
	}
}

// 5. Distinct DIDs: the same DID never pairs with itself, even with two
// seeks in the pool.
func TestDistinctDIDs(t *testing.T) {
	p := defaultParams(base)
	s1 := estab("s1", "did:solo", 1500, 200, base.Add(-time.Minute))
	s2 := estab("s2", "did:solo", 1500, 200, base.Add(-time.Second))
	if pairs := Pair([]Candidate{s1, s2}, p); len(pairs) != 0 {
		t.Fatal("a DID was paired with itself")
	}
}

// 6. Distinct operators when both are known (config-gated).
func TestDistinctOperators(t *testing.T) {
	a := estab("s1", "did:a", 1500, 200, base.Add(-time.Minute))
	a.OperatorDID = "did:operator"
	b := estab("s2", "did:b", 1500, 200, base.Add(-time.Second))
	b.OperatorDID = "did:operator"

	p := defaultParams(base)
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 0 {
		t.Fatal("two bots of one operator paired despite DistinctOperators")
	}
	p.DistinctOperators = false
	if pairs := Pair([]Candidate{a, b}, p); len(pairs) != 1 {
		t.Fatal("same-operator bots blocked with DistinctOperators off")
	}

	// Unknown operators (no indexed profile) are never a reason to block.
	p = defaultParams(base)
	c := estab("s3", "did:c", 1500, 200, base.Add(-time.Second))
	c.OperatorDID = "did:operator" // a's operator unknown (""), same operator on one side
	a.OperatorDID = ""
	if pairs := Pair([]Candidate{a, c}, p); len(pairs) != 1 {
		t.Fatal("unknown operator blocked pairing")
	}
}

// 7. Greedy ordering: the longest waiter pairs first and claims the
// closest-rated partner; the rating gap is the partner tiebreak.
func TestGreedyOrdering(t *testing.T) {
	p := defaultParams(base)
	old := estab("s-old", "did:old", 1500, 200, base.Add(-10*time.Minute))
	mid := estab("s-mid", "did:mid", 1560, 200, base.Add(-5*time.Minute))
	closeR := estab("s-close", "did:close", 1510, 200, base.Add(-time.Second))
	farR := estab("s-far", "did:far", 1600, 400, base.Add(-time.Second))

	// old is the longest waiter and should take closeR (gap 10 over gap
	// 60), leaving mid with farR (gap 40).
	pairs := Pair([]Candidate{old, mid, closeR, farR}, p)
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want 2", len(pairs))
	}
	first := pairs[0]
	if first.A.DID != "did:old" {
		t.Fatalf("first pair initiator = %s, want the longest waiter did:old", first.A.DID)
	}
	if first.B.DID != "did:close" {
		t.Fatalf("longest waiter paired with %s, want the smallest gap did:close", first.B.DID)
	}
	second := pairs[1]
	if second.A.DID != "did:mid" || second.B.DID != "did:far" {
		t.Fatalf("second pair = %s/%s, want did:mid/did:far", second.A.DID, second.B.DID)
	}

	// Wait-time tiebreak: identical waits pair in DID order (deterministic).
	p = defaultParams(base)
	x := estab("s1", "did:x", 1500, 200, base)
	y := estab("s2", "did:y", 1500, 200, base)
	pairs = Pair([]Candidate{x, y}, p)
	if len(pairs) != 1 || pairs[0].A.DID != "did:x" {
		t.Fatalf("deterministic wait tiebreak broken: %+v", pairs)
	}

	// Gap tiebreak on equal waits: the smaller gap wins.
	p = defaultParams(base)
	anchor := estab("s0", "did:anchor", 1500, 500, base)
	g1 := estab("s1", "did:g1", 1520, 500, base) // gap 20
	g2 := estab("s2", "did:g2", 1700, 500, base) // gap 200
	pairs = Pair([]Candidate{anchor, g1, g2}, p)
	if len(pairs) != 1 || pairs[0].B.DID != "did:g1" {
		t.Fatalf("gap tiebreak broken: %+v", pairs)
	}
}

// 8. Seat alternation: the agent who played first-mover most recently gets
// the second seat; unknown/tied history breaks on the seeded coin.
func TestSeatAlternation(t *testing.T) {
	p := defaultParams(base)
	p.Rand = rand.New(rand.NewSource(42)) // fixed sequence for determinism

	a := estab("s1", "did:a", 1500, 200, base.Add(-time.Minute))
	a.LastSeat = "white" // a played first-mover most recently
	b := estab("s2", "did:b", 1500, 200, base.Add(-time.Second))
	b.LastSeat = "black"

	pairs := Pair([]Candidate{a, b}, p)
	if len(pairs) != 1 {
		t.Fatal("did not pair")
	}
	pr := pairs[0]
	if pr.APlaysFirst {
		t.Fatal("did:a played first-mover most recently but took the first seat again")
	}
	if pr.SeatA != "black" || pr.SeatB != "white" {
		t.Fatalf("seats = %s/%s, want black/white (a on second)", pr.SeatA, pr.SeatB)
	}

	// Flip: b was the recent first-mover → b takes second, a takes first.
	b.LastSeat = "white"
	a.LastSeat = "black"
	p = defaultParams(base)
	p.Rand = rand.New(rand.NewSource(42))
	pairs = Pair([]Candidate{a, b}, p)
	if !pairs[0].APlaysFirst || pairs[0].SeatA != "white" {
		t.Fatalf("recent first-mover b did not yield the first seat: %+v", pairs[0])
	}

	// Tie (both played white last): the coin decides, and the same seed
	// always decides the same way.
	for _, seed := range []int64{1, 2, 3, 7, 99} {
		a.LastSeat, b.LastSeat = "white", "white"
		p = defaultParams(base)
		p.Rand = rand.New(rand.NewSource(seed))
		pr1 := Pair([]Candidate{a, b}, p)[0]
		p = defaultParams(base)
		p.Rand = rand.New(rand.NewSource(seed))
		pr2 := Pair([]Candidate{a, b}, p)[0]
		if pr1.APlaysFirst != pr2.APlaysFirst {
			t.Fatalf("seed %d: coin not deterministic (%v vs %v)", seed, pr1.APlaysFirst, pr2.APlaysFirst)
		}
	}

	// Neither has history: coin decides (both outcomes valid, but the
	// marker seats still map consistently).
	a.LastSeat, b.LastSeat = "", ""
	p = defaultParams(base)
	p.Rand = rand.New(rand.NewSource(5))
	pr = Pair([]Candidate{a, b}, p)[0]
	if pr.APlaysFirst != (pr.SeatA == "white") {
		t.Fatalf("seat markers inconsistent with APlaysFirst: %+v", pr)
	}
}

// 9. Pairing metadata: ratingGap and waitMs reflect the pair.
func TestPairingMetadata(t *testing.T) {
	p := defaultParams(base)
	a := estab("s1", "did:a", 1500, 200, base.Add(-65*time.Second))
	b := estab("s2", "did:b", 1550, 200, base.Add(-5*time.Second))
	pr := Pair([]Candidate{a, b}, p)[0]
	if pr.RatingGap != 50 {
		t.Fatalf("ratingGap = %d, want 50", pr.RatingGap)
	}
	if pr.WaitMs != 65000 {
		t.Fatalf("waitMs = %d, want 65000 (the longer wait)", pr.WaitMs)
	}
}
