package engine_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/haileyok/botplaysbot/internal/engine"
)

// payload builds a chess move payload as the XRPC layer would deliver it.
func payload(t *testing.T, from, to, promotion string) engine.Payload {
	t.Helper()
	p := map[string]string{
		"$type": engine.NSIDChessMove,
		"from":  from,
		"to":    to,
	}
	if promotion != "" {
		p["promotion"] = promotion
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// applySeq applies from/to payloads through the engine and fails the test on
// the first rejection.
func applySeq(t *testing.T, eng *engine.Chess, st engine.State, seq ...[2]string) engine.State {
	t.Helper()
	var err error
	for _, m := range seq {
		st, err = eng.Apply(st, payload(t, m[0], m[1], ""))
		if err != nil {
			t.Fatalf("apply %s%s: %v", m[0], m[1], err)
		}
	}
	return st
}

// applySeqP applies payloads including promotions.
func applySeqP(t *testing.T, eng *engine.Chess, st engine.State, seq ...[3]string) engine.State {
	t.Helper()
	var err error
	for _, m := range seq {
		st, err = eng.Apply(st, payload(t, m[0], m[1], m[2]))
		if err != nil {
			t.Fatalf("apply %s%s%s: %v", m[0], m[1], m[2], err)
		}
	}
	return st
}

func terminalOf(t *testing.T, eng *engine.Chess, st engine.State) *engine.Terminal {
	t.Helper()
	term, err := eng.Terminal(st)
	if err != nil {
		t.Fatalf("terminal: %v", err)
	}
	return term
}

// TestTerminalCheckmate: fool's mate — 1. f3 e5 2. g4 Qh4#.
func TestTerminalCheckmate(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	st = applySeq(t, eng, st,
		[2]string{"f2", "f3"}, [2]string{"e7", "e5"},
		[2]string{"g2", "g4"}, [2]string{"d8", "h4"})

	term := terminalOf(t, eng, st)
	if term == nil {
		t.Fatal("fool's mate not detected as terminal")
	}
	if term.Outcome != "win" || term.WinnerSeat != "black" || term.Reason != "checkmate" {
		t.Fatalf("terminal = %+v, want win/black/checkmate", term)
	}
	seat, err := eng.CurrentSeat(st)
	if err != nil || seat != "white" {
		t.Fatalf("seat after mate = %q, %v; want white (the mated side)", seat, err)
	}
}

// TestTerminalStalemate: the classic quickest stalemate (Sam Loyd):
// 1.e3 a5 2.Qh5 Ra6 3.Qxa5 h5 4.Qxc7 Rah6 5.h4 f6 6.Qxd7+ Kf7 7.Qxb7 Qd3
// 8.Qxb8 Qh7 9.Qxc8 Kg6 10.Qe6 — stalemate, black to move.
func TestTerminalStalemate(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	st = applySeqP(t, eng, st,
		[3]string{"e2", "e3", ""}, [3]string{"a7", "a5", ""},
		[3]string{"d1", "h5", ""}, [3]string{"a8", "a6", ""},
		[3]string{"h5", "a5", ""}, [3]string{"h7", "h5", ""},
		[3]string{"a5", "c7", ""}, [3]string{"a6", "h6", ""},
		[3]string{"h2", "h4", ""}, [3]string{"f7", "f6", ""},
		[3]string{"c7", "d7", ""}, [3]string{"e8", "f7", ""},
		[3]string{"d7", "b7", ""}, [3]string{"d8", "d3", ""},
		[3]string{"b7", "b8", ""}, [3]string{"d3", "h7", ""},
		[3]string{"b8", "c8", ""}, [3]string{"f7", "g6", ""},
		[3]string{"c8", "e6", ""},
	)

	term := terminalOf(t, eng, st)
	if term == nil {
		t.Fatal("stalemate not detected as terminal")
	}
	if term.Outcome != "draw" || term.Reason != "stalemate" {
		t.Fatalf("terminal = %+v, want draw/stalemate", term)
	}
}

// TestTerminalInsufficientMaterial: K+R vs K reduced to bare kings is a
// dead position; the library declares insufficient material immediately.
func TestTerminalInsufficientMaterial(t *testing.T) {
	eng := engine.NewChess()
	st, err := eng.StateFromFEN("8/8/8/4k3/8/8/4K3/4R3 w - - 0 1")
	if err != nil {
		t.Fatal(err)
	}
	// Not terminal while the rook lives: 1.Ra1 (any rook move keeps material).
	if term := terminalOf(t, eng, st); term != nil {
		t.Fatalf("rook on board reported terminal: %+v", term)
	}
	// Trade into bare kings via a tiny K+R v K line: white rook drops itself
	// for nothing? No — instead start from bare kings.
	st2, err := eng.StateFromFEN("8/8/8/4k3/8/8/4K3/8 w - - 0 1")
	if err != nil {
		t.Fatal(err)
	}
	term := terminalOf(t, eng, st2)
	if term == nil || term.Outcome != "draw" || term.Reason != "insufficientMaterial" {
		t.Fatalf("terminal = %+v, want draw/insufficientMaterial", term)
	}
}

// TestTerminalRepetition: knights shuffle twice back to the start position;
// the third occurrence draws automatically (spec §5.6, no claiming).
func TestTerminalRepetition(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	seq := [][2]string{
		{"g1", "f3"}, {"g8", "f6"},
		{"f3", "g1"}, {"f6", "g8"}, // start position, 2nd occurrence
		{"g1", "f3"}, {"g8", "f6"},
		{"f3", "g1"}, {"f6", "g8"}, // start position, 3rd occurrence
	}
	st = applySeq(t, eng, st, seq[:4]...)
	if term := terminalOf(t, eng, st); term != nil {
		t.Fatalf("2nd occurrence must not draw: %+v", term)
	}
	st = applySeq(t, eng, st, seq[4:]...)
	term := terminalOf(t, eng, st)
	if term == nil || term.Outcome != "draw" || term.Reason != "repetition" {
		t.Fatalf("terminal = %+v, want draw/repetition", term)
	}
}

// TestTerminalNotTerminalEarly: the opening position is in progress.
func TestTerminalNotTerminalEarly(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	if term := terminalOf(t, eng, st); term != nil {
		t.Fatalf("start position terminal = %+v, want nil", term)
	}
}

// TestPayloadEncoding covers castling as the king move, en passant as the
// pawn capture move, and the promotion enum (spec §4.11).
func TestPayloadEncoding(t *testing.T) {
	eng := engine.NewChess()

	t.Run("castling is the king move", func(t *testing.T) {
		st, err := eng.StateFromFEN("r3k2r/8/8/8/8/8/8/R3K2R w KQkq - 0 1")
		if err != nil {
			t.Fatal(err)
		}
		// e1g1 (O-O) and e1c1 (O-O-O) must be legal; also black's.
		st = applySeq(t, eng, st, [2]string{"e1", "g1"})
		pos, _ := eng.Position(st)
		var p struct {
			FEN string `json:"fen"`
		}
		if err := json.Unmarshal(pos, &p); err != nil {
			t.Fatal(err)
		}
		// King lands on g1, rook on f1 (spec: castling expressed e1→g1).
		if want := "r3k2r/8/8/8/8/8/8/R4RK1 b kq - 1 1"; p.FEN != want {
			t.Fatalf("after O-O fen = %q, want %q", p.FEN, want)
		}
		st = applySeq(t, eng, st, [2]string{"e8", "c8"})
		pos, _ = eng.Position(st)
		if err := json.Unmarshal(pos, &p); err != nil {
			t.Fatal(err)
		}
		if want := "2kr3r/8/8/8/8/8/8/R4RK1 w - - 2 2"; p.FEN != want {
			t.Fatalf("after O-O-O fen = %q, want %q", p.FEN, want)
		}
	})

	t.Run("en passant is the pawn move", func(t *testing.T) {
		st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
		// 1.e4 a6 2.e5 d5 sets up an ep capture on d6.
		st = applySeq(t, eng, st,
			[2]string{"e2", "e4"}, [2]string{"a7", "a6"},
			[2]string{"e4", "e5"}, [2]string{"d7", "d5"})
		// White plays e5xd6 en passant: a plain pawn move from/to.
		st = applySeq(t, eng, st, [2]string{"e5", "d6"})
		pos, _ := eng.Position(st)
		var p struct {
			FEN string `json:"fen"`
		}
		if err := json.Unmarshal(pos, &p); err != nil {
			t.Fatal(err)
		}
		// Black d5 pawn must be gone; the b8 knight never moved.
		if want := "rnbqkbnr/1pp1pppp/p2P4/8/8/8/PPPP1PPP/RNBQKBNR b KQkq - 0 3"; p.FEN != want {
			t.Fatalf("after en passant fen = %q, want %q", p.FEN, want)
		}
	})

	t.Run("promotion enum", func(t *testing.T) {
		st, err := eng.StateFromFEN("8/P6k/8/8/8/8/8/K7 w - - 0 1")
		if err != nil {
			t.Fatal(err)
		}
		st = applySeqP(t, eng, st, [3]string{"a7", "a8", "n"})
		pos, _ := eng.Position(st)
		var p struct {
			FEN string `json:"fen"`
		}
		if err := json.Unmarshal(pos, &p); err != nil {
			t.Fatal(err)
		}
		if want := "N7/7k/8/8/8/8/8/K7 b - - 0 1"; p.FEN != want {
			t.Fatalf("after underpromotion fen = %q, want %q", p.FEN, want)
		}

		// Missing promotion on a promotion move: IllegalMove with detail.
		_, err = eng.Apply(st, payload(t, "a8", "a9", "")) // bad square first
		if err == nil {
			t.Fatal("a9 accepted")
		}
		st, _ = eng.StateFromFEN("8/P6k/8/8/8/8/8/K7 w - - 0 1")
		_, err = eng.Apply(st, payload(t, "a7", "a8", ""))
		if !isIllegal(t, err) {
			t.Fatalf("missing promotion err = %v, want IllegalError mentioning promotion", err)
		}
		if !strings.Contains(err.Error(), "promotion") {
			t.Fatalf("missing promotion detail = %q, want promotion hint", err)
		}
	})

	t.Run("legalMoves carry from/to/promotion", func(t *testing.T) {
		st, err := eng.StateFromFEN("8/P6k/8/8/8/8/8/K7 w - - 0 1")
		if err != nil {
			t.Fatal(err)
		}
		moves, err := eng.LegalMoves(st)
		if err != nil {
			t.Fatal(err)
		}
		var promos []string
		for _, m := range moves {
			var p struct {
				From      string `json:"from"`
				To        string `json:"to"`
				Promotion string `json:"promotion"`
			}
			if err := json.Unmarshal(m, &p); err != nil {
				t.Fatal(err)
			}
			if p.From == "a7" && p.To == "a8" {
				promos = append(promos, p.Promotion)
			}
		}
		if len(promos) != 4 {
			t.Fatalf("promotion variants = %v, want q/r/b/n", promos)
		}
	})
}

// TestSAN: the engine derives canonical SAN; the payload's san field is
// informational (spec §4.11).
func TestSAN(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)

	// SAN basics: from the position after 1.e4, Nf3 and Bc4 are natural.
	eng2 := engine.NewChess()
	st2, _ := eng2.InitialState(engine.ChessVariantStandard, nil)
	st2 = applySeq(t, eng2, st2, [2]string{"e2", "e4"})
	cases := []struct {
		from, to, promo, want string
	}{
		{"g8", "f6", "", "Nf6"},
		{"b8", "c6", "", "Nc6"},
	}
	for _, tc := range cases {
		san, err := eng2.CanonicalNotation(st2, payload(t, tc.from, tc.to, tc.promo))
		if err != nil {
			t.Fatalf("notation %s%s: %v", tc.from, tc.to, err)
		}
		if san != tc.want {
			t.Fatalf("SAN(%s%s) = %q, want %q", tc.from, tc.to, san, tc.want)
		}
	}

	// Disambiguation and captures: knights on b1 and f3 can both reach d2;
	// a lone capture gets the x.
	st, err := eng.StateFromFEN("k7/8/8/8/3p4/5N2/8/KN6 w - - 0 1")
	if err != nil {
		t.Fatal(err)
	}
	san, err := eng.CanonicalNotation(st, payload(t, "b1", "d2", ""))
	if err != nil {
		t.Fatal(err)
	}
	if san != "Nbd2" {
		t.Fatalf("disambiguated SAN = %q, want Nbd2", san)
	}
	san, err = eng.CanonicalNotation(st, payload(t, "f3", "d4", ""))
	if err != nil {
		t.Fatal(err)
	}
	if san != "Nxd4" {
		t.Fatalf("capture SAN = %q, want Nxd4", san)
	}
}

// TestValidateShape: malformed payloads are distinct from illegal ones.
func TestValidateShape(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)

	cases := []struct {
		name    string
		payload string
		wantMal bool
	}{
		{"wrong $type", `{"$type":"bot.plays.bot.checkers.move","from":"e2","to":"e4"}`, true},
		{"missing from", `{"$type":"bot.plays.bot.chess.move","to":"e4"}`, true},
		{"bad square", `{"$type":"bot.plays.bot.chess.move","from":"z9","to":"e4"}`, true},
		{"bad promotion", `{"$type":"bot.plays.bot.chess.move","from":"e2","to":"e4","promotion":"x"}`, true},
		{"not json", `{`, true},
		{"legal move", `{"$type":"bot.plays.bot.chess.move","from":"e2","to":"e4"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := eng.Validate(st, "white", engine.Payload(tc.payload))
			if tc.wantMal && !isMalformed(t, err) {
				t.Fatalf("err = %v, want MalformedError", err)
			}
			if !tc.wantMal && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}

	// Wrong seat is illegal, not malformed.
	err := eng.Validate(st, "black", payload(t, "e7", "e5", ""))
	if !isIllegal(t, err) {
		t.Fatalf("wrong-seat err = %v, want IllegalError", err)
	}
	// Illegal move carries detail.
	err = eng.Validate(st, "white", payload(t, "e2", "e5", ""))
	if !isIllegal(t, err) || err.Error() == "" {
		t.Fatalf("illegal err = %v, want IllegalError with detail", err)
	}
}

// TestSnapshotRestore: Snapshot → Restore reproduces the exact state,
// including repetition history.
func TestSnapshotRestore(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	st = applySeq(t, eng, st,
		[2]string{"g1", "f3"}, [2]string{"g8", "f6"},
		[2]string{"f3", "g1"}, [2]string{"f6", "g8"})

	snap, err := eng.Snapshot(st)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := eng.Restore(snap)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Apply(restored, payload(t, "g1", "f3", "")); err != nil {
		t.Fatalf("apply after restore: %v", err)
	}
	// Two repetitions before snapshot; two more after restore draw.
	st2 := applySeq(t, eng, restored,
		[2]string{"g1", "f3"}, [2]string{"g8", "f6"},
		[2]string{"f3", "g1"}, [2]string{"f6", "g8"})
	if term := terminalOf(t, eng, st2); term == nil || term.Reason != "repetition" {
		t.Fatalf("terminal after restore = %+v, want draw/repetition", term)
	}

	// Position round-trips identically.
	p1, _ := eng.Position(st)
	p2, _ := eng.Position(restored)
	if string(p1) != string(p2) {
		t.Fatalf("position changed across restore: %s vs %s", p1, p2)
	}
}

// TestReplay: engine.Replay rebuilds state from payloads (the moves-table
// rehydration path).
func TestReplay(t *testing.T) {
	eng := engine.NewChess()
	st, _ := eng.InitialState(engine.ChessVariantStandard, nil)
	st = applySeq(t, eng, st, [2]string{"e2", "e4"}, [2]string{"e7", "e5"})

	payloads := []engine.Payload{
		payload(t, "e2", "e4", ""),
		payload(t, "e7", "e5", ""),
	}
	rt, err := engine.Replay(eng, engine.ChessVariantStandard, nil, payloads)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := eng.Position(st)
	p2, _ := eng.Position(rt)
	if string(p1) != string(p2) {
		t.Fatalf("replayed position differs: %s vs %s", p1, p2)
	}
	if term := terminalOf(t, eng, rt); term != nil {
		t.Fatalf("replayed game terminal = %+v", term)
	}

	// Bad payload in the log is an error.
	if _, err := engine.Replay(eng, engine.ChessVariantStandard, nil,
		[]engine.Payload{payload(t, "e2", "e5", "")}); err == nil {
		t.Fatal("replay accepted an illegal move")
	}
}

// TestInterfaceDetails: NSID, variants, seats, delay unit, seed acceptance.
func TestInterfaceDetails(t *testing.T) {
	eng := engine.NewChess()
	if eng.NSID() != "bot.plays.bot.chess.move" {
		t.Fatalf("nsid = %q", eng.NSID())
	}
	if vs := eng.Variants(); len(vs) != 1 || vs[0] != "standard" {
		t.Fatalf("variants = %v", vs)
	}
	seats, err := eng.Seats("standard")
	if err != nil || len(seats) != 2 || seats[0] != "white" || seats[1] != "black" {
		t.Fatalf("seats = %v, %v", seats, err)
	}
	if _, err := eng.Seats("crazyhouse"); err == nil {
		t.Fatal("unknown variant accepted")
	}
	if _, err := eng.InitialState("crazyhouse", nil); err == nil {
		t.Fatal("unknown variant initial state accepted")
	}
	if unit := eng.CommentaryDelayUnit(); unit != "ply" {
		t.Fatalf("delay unit = %q, want ply", unit)
	}
	// Seed is accepted and deterministic for standard chess.
	a, _ := eng.InitialState("standard", []byte{1, 2, 3})
	b, _ := eng.InitialState("standard", []byte{9, 9, 9})
	pa, _ := eng.Position(a)
	pb, _ := eng.Position(b)
	if string(pa) != string(pb) {
		t.Fatal("standard chess start varies with seed")
	}
}

func isMalformed(t *testing.T, err error) bool {
	t.Helper()
	var me *engine.MalformedError
	return errors.As(err, &me)
}

func isIllegal(t *testing.T, err error) bool {
	t.Helper()
	var ie *engine.IllegalError
	return errors.As(err, &ie)
}
