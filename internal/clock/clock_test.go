package clock

import (
	"errors"
	"testing"
	"time"
)

var perMove300 = TimeControl{Kind: KindPerMove, PerMoveSeconds: 300}

func TestDeadlineForPerMove(t *testing.T) {
	// Spec §7: deadline = previousMove.receivedAt + perMoveSeconds.
	last := time.Date(2026, 9, 8, 14, 3, 22, 115_000_000, time.UTC)
	deadline, err := DeadlineFor(perMove300, last)
	if err != nil {
		t.Fatalf("DeadlineFor: %v", err)
	}
	want := last.Add(300 * time.Second)
	if !deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v", deadline, want)
	}
}

func TestRemainingMs(t *testing.T) {
	last := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)

	at30s, err := RemainingMs(perMove300, last, last.Add(30*time.Second))
	if err != nil {
		t.Fatalf("RemainingMs: %v", err)
	}
	if at30s != 270_000 {
		t.Errorf("remaining after 30s = %d, want 270000", at30s)
	}

	// Exactly at the deadline there is nothing left.
	atDeadline, err := RemainingMs(perMove300, last, last.Add(300*time.Second))
	if err != nil {
		t.Fatalf("RemainingMs: %v", err)
	}
	if atDeadline != 0 {
		t.Errorf("remaining at deadline = %d, want 0", atDeadline)
	}

	// Past the deadline stays floored at zero, never negative.
	late, err := RemainingMs(perMove300, last, last.Add(301*time.Second))
	if err != nil {
		t.Fatalf("RemainingMs: %v", err)
	}
	if late != 0 {
		t.Errorf("remaining past deadline = %d, want 0", late)
	}
}

func TestNotImplementedKinds(t *testing.T) {
	// Spec §7: fischer and correspondence are supported by the data model but
	// not implemented in v1. The type accepts them; evaluation rejects them.
	for _, tc := range []TimeControl{
		{Kind: KindFischer, InitialSeconds: 180, IncrementSeconds: 2},
		{Kind: KindCorrespondence},
	} {
		if _, err := DeadlineFor(tc, time.Now()); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("DeadlineFor(%s): err = %v, want ErrNotImplemented", tc.Kind, err)
		}
		if _, err := RemainingMs(tc, time.Now(), time.Now()); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("RemainingMs(%s): err = %v, want ErrNotImplemented", tc.Kind, err)
		}
	}
}

func TestInvalidTimeControls(t *testing.T) {
	for _, tc := range []TimeControl{
		{Kind: KindPerMove, PerMoveSeconds: 0},
		{Kind: KindPerMove, PerMoveSeconds: -5},
		{Kind: "bunnyhop", PerMoveSeconds: 300},
		{},
	} {
		if _, err := DeadlineFor(tc, time.Now()); !errors.Is(err, ErrInvalid) {
			t.Errorf("DeadlineFor(%+v): err = %v, want ErrInvalid", tc, err)
		}
	}

	if _, err := DeadlineFor(perMove300, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("DeadlineFor with zero lastReceivedAt: err = %v, want ErrInvalid", err)
	}
}
