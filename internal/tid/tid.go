// Package tid generates and parses ATProto TIDs (record keys).
//
// A TID is a 64-bit value: the high 54 bits are microseconds since the Unix
// epoch, the low 10 bits are a clock identifier. It is serialized as 13
// characters of the base32-sortable alphabet (atmos.Base32SortAlphabet), most
// significant first. This package delegates the wire codec to
// github.com/jcalabro/atmos and adds the process-local generator: calls never
// go backwards, and collisions within the same microsecond bump the clock ID.
package tid

import (
	"errors"
	"sync"
	"time"

	"github.com/jcalabro/atmos"
)

// TID is a syntactically valid ATProto TID.
type TID = atmos.TID

// ErrRange is returned for inputs that cannot form a TID.
var ErrRange = errors.New("tid: value out of range")

// Parse decodes a 13-character TID string.
func Parse(s string) (TID, error) {
	return atmos.ParseTID(s)
}

// FromParts builds a TID from microseconds since the Unix epoch and a clock
// ID in [0, 1024). Returns ErrRange for out-of-range inputs.
func FromParts(unixMicros int64, clockID uint) (TID, error) {
	if unixMicros < 0 || clockID >= 1024 || unixMicros > maxMicros {
		return "", ErrRange
	}
	return atmos.NewTID(unixMicros, clockID), nil
}

// maxMicros is the largest timestamp a TID can carry (54 bits).
const maxMicros = int64(1)<<(63-10) - 1

// generator hands out strictly increasing TIDs for one process.
type generator struct {
	mu           sync.Mutex
	clockID      uint
	lastMicros   int64
	lastClockSeq uint
}

var defaultGenerator = &generator{}

// Next returns a new TID from the package generator, strictly increasing
// across calls within the process: calls landing in the same microsecond as
// the previous call bump the 10-bit clock ID; if the clock ID wraps, the
// microsecond is advanced instead, so the encoded 64-bit value (and therefore
// the 13-character string) always sorts after its predecessor.
func Next() TID {
	return defaultGenerator.Next()
}

// Next returns a new TID from this generator with the monotonicity contract
// documented on the package-level Next.
func (g *generator) Next() TID {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := nowMicros()
	switch {
	case now > g.lastMicros:
		// Fresh microsecond: restart the collision counter.
		g.lastMicros = now
		g.lastClockSeq = 0
	case g.lastClockSeq < 1023:
		// Same microsecond as the previous TID: bump the clock ID.
		g.lastClockSeq++
	default:
		// Clock ID exhausted within this microsecond (1024 TIDs in one µs):
		// advance time one microsecond to keep strict ordering.
		g.lastMicros++
		g.lastClockSeq = 0
		now = g.lastMicros
	}

	clockID := (g.clockID + g.lastClockSeq) % 1024
	return atmos.NewTID(g.lastMicros, clockID)
}

// nowMicros is a hook for tests.
var nowMicros = func() int64 { return time.Now().UnixMicro() }
