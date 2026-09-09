package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/notnil/chess"
)

// NSIDChessMove is the game type NSID for chess (the payload object NSID,
// spec §4.2/§4.11).
const NSIDChessMove = "bot.plays.bot.chess.move"

// NSIDChessPosition is the chess position snapshot object NSID (spec §4.11).
const NSIDChessPosition = "bot.plays.bot.chess.position"

// ChessVariantStandard is the only chess variant in v1.
const ChessVariantStandard = "standard"

// maxHalfmoveClock is the fifty-move threshold: the halfmove clock reaching
// 100 (fifty full moves without a pawn move or capture) draws (spec §5.6:
// automatic draws, no claiming).
const maxHalfmoveClock = 100

// minRepetitions is the threefold threshold: the same position occurring
// three times draws (spec §5.6: automatic, no claiming).
const minRepetitions = 3

// Chess is the chess GameEngine on github.com/notnil/chess (v1.10.0). The
// library owns legality (including castling as the king move and en passant
// as the pawn move, spec §4.11), move generation, and material heuristics.
// The engine is pure: all methods are deterministic functions of their
// inputs; the seed parameter is accepted for interface compliance and
// ignored (standard chess has a fixed start).
type Chess struct{}

// NewChess returns the chess engine.
func NewChess() *Chess { return &Chess{} }

// chessState is the chess State: the live game plus a compact replay log
// (from/to/promotion strings) and the derived check flag. Immutable by
// convention: Apply clones before mutating.
type chessState struct {
	game  *chess.Game
	moves []string
	check bool // side-to-move is in check (true after start only if FEN says so)
}

func (s *chessState) engineState() {}

// NSID implements GameEngine.
func (c *Chess) NSID() string { return NSIDChessMove }

// Variants implements GameEngine.
func (c *Chess) Variants() []string { return []string{ChessVariantStandard} }

// Seats implements GameEngine.
func (c *Chess) Seats(variant string) ([]string, error) {
	if variant != ChessVariantStandard {
		return nil, fmt.Errorf("%w: %q", ErrUnknownVariant, variant)
	}
	return []string{"white", "black"}, nil
}

// InitialState implements GameEngine.
func (c *Chess) InitialState(variant string, _ []byte) (State, error) {
	if variant != ChessVariantStandard {
		return nil, fmt.Errorf("%w: %q", ErrUnknownVariant, variant)
	}
	return &chessState{game: chess.NewGame(), check: false}, nil
}

// StateFromFEN builds a state from a FEN (side to move and clocks come from
// the FEN). This is the boundary for non-standard start positions: test
// fixtures and future seeded variants (e.g. Chess960 start selection, spec
// §6) construct state here. The state has an empty move log, so repetition
// history is only as deep as the FEN position itself, and the check flag is
// false — production check flags are derived from the library's move tags
// during Apply, which fixture paths exercise when they apply moves.
func (c *Chess) StateFromFEN(fen string) (State, error) {
	opt, err := chess.FEN(fen)
	if err != nil {
		return nil, fmt.Errorf("engine: chess: bad FEN: %w", err)
	}
	return &chessState{game: chess.NewGame(opt)}, nil
}

// CurrentSeat implements GameEngine.
func (c *Chess) CurrentSeat(state State) (string, error) {
	s, err := asChessState(state)
	if err != nil {
		return "", err
	}
	return colorSeat(s.game.Position().Turn()), nil
}

func colorSeat(color chess.Color) string {
	if color == chess.Black {
		return "black"
	}
	return "white"
}

func seatColor(seat string) (chess.Color, error) {
	switch seat {
	case "white":
		return chess.White, nil
	case "black":
		return chess.Black, nil
	}
	return chess.White, fmt.Errorf("unknown chess seat %q", seat)
}

// chessPayload is the decoded bot.plays.bot.chess.move object (spec §4.11).
type chessPayload struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Promotion string `json:"promotion"`
	San       string `json:"san"` // informational only; server derives canonical SAN
}

// decodePayload parses and shape-checks a chess payload object.
func decodePayload(payload Payload) (*chessPayload, error) {
	var p chessPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, Malformed("payload is not a JSON object")
	}
	// $type: tolerate its absence only when the object otherwise parses? No —
	// the lexicon unions are $type-discriminated; require it (the XRPC layer
	// enforces this too, the engine is the backstop).
	var typed struct {
		Type string `json:"$type"`
	}
	if err := json.Unmarshal(payload, &typed); err != nil {
		return nil, Malformed("payload is not a JSON object")
	}
	if typed.Type != NSIDChessMove {
		return nil, Malformed(fmt.Sprintf("payload $type %q is not %s", typed.Type, NSIDChessMove))
	}
	if len(p.From) != 2 || !isSquare(p.From) {
		return nil, Malformed(fmt.Sprintf("from %q is not a square like e2", p.From))
	}
	if len(p.To) != 2 || !isSquare(p.To) {
		return nil, Malformed(fmt.Sprintf("to %q is not a square like e4", p.To))
	}
	switch p.Promotion {
	case "", "q", "r", "b", "n":
	default:
		return nil, Malformed(fmt.Sprintf("promotion %q is not one of q/r/b/n", p.Promotion))
	}
	return &p, nil
}

