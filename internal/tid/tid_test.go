package tid

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFormatIsThirteenSortableChars(t *testing.T) {
	id := Next()
	s := id.String()
	if len(s) != 13 {
		t.Fatalf("len = %d, want 13 (%q)", len(s), s)
	}
	const alphabet = "234567abcdefghijklmnopqrstuvwxyz"
	for i, c := range s {
		if !containsRune(alphabet, c) {
			t.Errorf("char %d = %q not in base32-sortable alphabet", i, c)
		}
	}
	// Encoded timestamps must start with a character from the lower half of
	// the alphabet (high bit of the 64-bit value clear).
	if s[0] > 'j' {
		t.Errorf("first char %q encodes a set high bit", s[0])
	}
}

func containsRune(alphabet string, c rune) bool {
	for _, a := range alphabet {
		if a == c {
			return true
		}
	}
	return false
}

func TestMonotonicUnderSameMicrosecond(t *testing.T) {
	g := &generator{}
	frozen := int64(1_757_340_202_000_123)
	nowMicros = func() int64 { return frozen }
	t.Cleanup(func() { nowMicros = func() int64 { return time.Now().UnixMicro() } })

	prev := g.Next().Integer()
	firstMicros := prev >> 10
	for range 3000 {
		next := g.Next().Integer()
		if next <= prev {
			t.Fatalf("TID not increasing: %d then %d", prev, next)
		}
		prev = next
	}
	// Within one frozen microsecond the generator bumps the clock ID (1024
	// slots); past that it advances the microsecond to preserve ordering. So
	// over 3001 calls the timestamp may advance by at most 2 microseconds and
	// never by more than the number of exhausted slots.
	drift := (prev >> 10) - firstMicros
	if drift > 2 {
		t.Fatalf("timestamp drifted %d µs within a frozen clock", drift)
	}
}

func TestClockIDAdvancesOnCollision(t *testing.T) {
	g := &generator{}
	frozen := int64(1_757_340_202_000_000)
	nowMicros = func() int64 { return frozen }
	t.Cleanup(func() { nowMicros = func() int64 { return time.Now().UnixMicro() } })

	first := g.Next()
	second := g.Next()
	if first.ClockID() != second.ClockID()-1 {
		t.Errorf("expected clock ID bump on collision: %d then %d",
			first.ClockID(), second.ClockID())
	}
}

func TestMonotonicAcrossClockRegression(t *testing.T) {
	g := &generator{}
	clock := int64(1_000_000)
	nowMicros = func() int64 { return clock }
	t.Cleanup(func() { nowMicros = func() int64 { return time.Now().UnixMicro() } })

	before := g.Next().Integer()
	clock = 999_999 // simulated backwards system clock
	after := g.Next().Integer()
	if after <= before {
		t.Errorf("TID went backwards across clock regression: %d then %d", before, after)
	}
}

func TestParseRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		micros  int64
		clockID uint
	}{
		{1_757_340_202_000_123, 0},
		{1_757_340_202_000_123, 1023},
		{1, 0},
		{time.Date(2026, 9, 8, 14, 3, 22, 115_000, time.UTC).UnixMicro(), 7},
	} {
		orig, err := FromParts(tc.micros, tc.clockID)
		if err != nil {
			t.Fatalf("FromParts(%d, %d): %v", tc.micros, tc.clockID, err)
		}
		parsed, err := Parse(orig.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", orig, err)
		}
		if parsed != orig {
			t.Errorf("roundtrip mismatch: %q != %q", parsed, orig)
		}
		if got := parsed.Integer() >> 10; got != uint64(tc.micros) {
			t.Errorf("micros: got %d want %d", got, tc.micros)
		}
		if got := parsed.ClockID(); got != tc.clockID {
			t.Errorf("clockID: got %d want %d", got, tc.clockID)
		}
		if want := time.UnixMicro(tc.micros); !parsed.Time().Equal(want) {
			t.Errorf("time: got %v want %v", parsed.Time(), want)
		}
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for _, bad := range []string{
		"",                          // empty
		"zzzzzzzzzzzzz",             // 13 chars but 'z' high-bit first char
		"234567abcdefgh",            // too short
		"234567abcdefghijk",         // too long
		"234567abcdefgh1",           // '1' not in alphabet
		"234567abcdefghl",           // 'l' not in alphabet
		"!!!!!!!!!!!!!",             // garbage
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): expected error", bad)
		}
	}
}

func TestFromPartsRange(t *testing.T) {
	if _, err := FromParts(-1, 0); !errors.Is(err, ErrRange) {
		t.Errorf("negative micros: err = %v", err)
	}
	if _, err := FromParts(1, 1024); !errors.Is(err, ErrRange) {
		t.Errorf("clockID 1024: err = %v", err)
	}
	if _, err := FromParts(1<<63-1, 0); !errors.Is(err, ErrRange) {
		t.Errorf("micros overflow: err = %v", err)
	}
}

func TestConcurrentNextIsMonotonic(t *testing.T) {
	const workers, per = 8, 500
	results := make(chan uint64, workers*per)
	var spawn sync.WaitGroup
	for range workers {
		spawn.Add(1)
		go func() {
			defer spawn.Done()
			prev := uint64(0)
			for range per {
				v := uint64(Next().Integer())
				// Each worker observes strictly increasing values: the
				// generator holds a lock across generation.
				if v <= prev {
					t.Errorf("worker saw non-increasing TID: %d then %d", prev, v)
					return
				}
				prev = v
				results <- v
			}
		}()
	}
	go func() { spawn.Wait(); close(results) }()

	// Arrival order across workers says nothing about generation order, so
	// check the global property that matters: every handed-out TID is unique.
	seen := make(map[uint64]bool, workers*per)
	for v := range results {
		if seen[v] {
			t.Fatalf("duplicate TID handed out: %d", v)
		}
		seen[v] = true
	}
	if len(seen) != workers*per {
		t.Fatalf("got %d unique TIDs, want %d", len(seen), workers*per)
	}
}
