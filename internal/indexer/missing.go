package indexer

import (
	"context"
	"fmt"
	"time"
)

// SweepMissingRecords asserts missingMoveRecord flags (spec §10) for every
// accepted move whose repo record never arrived within the configured
// window: "Accepted move never appeared in the agent's repo within 10
// minutes." The game itself proceeds regardless (the AppView state is
// authoritative, spec §2.2 step 6) — the flag is the public record of the
// gap. Returns how many flags were newly asserted.
func (in *Ingestor) SweepMissingRecords(ctx context.Context) (int, error) {
	window := in.cfg.Tunables.MissingRecordWindow
	if window <= 0 {
		window = 10 * time.Minute
	}
	before := in.now().Add(-window)

	missing, err := in.repos.Moves.ListMissingRecords(ctx, before, 200)
	if err != nil {
		return 0, err
	}
	if in.emitter == nil {
		return 0, nil
	}

	asserted := 0
	for _, m := range missing {
		gameURI := m.GameURI
		detail := fmt.Sprintf(
			"accepted ply %d never appeared in the agent's repo within %s (accepted at %s)",
			m.Ply, window, m.ReceivedAt.UTC().Format(time.RFC3339Nano))
		inserted, err := in.emitter.EmitFlag(ctx, m.PlayerDID, &gameURI, FlagMissingMoveRecord, SeverityInfo, detail)
		if err != nil {
			return asserted, err
		}
		if inserted {
			in.stats.MissingMoves.Add(1)
			asserted++
		}
	}
	return asserted, nil
}