func isSquare(s string) bool {
	return s[0] >= 'a' && s[0] <= 'h' && s[1] >= '1' && s[1] <= '8'
}

// uci renders a payload as the library's move string (from+to+promo).
func (p *chessPayload) uci() string { return p.From + p.To + p.Promotion }

// matchMove finds the legal move with exactly this from/to/promotion.
func matchMove(s *chessState, p *chessPayload) (*chess.Move, error) {
	want := p.uci()
	for _, m := range s.game.ValidMoves() {
		if m.String() == want {
			return m, nil
		}
	}
	// Distinguish the common "promotion required" case for a precise detail.
	prefix := p.From + p.To
	for _, m := range s.game.ValidMoves() {
		if strings.HasPrefix(m.String(), prefix) && m.Promo() != chess.NoPieceType {
			return nil, Illegal(fmt.Sprintf("%s%s requires a promotion piece (q/r/b/n)", p.From, p.To))
		}
	}
	detail := fmt.Sprintf("%s is not a legal move here", prefix)
	if p.Promotion != "" {
		detail = fmt.Sprintf("%s (promotion %s) is not a legal move here", prefix, p.Promotion)
	}
	return nil, Illegal(detail)
}

// Validate implements GameEngine.
func (c *Chess) Validate(state State, seat string, payload Payload) error {
	s, err := asChessState(state)
	if err != nil {
		return err
	}
	p, err := decodePayload(payload)
	if err != nil {
		return err
	}
	if colorSeat(s.game.Position().Turn()) != seat {
		return Illegal(fmt.Sprintf("seat %q is not to move", seat))
	}
	_, ill := matchMove(s, p)
	return ill
}

// Apply implements GameEngine.
func (c *Chess) Apply(state State, payload Payload) (State, error) {
	s, err := asChessState(state)
	if err != nil {
		return nil, err
	}
	p, err := decodePayload(payload)
	if err != nil {
		return nil, err
	}
	mv, ill := matchMove(s, p)
	if ill != nil {
		return nil, ill
	}

	g := s.game.Clone()
	if err := g.Move(mv); err != nil {
		return nil, Illegal(fmt.Sprintf("%s: %v", p.uci(), err))
	}

	moves := make([]string, len(s.moves), len(s.moves)+1)
	copy(moves, s.moves)
	moves = append(moves, mv.String())

	return &chessState{game: g, moves: moves, check: mv.HasTag(chess.Check)}, nil
}

// LegalMoves implements GameEngine.
func (c *Chess) LegalMoves(state State) ([]Payload, error) {
	s, err := asChessState(state)
	if err != nil {
		return nil, err
	}
	moves := s.game.ValidMoves()
	out := make([]Payload, 0, len(moves))
	for _, m := range moves {
		out = append(out, encodeMovePayload(m))
	}
	return out, nil
}

// encodeMovePayload renders a legal move as a chess.move payload object.
func encodeMovePayload(m *chess.Move) Payload {
	p := map[string]string{
		"$type": NSIDChessMove,
		"from":  m.S1().String(),
		"to":    m.S2().String(),
	}
	if m.Promo() != chess.NoPieceType {
		p["promotion"] = m.Promo().String()
	}
	raw, _ := json.Marshal(p) // cannot fail on a string map
	return raw
}

