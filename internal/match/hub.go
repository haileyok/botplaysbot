package match

// Hub is the per-DID notification fanout behind match.subscribe (spec
// §5.5a): the pairing loop publishes #matched, challenge creation publishes
// #challengeReceived, and every live match.subscribe connection for the
// target DID receives the event. Publish is drop-not-block — exactly the
// game event bus's semantics (internal/events): a subscriber that stops
// draining loses events rather than stalling the publisher, because a
// stalled WS client must never stall the pairing loop or an accepting
// request. The per-connection buffer (HubBufferSize) plus the drop counter
// is the backpressure signal; agent SDKs that care re-fetch state.

import (
	"sync"
	"time"
)

// HubBufferSize is the per-connection event buffer.
const HubBufferSize = 64

// Event kinds carried on a match.subscribe connection.
const (
	KindMatched           = "matched"
	KindChallengeReceived = "challengeReceived"
)

// MatchedEvent is a #matched notification (spec §5.5a).
type MatchedEvent struct {
	SeekID   string
	Game     string
	Seat     string // the notified agent's engine seat
	Opponent string
}

// ChallengeEvent is a #challengeReceived notification (spec §5.5a).
type ChallengeEvent struct {
	ChallengeID     string
	Challenger      string
	GameType        string
	Variant         string
	TimeControlJSON []byte // bot.plays.bot.game#timeControl JSON
	Rated           bool
	ExpiresAt       time.Time
}

// HubEvent is one notification. Kind selects the payload.
type HubEvent struct {
	Kind      string
	Matched   MatchedEvent
	Challenge ChallengeEvent
}

// Conn is one subscriber's channel.
type Conn struct {
	C <-chan HubEvent

	id  uint64
	ch  chan HubEvent
	hub *Hub
	did string

	mu      sync.Mutex
	dropped int
}

// Dropped reports how many events were discarded because this connection
// stopped draining (the hub never blocks a publisher).
func (c *Conn) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Hub fans events out to per-DID connections. Safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	nextID   uint64
	conns    map[string]map[uint64]*Conn
	presence *Presence
}

// NewHub returns an empty hub bound to the given presence registry
// (connections feed it: Connect/Disconnect stamp lastSeen).
func NewHub(presence *Presence) *Hub {
	return &Hub{conns: map[string]map[uint64]*Conn{}, presence: presence}
}

// Connect registers a new connection for did, marking them present.
func (h *Hub) Connect(did string) *Conn {
	ch := make(chan HubEvent, HubBufferSize)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	c := &Conn{C: ch, ch: ch, id: h.nextID, hub: h, did: did}
	if h.conns[did] == nil {
		h.conns[did] = map[uint64]*Conn{}
	}
	h.conns[did][c.id] = c
	h.presence.Connect(did, time.Now())
	return c
}

// Disconnect removes a connection, stamping presence. The channel is not
// closed: publishers never write to a removed connection, and the handler
// owns its channel's lifecycle (it simply stops ranging when its request
// context ends).
func (h *Hub) Disconnect(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conns := h.conns[c.did]; conns != nil {
		delete(conns, c.id)
		if len(conns) == 0 {
			delete(h.conns, c.did)
		}
	}
	h.presence.Disconnect(c.did, time.Now())
}

// Publish delivers ev to every live connection of did. Never blocks.
func (h *Hub) Publish(did string, ev HubEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns[did] {
		select {
		case c.ch <- ev:
		default:
			c.mu.Lock()
			c.dropped++
			c.mu.Unlock()
		}
	}
}

// Presence exposes the registry the hub feeds.
func (h *Hub) Presence() *Presence { return h.presence }
