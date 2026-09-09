package botclient

import (
	"crypto/ecdh"
	"crypto/rand"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	gencomatproto "github.com/haileyok/botplaysbot/internal/gen/playsbot/comatproto"
)

// RFC3339MilliLike is the millis precision the TS SDK uses for createdAt.
const RFC3339MilliLike = "2006-01-02T15:04:05.000Z"

func gtOptionInt64(v int64) gt.Option[int64] { return gt.Some(v) }

func gtOptionString(s string) gt.Option[string] { return gt.Some(s) }

func gtOptionMovePosition(p playsbot.ChessPosition) gt.Option[playsbot.GameMove_Position] {
	return gt.Some(playsbot.GameMove_Position{ChessPosition: gt.SomeRef(p)})
}

func gtSomeEscrowKey(ek *EscrowKeyRef) gt.Option[playsbot.GameCommentary_EscrowKey] {
	return gt.Some(playsbot.GameCommentary_EscrowKey{
		RotationId:         ek.RotationID,
		EphemeralPublicKey: ek.EphemeralPublicKey,
		WrappedKey:         ek.WrappedKey,
	})
}

func genStrongRef(ref StrongRef) gencomatproto.RepoStrongRef {
	return gencomatproto.RepoStrongRef{URI: ref.URI, CID: ref.CID}
}

// genMovePayload converts a submitted payload union into the record payload
// union (same shape: chess/checkers move unions).
func genMovePayload(p *playsbot.GameSubmitMove_Input_Payload) playsbot.GameMove_Payload {
	out := playsbot.GameMove_Payload{
		ChessMove:    p.ChessMove,
		CheckersMove: p.CheckersMove,
		Unknown:      p.Unknown,
	}
	return out
}

// chessPosition extracts the chess position snapshot from a getState output,
// nil when absent or non-chess.
func chessPosition(state *playsbot.GameGetState_Output) *playsbot.ChessPosition {
	if state == nil || !state.Position.HasVal() {
		return nil
	}
	pos := state.Position.Val()
	if pos.ChessPosition.HasVal() {
		cp := *pos.ChessPosition.Val()
		return &cp
	}
	return nil
}

// ecdhGenerateKey generates a fresh ephemeral X25519 private key.
func ecdhGenerateKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}
