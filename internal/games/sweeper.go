package games

import (
	"context"
	"errors"
	"time"
)

// StartSweeper runs the clock sweeper until Close (spec §7): every interval,
// active games past their deadline are finished by timeout (same code path
// as a rejecting SubmitMove) so an absent opponent does not need to poll.
// A non-positive interval falls back to the spec default of one second so a
// partially-populated Config can never panic the boot path.
func (m *Manager) StartSweeper(interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	m.sweepStop = make(chan struct{})
	m.sweepDone = make(chan struct{})
	go func() {
		defer close(m.sweepDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.sweepStop:
				return
			case <-ticker.C:
				if _, err := m.SweepOnce(context.Background()); err != nil {
					m.logger.Error("games: sweeper pass failed", "err", err)
				}
			}
		}
	}()
}

// SweepOnce finishes every active game whose deadline passed at sweep time,
// returning how many games ended.
func (m *Manager) SweepOnce(ctx context.Context) (int, error) {
	now := m.now()
	expired, err := m.repos.Games.ListExpired(ctx, now, 100)
	if err != nil {
		return 0, err
	}
	finished := 0
	for _, g := range expired {
		_, st, _, players, err := m.loadGame(ctx, g)
		if err != nil {
			m.logger.Error("games: sweeper load failed", "game", g.URI, "err", err)
			continue
		}
		result := m.timeoutResult(g, players)
		if err := m.finishTimeout(ctx, g, st, result, now); err != nil {
			if IsCode(err, CodeGameNotActive) {
				continue // finished concurrently; nothing to do
			}
			m.logger.Error("games: sweeper finish failed", "game", g.URI, "err", err)
			continue
		}
		finished++
	}
	return finished, nil
}

// IsCode reports whether err is a manager *Error with the given code.
func IsCode(err error, code string) bool {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Code == code
	}
	return false
}