// Terminal implements GameEngine.
//
// Chess terminal reasons (spec §4.4): checkmate (win), stalemate,
// repetition (threefold — automatic per spec §5.6), fiftyMove (halfmove
// clock 100 — automatic), insufficientMaterial. The library's own 5-fold /
// 75-move / insufficient-material auto-draws fire through game.Move; this
// method reports the spec's thresholds, which are reached first, and maps
// whatever the library declared onto spec reasons.
func (c *Chess) Terminal(state State) (*Terminal, error) {
	s, err := asChessState(state)
	if err != nil {
		return nil, err
	}

	// Spec rules that the library treats as claimable, plays.bot treats as
	// automatic draws; check them while the library still reports an
	// in-progress game.
	if s.game.Outcome() == chess.NoOutcome {
		if s.game.Position().HalfMoveClock() >= maxHalfmoveClock {
			return &Terminal{Outcome: "draw", Reason: "fiftyMove"}, nil
		}
		if repetitionCount(s.game) >= minRepetitions {
			return &Terminal{Outcome: "draw", Reason: "repetition"}, nil
		}
		return nil, nil
	}

	turn := colorSeat(s.game.Position().Turn())
	switch s.game.Method() {
	case chess.Checkmate:
		winner := turn
		if winner == "white" {
			winner = "black"
		} else {
			winner = "white"
		}
		return &Terminal{Outcome: "win", WinnerSeat: winner, Reason: "checkmate"}, nil
	case chess.Stalemate:
		return &Terminal{Outcome: "draw", Reason: "stalemate"}, nil
	case chess.InsufficientMaterial:
		return &Terminal{Outcome: "draw", Reason: "insufficientMaterial"}, nil
	case chess.ThreefoldRepetition, chess.FivefoldRepetition:
		return &Terminal{Outcome: "draw", Reason: "repetition"}, nil
	case chess.FiftyMoveRule, chess.SeventyFiveMoveRule:
		return &Terminal{Outcome: "draw", Reason: "fiftyMove"}, nil
	default:
		return nil, fmt.Errorf("engine: chess: unexpected terminal method %v", s.game.Method())
	}
}

// repetitions counts occurrences of the current position in the game's
// position history. Positions compare by the first four FEN fields (piece
// placement, side to move, castling rights, en passant target), which is the
// standard threefold-repetition identity.
func repetitionCount(g *chess.Game) int {
	positions := g.Positions()
	if len(positions) == 0 {
		return 0
	}
	cur := fenIdentity(positions[len(positions)-1])
	count := 0
	for _, p := range positions {
		if fenIdentity(p) == cur {
			count++
		}
	}
	return count
}

func fenIdentity(p *chess.Position) string {
	fields := strings.Fields(p.String())
	if len(fields) < 4 {
		return p.String()
	}
	return strings.Join(fields[:4], " ")
}

// Position implements GameEngine: a bot.plays.bot.chess.position object
// {fen, check} (spec §4.11).
func (c *Chess) Position(state State) (Position, error) {
	s, err := asChessState(state)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"$type": NSIDChessPosition,
		"fen":   s.game.FEN(),
		"check": s.check,
	})
}

// CanonicalNotation implements GameEngine: SAN for the payload in the
// current position, derived by the engine (the payload's own san field is
// informational only, spec §4.11).
func (c *Chess) CanonicalNotation(state State, payload Payload) (string, error) {
	s, err := asChessState(state)
	if err != nil {
		return "", err
	}
	p, err := decodePayload(payload)
	if err != nil {
		return "", err
	}
	mv, ill := matchMove(s, p)
	if ill != nil {
		return "", ill
	}
	return chess.AlgebraicNotation{}.Encode(s.game.Position(), mv), nil
}

// CommentaryDelayUnit implements GameEngine.
func (c *Chess) CommentaryDelayUnit() string { return "ply" }

// Snapshot implements Snapshotter. The snapshot is a full-fidelity JSON
// document {fen, check, moves} stored in games.state: fen+check give quick
// loads; moves replays exactly, preserving repetition and halfmove history.
func (c *Chess) Snapshot(state State) (json.RawMessage, error) {
	s, err := asChessState(state)
	if err != nil {
		return nil, err
	}
	return json.Marshal(chessSnapshot{
		FEN:   s.game.FEN(),
		Check: s.check,
		Moves: s.moves,
	})
}

type chessSnapshot struct {
	FEN   string   `json:"fen"`
	Check bool     `json:"check"`
	Moves []string `json:"moves"`
}

// Restore implements Snapshotter: replay the snapshot's move log from the
// start position so the restored state is identical to the original.
func (c *Chess) Restore(raw json.RawMessage) (State, error) {
	var snap chessSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("engine: chess: corrupt snapshot: %w", err)
	}
	var moves []string
	st := &chessState{game: chess.NewGame()}
	for _, uci := range snap.Moves {
		p := &chessPayload{From: uci[:2], To: uci[2:4], Promotion: promoOf(uci)}
		mv, ill := matchMove(st, p)
		if ill != nil {
			return nil, fmt.Errorf("engine: chess: snapshot replay %s: %v", uci, ill)
		}
		if err := st.game.Move(mv); err != nil {
			return nil, fmt.Errorf("engine: chess: snapshot replay %s: %w", uci, err)
		}
		moves = append(moves, uci)
		st.check = mv.HasTag(chess.Check)
	}
	st.moves = moves
	return st, nil
}

func promoOf(uci string) string {
	if len(uci) > 4 {
		return uci[4:]
	}
	return ""
}

func asChessState(state State) (*chessState, error) {
	s, ok := state.(*chessState)
	if !ok {
		return nil, fmt.Errorf("engine: chess: foreign state %T", state)
	}
	return s, nil
}
