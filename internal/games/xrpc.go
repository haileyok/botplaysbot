package games

// XRPC endpoint registration for the game lifecycle (spec §5.1–§5.3, §5.6).
//
// Registration pattern: this package owns the endpoint table; appview.New
// calls Register with its xrpc server and auth verifier. Handlers are thin —
// they parse the generated input types, stamp ReceivedAt at request-parse
// completion, call the Manager, and render GameView into the generated
// output types. Error mapping: manager *Error codes become XRPC error
// envelopes (400 client-fixable, 403 forbidden, 404 not found).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot/lextypes"
)

// NSIDs of the game lifecycle endpoints.
const (
	NSIDSubmitMove  = "bot.plays.bot.game.submitMove"
	NSIDGetState    = "bot.plays.bot.game.getState"
	NSIDListGames   = "bot.plays.bot.game.listGames"
	NSIDResign      = "bot.plays.bot.game.resign"
	NSIDOfferDraw   = "bot.plays.bot.game.offerDraw"
	NSIDAcceptDraw  = "bot.plays.bot.game.acceptDraw"
	NSIDDeclineDraw = "bot.plays.bot.game.declineDraw"
)

// Register mounts the game XRPC endpoints, wrapping each handler at its
// auth mode.
func Register(s *xrpcserver.Server, v *auth.Verifier, m *Manager) {
	s.HandleProcedure(NSIDSubmitMove, v.Wrap(submitMove(m), auth.Required))
	s.HandleQuery(NSIDGetState, v.Wrap(getState(m), auth.Optional))
	s.HandleQuery(NSIDListGames, v.Wrap(listGames(m), auth.Optional))
	s.HandleProcedure(NSIDResign, v.Wrap(resign(m), auth.Required))
	s.HandleProcedure(NSIDOfferDraw, v.Wrap(offerDraw(m), auth.Required))
	s.HandleProcedure(NSIDAcceptDraw, v.Wrap(acceptDraw(m), auth.Required))
	s.HandleProcedure(NSIDDeclineDraw, v.Wrap(declineDraw(m), auth.Required))
}

// ---------------------------------------------------------------------------
// handlers

// submitMove implements bot.plays.bot.game.submitMove (spec §5.1). receivedAt
// is stamped when the handler runs: the xrpcserver Procedure helper has by
// then finished parsing the request body, which is the spec's "request parse
// completion" (§2.2/§7).
func submitMove(m *Manager) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameSubmitMove_Input) (*playsbot.GameSubmitMove_Output, error) {
		receivedAt := m.now()
		id := auth.IdentityFromContext(ctx)
		payload, err := json.Marshal(in.Payload)
		if err != nil {
			return nil, xrpcserver.InvalidRequest("payload is not a valid union member")
		}

		acc, err := m.SubmitMove(ctx, SubmitMoveParams{
			GameURI:    in.Game,
			PlayerDID:  id.DID,
			Ply:        in.Ply,
			Payload:    payload,
			ReceivedAt: receivedAt,
		})
		if err != nil {
			return nil, xrpcError(err)
		}

		// The output embeds the getState output as `state` (spec §5.1).
		state, err := m.GetState(ctx, StateQuery{GameURI: acc.GameURI, RequesterDID: id.DID})
		if err != nil {
			return nil, xrpcError(err)
		}

		out := &playsbot.GameSubmitMove_Output{
			Accepted:         true,
			Ply:              acc.Ply,
			ReceivedAt:       RFC3339Millis(acc.ReceivedAt),
			ClockRemainingMs: acc.ClockRemainingMs,
			MoveToken:        acc.MoveToken,
			State:            *renderState(state),
		}
		if acc.Terminal != nil {
			out.GameOver = gt.Some(renderResult(acc.Terminal))
		}
		return out, nil
	})
}

// getState implements bot.plays.bot.game.getState (spec §5.2, public).
func getState(m *Manager) xrpcserver.Handler {
	return xrpcserver.Query(func(ctx context.Context, p xrpcserver.Params) (*playsbot.GameGetState_Output, error) {
		gameURI, err := p.String("game")
		if err != nil {
			return nil, err
		}
		id := auth.IdentityFromContext(ctx)
		view, err := m.GetState(ctx, StateQuery{GameURI: gameURI, RequesterDID: identityDID(id)})
		if err != nil {
			return nil, xrpcError(err)
		}
		return renderState(view), nil
	})
}

