package games

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// rkeyOf extracts the record key from an AT URI.
func rkeyOf(uri string) string {
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

// gameURI builds the AT URI of a game record in the service repo.
func gameURI(serviceDID, rkey string) string {
	return fmt.Sprintf("at://%s/bot.plays.bot.game/%s", serviceDID, rkey)
}

// decodePlayers unmarshals the games.players jsonb.
func decodePlayers(raw json.RawMessage) ([]GamePlayer, error) {
	if len(raw) == 0 {
		return nil, gameError(CodeInvalidRequest, "game has no players")
	}
	var players []GamePlayer
	if err := json.Unmarshal(raw, &players); err != nil {
		return nil, fmt.Errorf("games: corrupt players jsonb: %w", err)
	}
	return players, nil
}

// seatOf returns the seat a DID holds.
func seatOf(players []GamePlayer, did string) (string, error) {
	for _, p := range players {
		if p.DID == did {
			return p.Seat, nil
		}
	}
	return "", fmt.Errorf("games: %s is not a player", did)
}

// nextSeatDID returns the DID whose turn follows seat, in engine seat order.
func nextSeatDID(players []GamePlayer, seat string) string {
	for i, p := range players {
		if p.Seat == seat {
			return players[(i+1)%len(players)].DID
		}
	}
	if len(players) > 0 {
		return players[0].DID
	}
	return ""
}

// payloadsEqual compares two payload objects semantically (key order and
// whitespace insensitive) via canonical JSON.
func payloadsEqual(a, b json.RawMessage) (bool, error) {
	ca, err := engine.CanonicalJSON(a)
	if err != nil {
		return false, err
	}
	cb, err := engine.CanonicalJSON(b)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

// decodeTimeControl unmarshals the games.time_control jsonb.
func decodeTimeControl(raw json.RawMessage) (clock.TimeControl, error) {
	var tc clock.TimeControl
	if len(raw) == 0 {
		return tc, fmt.Errorf("game has no time control")
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return tc, fmt.Errorf("corrupt time control: %w", err)
	}
	return tc, nil
}

// resultFromTerminal converts an engine terminal to the spec result shape,
// mapping the winning seat to its DID.
func resultFromTerminal(term *engine.Terminal, players []GamePlayer) *events.Result {
	r := &events.Result{Outcome: term.Outcome, Reason: term.Reason}
	if term.Outcome == "win" {
		for _, p := range players {
			if p.Seat == term.WinnerSeat {
				r.Winner = p.DID
			}
		}
	}
	return r
}

// deref, derefI64: nil-safe accessors for pointer columns.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// resultSummary renders a result as compact human-readable text (error
// messages and logs only, never machine parsing).
func resultSummary(r *events.Result) string {
	if r.Outcome == "win" {
		return fmt.Sprintf("win for %s (%s)", r.Winner, r.Reason)
	}
	return fmt.Sprintf("%s (%s)", r.Outcome, r.Reason)
}

// isoOr formats t, falling back to now when nil (lexicon datetimes are
// required once present).
func isoOr(t *time.Time) string {
	if t == nil {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// gameRecord renders the bot.plays.bot.game record (spec §4.4) from the
// games row. players may be nil on the update path (the row already holds
// the snapshot); result is nil while the game is active; finalPos attaches
// the terminal position on the finished update.
func (m *Manager) gameRecord(g *repo.Game, players []GamePlayer, result *events.Result, finalPos engine.Position) *playsbot.BotGame {
	if len(players) == 0 {
		players, _ = decodePlayers(g.Players)
	}

	recPlayers := make([]playsbot.BotGame_Player, 0, len(players))
	for _, p := range players {
		rp := playsbot.BotGame_Player{DID: p.DID, Seat: p.Seat}
		if p.ProfileRevision != nil {
			rp.ProfileRevision = gt.Some(*p.ProfileRevision)
		}
		if p.ProfileHash != nil {
			rp.ProfileHash = gt.Some(*p.ProfileHash)
		}
		recPlayers = append(recPlayers, rp)
	}

	rec := &playsbot.BotGame{
		GameType:    g.GameType,
		Players:     recPlayers,
		Status:      g.Status,
		CreatedAt:   isoOr(g.CreatedAt),
		TimeControl: playsbot.BotGame_TimeControl{Kind: "perMove"},
		PlyCount:    gt.Some(int64(g.Ply)),
	}
	if g.Variant != nil {
		rec.Variant = gt.Some(*g.Variant)
	}
	var tc clock.TimeControl
	if err := json.Unmarshal(g.TimeControl, &tc); err == nil {
		rec.TimeControl = playsbot.BotGame_TimeControl{Kind: string(tc.Kind)}
		if tc.PerMoveSeconds != 0 {
			rec.TimeControl.PerMoveSeconds = gt.Some(tc.PerMoveSeconds)
		}
		if tc.InitialSeconds != 0 {
			rec.TimeControl.InitialSeconds = gt.Some(tc.InitialSeconds)
		}
		if tc.IncrementSeconds != 0 {
			rec.TimeControl.IncrementSeconds = gt.Some(tc.IncrementSeconds)
		}
	}
	if len(g.CommentaryDelay) > 0 {
		var d configCommentaryDelay
		if err := json.Unmarshal(g.CommentaryDelay, &d); err == nil {
			rec.CommentaryDelay = gt.Some(playsbot.BotGame_CommentaryDelay{
				Plies:   int64(d.Plies),
				Seconds: int64(d.Seconds),
			})
		}
	}
	if g.StartedAt != nil {
		rec.StartedAt = gt.Some(isoOr(g.StartedAt))
	}
	if g.FinishedAt != nil {
		rec.FinishedAt = gt.Some(isoOr(g.FinishedAt))
	}

	if result != nil {
		rr := playsbot.BotGame_Result{Outcome: result.Outcome, Reason: result.Reason}
		if result.Winner != "" {
			rr.Winner = gt.Some(result.Winner)
		}
		rec.Result = gt.Some(rr)
	}
	if len(finalPos) > 0 {
		var probe struct {
			Type string `json:"$type"`
		}
		if json.Unmarshal(finalPos, &probe) == nil && probe.Type == engine.NSIDChessPosition {
			var cp playsbot.ChessPosition
			if json.Unmarshal(finalPos, &cp) == nil {
				rec.FinalPosition = gt.Some(playsbot.BotGame_FinalPosition{ChessPosition: gt.SomeRef(cp)})
			}
		}
	}
	return rec
}

// configCommentaryDelay mirrors config.CommentaryDelay's JSON shape without
// importing config here.
type configCommentaryDelay struct {
	Plies   int `json:"plies"`
	Seconds int `json:"seconds"`
}
