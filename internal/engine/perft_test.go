package engine_test

import (
	"testing"

	"github.com/haileyok/botplaysbot/internal/engine"
)

// TestPerft verifies move generation against published perft counts:
// startpos depth 1/2/3 = 20/400/8902; kiwipete (the standard
// castling/en-passant/pin-rich perft position) depth 1/2 = 48/2039. Counting
// goes through the engine interface itself (LegalMoves + Apply), not the
// library directly.
func TestPerft(t *testing.T) {
	cases := []struct {
		name   string
		fen    string // "" = standard start
		depth  int
		expect int64
	}{
		{"startpos-1", "", 1, 20},
		{"startpos-2", "", 2, 400},
		{"startpos-3", "", 3, 8902},
		{"kiwipete-1", "r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq - 0 1", 1, 48},
		{"kiwipete-2", "r3k2r/p1ppqpb1/bn2pnp1/3PN3/1p2P3/2N2Q1p/PPPBBPPP/R3K2R w KQkq - 0 1", 2, 2039},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.NewChess()
			var st engine.State
			var err error
			if tc.fen == "" {
				st, err = eng.InitialState(engine.ChessVariantStandard, nil)
			} else {
				st, err = eng.StateFromFEN(tc.fen)
			}
			if err != nil {
				t.Fatalf("initial state: %v", err)
			}
			if got := perft(eng, st, tc.depth); got != tc.expect {
				t.Fatalf("perft(%s) = %d, want %d", tc.name, got, tc.expect)
			}
		})
	}
}

func perft(eng *engine.Chess, st engine.State, depth int) int64 {
	moves, err := eng.LegalMoves(st)
	if err != nil {
		panic(err)
	}
	if depth == 1 {
		return int64(len(moves))
	}
	var nodes int64
	for _, m := range moves {
		next, err := eng.Apply(st, m)
		if err != nil {
			panic(err)
		}
		nodes += perft(eng, next, depth-1)
	}
	return nodes
}
