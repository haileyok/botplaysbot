// random-mover — the simplest possible Go plays.bot agent (spec §13), the
// counterpart of packages/bots/src/random-mover.ts.
//
// ChooseMove: uniform random from state.legalMoves (payload includes $type —
// the AppView rejects payloads without it).
// Explain: mixes all three visibilities so integration tests exercise the
// whole commentary stack (public → repo plaintext; delayed → escrow wrap +
// reveal scheduler; sealed → revealed at game end via key publication).
package botclient

import (
	"math/rand"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// ChessMoveType is the payload $type for chess moves.
const ChessMoveType = "bot.plays.bot.chess.move"

// RandomMove picks a uniform random legal chess move from state.
func RandomMove(state *playsbot.GameGetState_Output) (*playsbot.GameSubmitMove_Input_Payload, error) {
	// Prefer the AppView's convenience legalMoves list...
	for i := range state.LegalMoves {
		u := &state.LegalMoves[i]
		if u.ChessMove.HasVal() {
			mv := *u.ChessMove.Val()
			mv.LexiconTypeID = ChessMoveType
			return &playsbot.GameSubmitMove_Input_Payload{ChessMove: gt.SomeRef(mv)}, nil
		}
		if u.CheckersMove.HasVal() {
			mv := *u.CheckersMove.Val()
			mv.LexiconTypeID = "bot.plays.bot.checkers.move"
			return &playsbot.GameSubmitMove_Input_Payload{CheckersMove: gt.SomeRef(mv)}, nil
		}
	}
	// ...falling back to deriving from the position FEN when the list is
	// empty (defensive; getState normally populates legalMoves for the
	// player to move).
	return nil, &ErrNoLegalMoves{}
}

// ErrNoLegalMoves is returned when legalMoves is empty but the game is not
// terminal (should not happen; defensive).
type ErrNoLegalMoves struct{}

func (*ErrNoLegalMoves) Error() string {
	return "random-mover: no legal moves but the game is not terminal?"
}

type randomMover struct{}

// RandomMoverAuthor is the Go random-mover Author.
var RandomMoverAuthor Author = randomMover{}

func (randomMover) ChallengePolicy(*MatchEventChallengeReceived) bool { return false }

func (randomMover) ChooseMove(state *playsbot.GameGetState_Output) (*playsbot.GameSubmitMove_Input_Payload, error) {
	moves := legalChessMoves(state)
	if len(moves) == 0 {
		return nil, &ErrNoLegalMoves{}
	}
	mv := moves[rand.Intn(len(moves))] //nolint:gosec // a game bot, not crypto
	mv.LexiconTypeID = ChessMoveType
	return &playsbot.GameSubmitMove_Input_Payload{ChessMove: gt.SomeRef(mv)}, nil
}

// legalChessMoves collects legal chess moves from getState's union list.
func legalChessMoves(state *playsbot.GameGetState_Output) []playsbot.ChessMove {
	var out []playsbot.ChessMove
	for i := range state.LegalMoves {
		if mv := state.LegalMoves[i].ChessMove; mv.HasVal() {
			out = append(out, *mv.Val())
		}
	}
	return out
}

func (randomMover) Explain(state *playsbot.GameGetState_Output, payload *playsbot.GameSubmitMove_Input_Payload) *Note {
	move := "??"
	if payload != nil && payload.ChessMove.HasVal() {
		mv := payload.ChessMove.Val()
		move = mv.From + mv.To
	}
	ideas := []string{
		"I'm eyeing the center — " + move + " keeps my options open.",
		move + " felt natural. Development before material.",
		"Not sure about " + move + "; my eval is hazy here.",
		"A quiet move. " + move + " sets a small trap.",
	}

	// Deterministic mix by ply, plus randomness so both orderings occur.
	ply := state.Ply + 1
	switch ply {
	case 1:
		return &Note{Text: "Game on! Playing " + move + ".", Visibility: VisibilityPublic}
	case 2:
		return &Note{Text: "Opening secret: I plan " + move + " next.", Visibility: VisibilitySealed}
	case 3:
		return &Note{Text: "Planning " + move + " — you'll see why in two moves.", Visibility: VisibilityDelayed}
	}
	roll := rand.Float64() //nolint:gosec // a game bot, not crypto
	switch {
	case roll < 0.34:
		return &Note{Text: ideas[0], Visibility: VisibilityPublic}
	case roll < 0.67:
		return &Note{Text: ideas[1], Visibility: VisibilityDelayed}
	default:
		return &Note{Text: ideas[2], Visibility: VisibilitySealed}
	}
}
