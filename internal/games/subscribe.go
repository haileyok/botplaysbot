package games

// game.subscribe (spec §5.4): the WebSocket stream of live game events.
//
// The handler adapts the in-memory game event bus to the lexicon's message
// shapes and frames them through the atmos Stream (which owns the upgrade,
// subprotocol negotiation, and framing). The `game` parameter filters to one
// game; omitting it streams every live game (the spectator firehose).
//
// Slow-consumer behavior (documented contract): the bus is drop-not-block —
// a subscriber that stops draining its buffer loses events (the drop counter
// on the Subscription tracks how many). This WS handler always drains, but a
// wedged TCP connection surfaces as a Stream write timeout, which fails Send,
// which ends the handler and tears the connection down; while the handler is
// wedged in a write, further bus events for it are dropped, never queued
// unboundedly. A spectator that cannot keep up therefore sees a lossy but
// always-live stream, and a stalled client can never stall a player's
// SubmitMove.

import (
	"context"
	"encoding/json"

	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot/lextypes"
)

// NSIDGameSubscribe is the game.subscribe endpoint NSID.
const NSIDGameSubscribe = "bot.plays.bot.game.subscribe"

// RegisterSubscriptions mounts the game WebSocket subscription. game.subscribe
// is public (spec §5.4); match.subscribe lives in the match package because
// it is per-DID and authenticated.
func RegisterSubscriptions(s *xrpcserver.Server, m *Manager) error {
	return s.HandleSubscription(NSIDGameSubscribe, xrpcserver.SubscriptionConfig{
		Subprotocols: gt.Some([]xrpc.Subprotocol{xrpc.SubprotocolV0CBOR, xrpc.SubprotocolV1JSON}),
	}, gameSubscribe(m))
}

// gameSubscribe serves one game.subscribe connection until the client goes
// away or the stream write fails.
func gameSubscribe(m *Manager) xrpcserver.SubscriptionHandler {
	return func(ctx context.Context, p xrpcserver.Params, stream *xrpcserver.Stream) error {
		opts := events.SubscribeOptions{}
		if p.Has("game") {
			gameURI, err := p.String("game")
			if err != nil {
				return err
			}
			opts.GameURI = gameURI
		}
		sub := m.Bus().Subscribe(opts)
		defer m.Bus().Close(sub)

		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-sub.C:
				if !ok {
					return nil
				}
				msg, fragment := gameMessage(ev)
				if msg == nil {
					continue // a kind this endpoint does not carry
				}
				if err := stream.Send(ctx, fragment, msg); err != nil {
					return err
				}
			}
		}
	}
}

// gameMessage adapts a bus event to the lexicon message union, returning the
// fragment (for the v0 header) alongside.
func gameMessage(ev events.Event) (*playsbot.GameSubscribe_Message, string) {
	switch ev.Kind {
	case events.KindGameStarted:
		return &playsbot.GameSubscribe_Message{
			GameSubscribe_GameStarted: gt.SomeRef(playsbot.GameSubscribe_GameStarted{
				Game: ev.GameURI,
			}),
		}, "#gameStarted"
	case events.KindMove:
		mv := &playsbot.GameSubscribe_Move{
			Game:    ev.GameURI,
			Ply:     ev.Move.Ply,
			Player:  ev.Move.Player,
			Payload: movePayloadUnion(ev.Move.Payload),
		}
		if !ev.Move.ReceivedAt.IsZero() {
			mv.ReceivedAt = gt.Some(RFC3339Millis(ev.Move.ReceivedAt))
		}
		if ev.Move.San != "" {
			mv.San = gt.Some(ev.Move.San)
		}
		if pos, ok := renderPositionUnion(ev.Move.Position); ok {
			mv.Position = gt.Some(playsbot.GameSubscribe_Move_Position{
				ChessPosition: gt.SomeRef(pos),
			})
		}
		for _, c := range ev.Move.Clocks {
			mv.Clocks = append(mv.Clocks, playsbot.GameGetState_Clock{
				DID:         c.DID,
				RemainingMs: c.RemainingMs,
				Deadline:    c.Deadline,
			})
		}
		return &playsbot.GameSubscribe_Message{GameSubscribe_Move: gt.SomeRef(*mv)}, "#move"
	case events.KindDrawOffered:
		return &playsbot.GameSubscribe_Message{
			GameSubscribe_DrawOffered: gt.SomeRef(playsbot.GameSubscribe_DrawOffered{
				Game:   ev.GameURI,
				Player: ev.OfferedBy,
			}),
		}, "#drawOffered"
	case events.KindCommentaryPosted:
		mv := &playsbot.GameSubscribe_CommentaryPosted{
			Game:       ev.GameURI,
			Player:     ev.CommentaryPosted.Player,
			Ply:        ev.CommentaryPosted.Ply,
			Visibility: ev.CommentaryPosted.Visibility,
		}
		// Only unrevealed delayed commentary carries reveal bounds (spec
		// §5.4: commentary events never carry text until revealed).
		if ev.CommentaryPosted.RevealsAt != nil {
			mv.RevealsAt = gt.Some(RFC3339Millis(*ev.CommentaryPosted.RevealsAt))
		}
		if ev.CommentaryPosted.RevealsAtPly != nil {
			mv.RevealsAtPly = gt.Some(*ev.CommentaryPosted.RevealsAtPly)
		}
		return &playsbot.GameSubscribe_Message{GameSubscribe_CommentaryPosted: gt.SomeRef(*mv)}, "#commentaryPosted"
	case events.KindCommentaryRevealed:
		mv := &playsbot.GameSubscribe_CommentaryRevealed{
			Game:   ev.GameURI,
			Player: ev.CommentaryRevealed.Player,
			Ply:    ev.CommentaryRevealed.Ply,
			Text:   ev.CommentaryRevealed.Text,
		}
		return &playsbot.GameSubscribe_Message{GameSubscribe_CommentaryRevealed: gt.SomeRef(*mv)}, "#commentaryRevealed"
	case events.KindGameFinished:
		if ev.Result == nil {
			return nil, ""
		}
		return &playsbot.GameSubscribe_Message{
			GameSubscribe_GameFinished: gt.SomeRef(playsbot.GameSubscribe_GameFinished{
				Game:   ev.GameURI,
				Result: renderResult(ev.Result),
			}),
		}, "#gameFinished"
	default:
		return nil, ""
	}
}

// movePayloadUnion wraps a stored payload for the move message's payload
// union (same shape as the history union).
func movePayloadUnion(raw engine.Payload) playsbot.GameSubscribe_Move_Payload {
	var probe struct {
		Type string `json:"$type"`
	}
	_ = json.Unmarshal(raw, &probe)
	switch probe.Type {
	case engine.NSIDChessMove:
		var mv playsbot.ChessMove
		if err := json.Unmarshal(raw, &mv); err == nil {
			return playsbot.GameSubscribe_Move_Payload{ChessMove: gt.SomeRef(mv)}
		}
	}
	return playsbot.GameSubscribe_Move_Payload{
		Unknown: gt.SomeRef(lextypes.UnknownUnionVariant{Type: probe.Type, Raw: json.RawMessage(raw)}),
	}
}
