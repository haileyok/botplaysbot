package match

// match.subscribe (spec §5.5a): the always-on agent's authenticated
// WebSocket stream. One connection per agent emits #matched and
// #challengeReceived notifications and feeds the presence registry: the
// connection registers on open, and its disappearance stamps lastSeen so
// the pairing loop can expire that DID's standing seeks after
// PLAYSBOT_STANDING_EXPIRE (spec §9a.3).
//
// Auth: atmos owns the upgrade and exposes no http.Request to the
// subscription handler, so the AppView's mux injects the verified identity
// into the request context before the xrpc server dispatches (see
// appview.go's authedXRPC). The in-handler identity check below is a
// backstop: without a verified identity the handler refuses to register
// presence or deliver events.
//
// Slow consumers: hub.Publish is drop-not-block (see hub.go) — a stalled
// WS client loses notifications (counted on the Conn) and can never stall
// the pairing loop or an accepting request. The atmos Stream's write
// timeout turns a wedged TCP connection into a Send error, which ends the
// handler and tears the connection down.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// timeRFC3339Nano is the lexicon datetime layout used across this package.
const timeRFC3339Nano = time.RFC3339Nano

// NSIDMatchSubscribe is the match.subscribe endpoint NSID.
const NSIDMatchSubscribe = "bot.plays.bot.match.subscribe"

// RegisterSubscription mounts the match.subscribe WebSocket subscription.
func RegisterSubscription(s *xrpcserver.Server, m *Matcher) error {
	return s.HandleSubscription(NSIDMatchSubscribe, xrpcserver.SubscriptionConfig{
		Subprotocols: gt.Some([]xrpc.Subprotocol{xrpc.SubprotocolV0CBOR, xrpc.SubprotocolV1JSON}),
	}, matchSubscribe(m))
}

// matchSubscribe serves one agent's notification stream for the connection
// lifetime.
func matchSubscribe(m *Matcher) xrpcserver.SubscriptionHandler {
	return func(ctx context.Context, _ xrpcserver.Params, stream *xrpcserver.Stream) error {
		id := auth.IdentityFromContext(ctx)
		if id == nil {
			// Post-upgrade rejection: close the stream. The mux middleware
			// normally rejects unauthenticated upgrades with a proper XRPC
			// 401 envelope before this point.
			return xrpcserver.AuthRequired("authentication required")
		}

		conn := m.hub.Connect(id.DID)
		defer m.hub.Disconnect(conn)

		for {
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-conn.C:
				if !ok {
					return nil
				}
				msg, fragment := matchMessage(ev)
				if msg == nil {
					continue
				}
				if err := stream.Send(ctx, fragment, msg); err != nil {
					return err
				}
			}
		}
	}
}

// matchMessage converts a hub event into the lexicon message union.
func matchMessage(ev HubEvent) (*playsbot.MatchSubscribe_Message, string) {
	switch ev.Kind {
	case KindMatched:
		return &playsbot.MatchSubscribe_Message{
			MatchSubscribe_Matched: gt.SomeRef(playsbot.MatchSubscribe_Matched{
				SeekId:   ev.Matched.SeekID,
				Game:     ev.Matched.Game,
				Seat:     ev.Matched.Seat,
				Opponent: ev.Matched.Opponent,
			}),
		}, "#matched"
	case KindChallengeReceived:
		ch := &playsbot.MatchSubscribe_ChallengeReceived{
			ChallengeId: ev.Challenge.ChallengeID,
			Challenger:  ev.Challenge.Challenger,
			GameType:    ev.Challenge.GameType,
			Rated:       gt.Some(ev.Challenge.Rated),
		}
		if ev.Challenge.Variant != "" {
			ch.Variant = gt.Some(ev.Challenge.Variant)
		}
		if len(ev.Challenge.TimeControlJSON) > 0 {
			var tc struct {
				Kind             string `json:"kind"`
				PerMoveSeconds   int64  `json:"perMoveSeconds"`
				InitialSeconds   int64  `json:"initialSeconds"`
				IncrementSeconds int64  `json:"incrementSeconds"`
			}
			if json.Unmarshal(ev.Challenge.TimeControlJSON, &tc) == nil {
				lex := playsbot.BotGame_TimeControl{Kind: tc.Kind}
				if tc.PerMoveSeconds != 0 {
					lex.PerMoveSeconds = gt.Some(tc.PerMoveSeconds)
				}
				if tc.InitialSeconds != 0 {
					lex.InitialSeconds = gt.Some(tc.InitialSeconds)
				}
				if tc.IncrementSeconds != 0 {
					lex.IncrementSeconds = gt.Some(tc.IncrementSeconds)
				}
				ch.TimeControl = gt.Some(lex)
			}
		}
		if !ev.Challenge.ExpiresAt.IsZero() {
			ch.ExpiresAt = gt.Some(ev.Challenge.ExpiresAt.UTC().Format(timeRFC3339Nano))
		}
		return &playsbot.MatchSubscribe_Message{
			MatchSubscribe_ChallengeReceived: gt.SomeRef(*ch),
		}, "#challengeReceived"
	default:
		return nil, ""
	}
}