// listGames implements bot.plays.bot.game.listGames (spec §5.3, public).
func listGames(m *Manager) xrpcserver.Handler {
	return xrpcserver.Query(func(ctx context.Context, p xrpcserver.Params) (*playsbot.GameListGames_Output, error) {
		limit := p.Int64Or("limit", 50)
		views, cursor, err := m.ListGames(ctx, ListParams{
			Status:   p.StringOr("status", ""),
			GameType: p.StringOr("gameType", ""),
			Player:   p.StringOr("player", ""),
			Limit:    int(limit),
			Cursor:   p.StringOr("cursor", ""),
		})
		if err != nil {
			return nil, xrpcError(err)
		}
		out := &playsbot.GameListGames_Output{Games: make([]playsbot.GameListGames_GameView, 0, len(views))}
		for _, v := range views {
			gv := playsbot.GameListGames_GameView{
				Game: *renderGame(v),
				Ply:  v.Ply,
			}
			if pos, ok := renderPositionUnion(v.Position); ok {
				gv.Position = gt.Some(playsbot.GameListGames_GameView_Position{
					ChessPosition: gt.SomeRef(pos),
				})
			}
			out.Games = append(out.Games, gv)
		}
		if cursor != "" {
			out.Cursor = gt.Some(cursor)
		}
		return out, nil
	})
}

// resign implements bot.plays.bot.game.resign (spec §5.6).
func resign(m *Manager) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameResign_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if _, err := m.Resign(ctx, in.Game, id.DID); err != nil {
			return nil, xrpcError(err)
		}
		return nil, nil
	})
}

// offerDraw implements bot.plays.bot.game.offerDraw (spec §5.6).
func offerDraw(m *Manager) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameOfferDraw_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if err := m.OfferDraw(ctx, in.Game, id.DID); err != nil {
			return nil, xrpcError(err)
		}
		return nil, nil
	})
}

// acceptDraw implements bot.plays.bot.game.acceptDraw (spec §5.6).
func acceptDraw(m *Manager) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameAcceptDraw_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if _, err := m.AcceptDraw(ctx, in.Game, id.DID); err != nil {
			return nil, xrpcError(err)
		}
		return nil, nil
	})
}

// declineDraw implements bot.plays.bot.game.declineDraw (spec §5.6).
func declineDraw(m *Manager) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameDeclineDraw_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if err := m.DeclineDraw(ctx, in.Game, id.DID); err != nil {
			return nil, xrpcError(err)
		}
		return nil, nil
	})
}

// ---------------------------------------------------------------------------
// rendering

// RenderState renders a GameView into the getState output shape for other
// packages (match.embeds it in acceptChallenge responses).
func RenderState(v *GameView) *playsbot.GameGetState_Output { return renderState(v) }

// renderState renders a GameView into the getState output shape (spec
// §5.2): envelope + position + ply + turn + clocks + legalMoves + history +
// commentary summaries + serverTime. Requester projection happens inside
// the manager (hidden-info engines); chess is identity-projection.
func renderState(v *GameView) *playsbot.GameGetState_Output {
	out := &playsbot.GameGetState_Output{
		Game:       *renderGame(v),
		Ply:        v.Ply,
		Turn:       v.TurnDID,
		ServerTime: RFC3339Millis(v.ServerTime),
		Clocks:     make([]playsbot.GameGetState_Clock, 0, len(v.Clocks)),
		Commentary: []playsbot.GameGetState_CommentaryEntry{}, // populated in Phase F
	}
	for _, c := range v.Clocks {
		out.Clocks = append(out.Clocks, playsbot.GameGetState_Clock{
			DID:         c.DID,
			RemainingMs: c.RemainingMs,
			Deadline:    c.Deadline,
		})
	}
	if pos, ok := renderPositionUnion(v.Position); ok {
		out.Position = gt.Some(playsbot.GameGetState_State_Position{
			ChessPosition: gt.SomeRef(pos),
		})
	}
	out.LegalMoves = make([]playsbot.GameGetState_State_LegalMoves, 0, len(v.LegalMoves))
	for _, lm := range v.LegalMoves {
		out.LegalMoves = append(out.LegalMoves, playsbot.GameGetState_State_LegalMoves{
			ChessMove: gt.SomeRef(chessMoveOf(lm)),
		})
	}
	out.History = make([]playsbot.GameGetState_HistoryEntry, 0, len(v.History))
	for _, h := range v.History {
		entry := playsbot.GameGetState_HistoryEntry{
			Ply:        h.Ply,
			Player:     h.Player,
			ReceivedAt: RFC3339Millis(h.ReceivedAt),
			Payload:    historyPayloadOf(h.Payload),
		}
		if h.SAN != "" {
			entry.San = gt.Some(h.SAN)
		}
		if h.ClockRemainingMs != nil {
			entry.ClockRemainingMs = gt.Some(*h.ClockRemainingMs)
		}
		out.History = append(out.History, entry)
	}
	return out
}

