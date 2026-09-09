// Package engine defines the pluggable game engine interface (spec §6) and
// hosts the chess engine.
//
// An engine owns legality, state transitions, and terminal detection for one
// game type. Engines must be deterministic and pure: no globals, no wall
// clock, no RNG without the seed the AppView supplies (spec §6: "the engine
// must be deterministic and pure").
//
// Payloads and positions are lexicon union members: JSON objects with a
// $type discriminator (e.g. bot.plays.bot.chess.move), represented here as
// raw JSON so one interface serves every game (spec §4.2 open unions).
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Payload is a game-specific move payload: a lexicon union member object as
// canonical JSON, including its $type field.
type Payload = json.RawMessage

// Position is a public position snapshot: a lexicon union member object
// (e.g. bot.plays.bot.chess.position with {fen, check}).
type Position = json.RawMessage

// State is engine-owned game state. Implementations keep it immutable:
// Apply returns a new State and never mutates the input. States are opaque
// to callers; engines that can persist themselves also implement Snapshotter.
type State interface {
	engineState()
}

// Terminal is the result of a finished game (spec §6 terminal).
type Terminal struct {
	// Outcome is "win" or "draw".
	Outcome string
	// WinnerSeat is the winning seat for Outcome "win", else "".
	WinnerSeat string
	// Reason is a spec §4.4 result.reason value: one of checkmate, stalemate,
	// resignation, timeout, agreement, repetition, fiftyMove,
	// insufficientMaterial, noMoves, abandonment, adjudication. Engines only
	// produce game-mechanical reasons (e.g. chess: checkmate, stalemate,
	// repetition, fiftyMove, insufficientMaterial); resignation/timeout/
	// agreement are AppView decisions, not engine ones.
	Reason string
}

// Errors returned by engines. The games layer maps these to XRPC error names.
var (
	// ErrUnknownVariant is returned for a variant the engine does not support.
	ErrUnknownVariant = errors.New("engine: unknown variant")
	// ErrUnknownNSID is returned by registry lookups for an unregistered
	// game type.
	ErrUnknownNSID = errors.New("engine: no engine for game type")
)

// MalformedError reports a payload that violates the game's payload schema:
// wrong shape, missing or out-of-enum fields. Mapped to XRPC MalformedPayload.
type MalformedError struct{ Detail string }

func (e *MalformedError) Error() string { return "malformed payload: " + e.Detail }

// Malformed builds a MalformedError.
func Malformed(detail string) *MalformedError { return &MalformedError{Detail: detail} }

// IllegalError reports a well-formed payload that is not legal in the
// current state (or not legal for the given seat). Mapped to XRPC
// IllegalMove with the detail surfaced to the caller.
type IllegalError struct{ Detail string }

func (e *IllegalError) Error() string { return "illegal move: " + e.Detail }

// Illegal builds an IllegalError.
func Illegal(detail string) *IllegalError { return &IllegalError{Detail: detail} }

// GameEngine implements one game (spec §6, Go-ported).
type GameEngine interface {
	// NSID is the game type: the NSID of the game's move payload object
	// (e.g. "bot.plays.bot.chess.move", spec §4.2).
	NSID() string
	// Variants lists supported variants (e.g. ["standard"]).
	Variants() []string
	// Seats lists the seats for a variant, in move order (e.g. white, black).
	Seats(variant string) ([]string, error)
	// InitialState builds the state a game starts in. seed is optional
	// engine-supplied randomness recorded on the game and published at game
	// end (spec §6); games that need none ignore it.
	InitialState(variant string, seed []byte) (State, error)
	// CurrentSeat returns the seat to move in state.
	CurrentSeat(state State) (string, error)
	// Validate checks that payload is well-formed and legal for seat in
	// state. Shape violations return *MalformedError; legality violations
	// return *IllegalError.
	Validate(state State, seat string, payload Payload) error
	// Apply applies a legal payload and returns the new state. Callers must
	// Validate first; Apply re-checks and errors on anything illegal.
	Apply(state State, payload Payload) (State, error)
	// LegalMoves returns every legal payload in state (the AppView's
	// authoritative convenience list, spec §5.2).
	LegalMoves(state State) ([]Payload, error)
	// Terminal returns the game's terminal state, or nil when the game is
	// still in progress. Once Terminal is non-nil the state is final.
	Terminal(state State) (*Terminal, error)
	// Position returns the public position snapshot of state.
	Position(state State) (Position, error)
	// CanonicalNotation returns the notation (SAN for chess) the AppView
	// derives for payload in state. Engines without notation return "".
	CanonicalNotation(state State, payload Payload) (string, error)
	// CommentaryDelayUnit is the natural commentary-delay unit for the game
	// ("ply" for chess, spec §6).
	CommentaryDelayUnit() string
}

// Snapshotter is implemented by engines that can serialize state to a JSON
// snapshot (stored in games.state) and restore State from one. The snapshot
// is a quick-load cache; correctness-critical paths may instead rehydrate by
// replaying accepted payloads through InitialState/Apply.
type Snapshotter interface {
	// Snapshot serializes state for the games.state column.
	Snapshot(state State) (json.RawMessage, error)
	// Restore rebuilds a State from a Snapshot payload.
	Restore(raw json.RawMessage) (State, error)
}

// Replay reconstructs state by applying payloads in order from the initial
// state (spec: the engine is pure, so replay is deterministic). Used to
// rehydrate games where history matters (repetition counters, halfmove
// clocks) rather than inventing a lossy state serialization.
func Replay(e GameEngine, variant string, seed []byte, payloads []Payload) (State, error) {
	st, err := e.InitialState(variant, seed)
	if err != nil {
		return nil, err
	}
	for i, p := range payloads {
		st, err = e.Apply(st, p)
		if err != nil {
			return nil, fmt.Errorf("engine: replay payload %d: %w", i+1, err)
		}
	}
	return st, nil
}

// Registry resolves game type NSIDs to engines.
type Registry map[string]GameEngine

// NewRegistry indexes engines by NSID.
func NewRegistry(engines ...GameEngine) Registry {
	r := Registry{}
	for _, e := range engines {
		r[e.NSID()] = e
	}
	return r
}

// Get returns the engine for a game type NSID.
func (r Registry) Get(nsid string) (GameEngine, error) {
	e, ok := r[nsid]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownNSID, nsid)
	}
	return e, nil
}

// CanonicalJSON normalizes a payload/position to a canonical byte form for
// equality comparison: decode as generic JSON, re-encode with sorted keys.
// Used by idempotency checks that must treat re-serialized equal objects as
// equal.
func CanonicalJSON(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
