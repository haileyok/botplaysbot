// Package clock implements plays.bot game clocks per spec §7.
//
// The AppView is the clock: every deadline derives from AppView receipt time,
// never agent-reported or repo-commit time. Only the perMove time control is
// implemented; fischer and correspondence are modeled but rejected at runtime.
package clock

import (
	"errors"
	"fmt"
	"time"
)

// Kind enumerates supported time control kinds (spec §4.4 timeControl).
type Kind string

const (
	KindPerMove        Kind = "perMove"
	KindFischer        Kind = "fischer"
	KindCorrespondence Kind = "correspondence"
)

// TimeControl mirrors the lexicon bot.plays.bot.game#timeControl object.
type TimeControl struct {
	Kind             Kind  `json:"kind"`
	PerMoveSeconds   int64 `json:"perMoveSeconds,omitempty"`
	InitialSeconds   int64 `json:"initialSeconds,omitempty"`
	IncrementSeconds int64 `json:"incrementSeconds,omitempty"`
}

// ErrNotImplemented is returned for time control kinds the v1 AppView does
// not implement (spec §7: perMove only).
var ErrNotImplemented = errors.New("clock: time control not implemented")

// ErrInvalid describes a TimeControl that cannot produce a deadline (zero or
// negative budget, or an unknown kind).
var ErrInvalid = errors.New("clock: invalid time control")

// validate kind-specific budget requirements.
func (tc TimeControl) budget() (time.Duration, error) {
	switch tc.Kind {
	case KindPerMove:
		if tc.PerMoveSeconds <= 0 {
			return 0, fmt.Errorf("%w: perMove requires positive PerMoveSeconds, got %d", ErrInvalid, tc.PerMoveSeconds)
		}
		return time.Duration(tc.PerMoveSeconds) * time.Second, nil
	case KindFischer, KindCorrespondence:
		return 0, fmt.Errorf("%w: %s", ErrNotImplemented, tc.Kind)
	case "":
		return 0, fmt.Errorf("%w: missing kind", ErrInvalid)
	default:
		return 0, fmt.Errorf("%w: unknown kind %q", ErrInvalid, tc.Kind)
	}
}

// DeadlineFor returns the moment the on-the-move player's clock expires.
//
// For perMove, the deadline is previousMove.receivedAt + PerMoveSeconds
// (spec §7). The caller passes the AppView receipt time of the previous
// accepted move — or, for the first move of a game, the receipt time of the
// event that started the clock (challenge acceptance or match grace end).
func DeadlineFor(tc TimeControl, lastReceivedAt time.Time) (time.Time, error) {
	d, err := tc.budget()
	if err != nil {
		return time.Time{}, err
	}
	if lastReceivedAt.IsZero() {
		return time.Time{}, fmt.Errorf("%w: zero lastReceivedAt", ErrInvalid)
	}
	return lastReceivedAt.Add(d), nil
}

// RemainingMs returns the milliseconds left on the clock at now, floored at
// zero. A result of 0 means the deadline has passed: the next received move is
// rejected with ClockExpired and the game finishes by timeout (spec §7).
func RemainingMs(tc TimeControl, lastReceivedAt, now time.Time) (int64, error) {
	deadline, err := DeadlineFor(tc, lastReceivedAt)
	if err != nil {
		return 0, err
	}
	left := deadline.Sub(now)
	if left < 0 {
		return 0, nil
	}
	return left.Milliseconds(), nil
}