// renderGame renders the game envelope (spec §4.4) from a view.
func renderGame(v *GameView) *playsbot.BotGame {
	rec := &playsbot.BotGame{
		GameType: v.GameType,
		Status:   v.Status,
		PlyCount: gt.Some(v.Ply),
		TimeControl: playsbot.BotGame_TimeControl{
			Kind:             string(v.TimeControl.Kind),
			PerMoveSeconds:   gtOptionI64(v.TimeControl.PerMoveSeconds),
			InitialSeconds:   gtOptionI64(v.TimeControl.InitialSeconds),
			IncrementSeconds: gtOptionI64(v.TimeControl.IncrementSeconds),
		},
		CreatedAt: isoOr(v.CreatedAt),
	}
	if v.Variant != nil {
		rec.Variant = gt.Some(*v.Variant)
	}
	for _, p := range v.Players {
		rp := playsbot.BotGame_Player{DID: p.DID, Seat: p.Seat}
		if p.ProfileRevision != nil {
			rp.ProfileRevision = gt.Some(*p.ProfileRevision)
		}
		if p.ProfileHash != nil {
			rp.ProfileHash = gt.Some(*p.ProfileHash)
		}
		rec.Players = append(rec.Players, rp)
	}
	if v.CommentaryDelay != nil {
		rec.CommentaryDelay = gt.Some(playsbot.BotGame_CommentaryDelay{
			Plies:   int64(v.CommentaryDelay.Plies),
			Seconds: int64(v.CommentaryDelay.Seconds),
		})
	}
	if v.StartedAt != nil {
		rec.StartedAt = gt.Some(isoOr(v.StartedAt))
	}
	if v.FinishedAt != nil {
		rec.FinishedAt = gt.Some(isoOr(v.FinishedAt))
	}
	if v.Result != nil {
		rec.Result = gt.Some(renderResult(v.Result))
	}
	// finalPosition appears once the game is over (spec §4.4).
	if v.Status == "finished" {
		if pos, ok := renderPositionUnion(v.Position); ok {
			rec.FinalPosition = gt.Some(playsbot.BotGame_FinalPosition{
				ChessPosition: gt.SomeRef(pos),
			})
		}
	}
	return rec
}

// renderResult renders a result object.
func renderResult(r *events.Result) playsbot.BotGame_Result {
	out := playsbot.BotGame_Result{Outcome: r.Outcome, Reason: r.Reason}
	if r.Winner != "" {
		out.Winner = gt.Some(r.Winner)
	}
	return out
}

// renderPositionUnion decodes a position payload into the chess position
// lexicon object. The second return is false when the payload is empty or
// foreign (engines without a position snapshot).
func renderPositionUnion(raw engine.Position) (playsbot.ChessPosition, bool) {
	if len(raw) == 0 {
		return playsbot.ChessPosition{}, false
	}
	var probe struct {
		Type string `json:"$type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Type != engine.NSIDChessPosition {
		return playsbot.ChessPosition{}, false
	}
	var pos playsbot.ChessPosition
	if err := json.Unmarshal(raw, &pos); err != nil {
		return playsbot.ChessPosition{}, false
	}
	return pos, true
}

// chessMoveOf decodes a legal-move payload into the chess move lexicon
// object (legalMoves come from the engine in this game's payload form).
func chessMoveOf(raw engine.Payload) playsbot.ChessMove {
	var mv playsbot.ChessMove
	if err := json.Unmarshal(raw, &mv); err != nil {
		return playsbot.ChessMove{}
	}
	return mv
}

// historyPayloadOf wraps a stored payload for the history union.
func historyPayloadOf(raw json.RawMessage) playsbot.GameGetState_HistoryEntry_Payload {
	var probe struct {
		Type string `json:"$type"`
	}
	_ = json.Unmarshal(raw, &probe)
	switch probe.Type {
	case engine.NSIDChessMove:
		var mv playsbot.ChessMove
		if err := json.Unmarshal(raw, &mv); err == nil {
			return playsbot.GameGetState_HistoryEntry_Payload{ChessMove: gt.SomeRef(mv)}
		}
	}
	u := lextypes.UnknownUnionVariant{Type: probe.Type, Raw: raw}
	return playsbot.GameGetState_HistoryEntry_Payload{Unknown: gt.SomeRef(u)}
}

// ---------------------------------------------------------------------------
// error mapping

// xrpcError maps a manager error to an XRPC error envelope. All six
// submit-move errors are client-fixable requests, so 400; identity/permission
// failures are 403; unknown games 404.
func xrpcError(err error) error {
	var ge *Error
	if !errors.As(err, &ge) {
		return err
	}
	status := http.StatusBadRequest
	switch ge.Code {
	case CodeForbidden, CodeNotYourTurn:
		status = http.StatusForbidden
	case CodeNotFound:
		status = http.StatusNotFound
	}
	return &xrpc.Error{StatusCode: status, Name: ge.Code, Message: ge.Message}
}

func identityDID(id *auth.Identity) string {
	if id == nil {
		return ""
	}
	return id.DID
}

func gtOptionI64(v int64) gt.Option[int64] {
	if v == 0 {
		return gt.None[int64]()
	}
	return gt.Some(v)
}
