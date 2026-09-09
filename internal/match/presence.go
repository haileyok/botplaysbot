package match

// Presence tracks which DIDs have a live match.subscribe connection
// (spec §9a.3: a standing seek expires when its DID's connection has been
// closed for more than PLAYSBOT_STANDING_EXPIRE).
//
// Storage choice: in-memory, did→lastSeen, single-process. The AppView owns
// match.subscribe terminations, so presence is process-local by nature; a
// restart simply reconnects agents (they redial the subscription), which
// refreshes presence. Phase E's multi-process work, if any, would move this
// to a shared store with the notification hub.

import (
	"sync"
	"time"
)

// Presence is the in-memory presence registry. Safe for concurrent use.
type Presence struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	conns    map[string]int
}

// NewPresence returns an empty registry.
func NewPresence() *Presence {
	return &Presence{lastSeen: map[string]time.Time{}, conns: map[string]int{}}
}

// Connect records that did opened a match.subscribe connection at now.
func (p *Presence) Connect(did string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[did]++
	p.lastSeen[did] = now
}

// Disconnect records that one of did's connections closed at now.
func (p *Presence) Disconnect(did string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[did] > 0 {
		p.conns[did]--
	}
	p.lastSeen[did] = now
}

// Online reports whether did has at least one live connection.
func (p *Presence) Online(did string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns[did] > 0
}

// GoneSince reports whether did has been offline since before the given
// cutoff: either offline with lastSeen earlier than cutoff, or never seen.
// The pairing loop expires standing seeks for DIDs that fail this check.
func (p *Presence) GoneSince(did string, cutoff time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns[did] > 0 {
		return false
	}
	seen, ok := p.lastSeen[did]
	if !ok {
		return true // never connected
	}
	return seen.Before(cutoff)
}
