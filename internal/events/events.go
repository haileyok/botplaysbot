// Package events is the in-memory game event bus.
//
// The games manager publishes lifecycle events here; subscribers observe
// them with an optional per-game and per-kind filter. Phase D attaches
// WebSocket subscription handlers (bot.plays.bot.game.subscribe) to a Bus;
// the API below is shaped for that: Subscribe returns a Subscription with a
// receive channel the WS handler drains into stream frames, and Close
// releases the slot. Publishing never blocks on a slow subscriber: a
// subscription whose buffer fills drops events (the drop counter on the
// Subscription is the backpressure signal) so a stalled WS client cannot
// stall a player's SubmitMove.
//
// Delivery is at-most-once and unordered across games; ordering is
// guaranteed only within a (game, kind) pair, which is all the spec's
// event stream promises.
package events

import (
	"encoding/json"
	"sync"
)

// Event kinds (spec §5.4 event names, minus the commentary kinds that a
// later phase adds).
const (
	KindGameStarted = "gameStarted"
	KindMove        = "move"
	KindDrawOffered = "drawOffered"
	KindGameFinished = "gameFinished"
)

// Clock is a per-player clock snapshot carried on move events.
type Clock struct {
	DID         string `json:"did"`
	RemainingMs int64  `json:"remainingMs"`
	Deadline    string `json:"deadline,omitempty"`
}

// Result is the terminal result carried on gameFinished events.
type Result struct {
	Outcome string `json:"outcome"`          // win | draw | aborted
	Winner  string `json:"winner,omitempty"` // DID, for wins
	Reason  string `json:"reason"`           // spec §4.4 result.reason
}

// Event is one published game event. Exactly one field is set.
type Event struct {
	Kind    string          `json:"-"` // one of the Kind* constants
	GameURI string          `json:"-"`

	// GameStarted: nothing beyond the game URI.

	// Move:
	Move struct {
		Ply      int64           `json:"ply"`
		Player   string          `json:"player"`
		Payload  json.RawMessage `json:"payload"`
		San      string          `json:"san,omitempty"`
		Position json.RawMessage `json:"position"`
		Clocks   []Clock         `json:"clocks"`
	} `json:"-"`

	// DrawOffered:
	OfferedBy string `json:"-"`

	// GameFinished:
	Result *Result `json:"-"`
}

// SubscribeOptions filter a subscription. Zero fields mean "all".
type SubscribeOptions struct {
	// GameURI restricts to one game; empty = every game (spectator firehose).
	GameURI string
	// Kinds restricts to these event kinds; empty = all kinds.
	Kinds []string
}

// Subscription is one subscriber's view of the bus.
type Subscription struct {
	// C receives events matching the subscription's filters.
	C <-chan Event

	id   uint64
	dropMu sync.Mutex
	dropped int
}

// Dropped reports how many events were discarded because the subscriber
// stopped draining C.
func (s *Subscription) Dropped() int {
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	return s.dropped
}

// Bus is an in-memory pub/sub hub. Safe for concurrent use.
type Bus struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]*busSub
}

type busSub struct {
	opts SubscribeOptions
	ch   chan Event
	sub  *Subscription
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[uint64]*busSub{}}
}

// SubscribeBufferSize is the per-subscription event buffer. Filled buffers
// drop rather than block publishers (see package comment).
const SubscribeBufferSize = 256

// Subscribe registers a filtered subscription. Cancel it with Close.
func (b *Bus) Subscribe(opts SubscribeOptions) *Subscription {
	ch := make(chan Event, SubscribeBufferSize)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID
	sub := &busSub{opts: opts, ch: ch, sub: &Subscription{C: ch, id: id}}
	b.subs[id] = sub
	return sub.sub
}

// Close cancels a subscription; the C channel is closed.
func (b *Bus) Close(s *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub, ok := b.subs[s.id]
	if !ok {
		return
	}
	delete(b.subs, s.id)
	close(sub.ch)
}

// Publish delivers ev to every matching subscriber. Never blocks.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		if !matches(s.opts, ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			s.sub.dropMu.Lock()
			s.sub.dropped++
			s.sub.dropMu.Unlock()
		}
	}
}

func matches(opts SubscribeOptions, ev Event) bool {
	if opts.GameURI != "" && opts.GameURI != ev.GameURI {
		return false
	}
	if len(opts.Kinds) == 0 {
		return true
	}
	for _, k := range opts.Kinds {
		if k == ev.Kind {
			return true
		}
	}
	return false
}
