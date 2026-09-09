package match

// XRPC endpoints for challenges and seeks (spec §5.5, §5.5a). Handlers are
// thin: parse the generated input, call the Matcher, render the output.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// NSIDs of the challenge + seek endpoints.
const (
	NSIDCreateChallenge  = "bot.plays.bot.game.createChallenge"
	NSIDAcceptChallenge  = "bot.plays.bot.game.acceptChallenge"
	NSIDDeclineChallenge = "bot.plays.bot.game.declineChallenge"
	NSIDCancelChallenge  = "bot.plays.bot.game.cancelChallenge"
	NSIDListChallenges   = "bot.plays.bot.game.listChallenges"
	NSIDMatchSeek        = "bot.plays.bot.match.seek"
	NSIDMatchCancelSeek  = "bot.plays.bot.match.cancelSeek"
	NSIDMatchGetSeeks    = "bot.plays.bot.match.getSeeks"
)

// Register mounts the challenge + seek XRPC endpoints.
func Register(s *xrpcserver.Server, v *auth.Verifier, m *Matcher) {
	s.HandleProcedure(NSIDCreateChallenge, v.Wrap(createChallenge(m), auth.Required))
	s.HandleProcedure(NSIDAcceptChallenge, v.Wrap(acceptChallenge(m), auth.Required))
	s.HandleProcedure(NSIDDeclineChallenge, v.Wrap(declineChallenge(m), auth.Required))
	s.HandleProcedure(NSIDCancelChallenge, v.Wrap(cancelChallenge(m), auth.Required))
	s.HandleQuery(NSIDListChallenges, v.Wrap(listChallenges(m), auth.Optional))
	s.HandleProcedure(NSIDMatchSeek, v.Wrap(matchSeek(m), auth.Required))
	s.HandleProcedure(NSIDMatchCancelSeek, v.Wrap(matchCancelSeek(m), auth.Required))
	s.HandleQuery(NSIDMatchGetSeeks, v.Wrap(matchGetSeeks(m), auth.Required))
}

// ---------------------------------------------------------------------------
// challenges

func createChallenge(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameCreateChallenge_Input) (*playsbot.GameCreateChallenge_Output, error) {
		id := auth.IdentityFromContext(ctx)
		now := m.now()

		// Expiry: absolute datetime or the default TTL, capped at 24h
		// (spec §4.8). A zero TTL (partially-populated config) falls back
		// to the spec default so a challenge is never born expired.
		ttl := m.cfg.Tunables.ChallengeTTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		expiresAt := now.Add(ttl)
		if in.ExpiresAt.HasVal() {
			parsed, err := time.Parse(time.RFC3339, in.ExpiresAt.Val())
			if err != nil {
				return nil, xrpcserver.InvalidRequest("expiresAt must be an RFC 3339 datetime")
			}
			expiresAt = parsed
		}
		if maxExpiry := now.Add(m.cfg.Tunables.ChallengeMaxTTL); m.cfg.Tunables.ChallengeMaxTTL > 0 && expiresAt.After(maxExpiry) {
			expiresAt = maxExpiry
		}

		c, err := m.CreateChallenge(ctx, id.DID, ChallengeParams{
			Opponent:        in.Opponent.ValOr(""),
			GameType:        in.GameType,
			Variant:         in.Variant.ValOr(""),
			TimeControl:     timeControlFromLex(in.TimeControl),
			CommentaryDelay: commentaryDelayFromLex(in.CommentaryDelay),
			SeatPreference:  in.SeatPreference.ValOr(""),
			Rated:           in.Rated.ValOr(true),
			ExpiresAt:       expiresAt,
		})
		if err != nil {
			return nil, mapError(err)
		}
		return &playsbot.GameCreateChallenge_Output{
			ChallengeId: c.ID,
			ExpiresAt:   RFC3339(derefTime(c.ExpiresAt, now)),
		}, nil
	})
}

func acceptChallenge(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameAcceptChallenge_Input) (*playsbot.GameAcceptChallenge_Output, error) {
		id := auth.IdentityFromContext(ctx)
		out, err := m.AcceptChallenge(ctx, id.DID, in.ChallengeId)
		if err != nil {
			return nil, mapError(err)
		}
		state, err := m.games.GetState(ctx, games.StateQuery{GameURI: out.Created.URI, RequesterDID: id.DID})
		if err != nil {
			return nil, mapError(err)
		}
		return &playsbot.GameAcceptChallenge_Output{
			Game:  out.Created.URI,
			Seat:  out.Seat,
			State: *games.RenderState(state),
		}, nil
	})
}

func declineChallenge(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameDeclineChallenge_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if err := m.DeclineChallenge(ctx, id.DID, in.ChallengeId); err != nil {
			return nil, mapError(err)
		}
		return nil, nil
	})
}

func cancelChallenge(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.GameCancelChallenge_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if err := m.CancelChallenge(ctx, id.DID, in.ChallengeId); err != nil {
			return nil, mapError(err)
		}
		return nil, nil
	})
}

func listChallenges(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Query(func(ctx context.Context, p xrpcserver.Params) (*playsbot.GameListChallenges_Output, error) {
		id := auth.IdentityFromContext(ctx)
		open := new(bool)
		if p.Has("open") {
			b, err := p.Bool("open")
			if err != nil {
				return nil, err
			}
			*open = b
		} else {
			open = nil
		}
		challenges, cursor, err := m.ListChallenges(ctx, identityDID(id), ChallengeFilter{
			GameType: p.StringOr("gameType", ""),
			Opponent: p.StringOr("opponent", ""),
			Open:     open,
			Limit:    int(p.Int64Or("limit", 50)),
			Cursor:   p.StringOr("cursor", ""),
		})
		if err != nil {
			return nil, mapError(err)
		}
		out := &playsbot.GameListChallenges_Output{Challenges: make([]playsbot.GameListChallenges_ChallengeView, 0, len(challenges))}
		for _, c := range challenges {
			out.Challenges = append(out.Challenges, challengeView(c))
		}
		if cursor != "" {
			out.Cursor = gt.Some(cursor)
		}
		return out, nil
	})
}

func challengeView(c *repo.Challenge) playsbot.GameListChallenges_ChallengeView {
	var tc clock.TimeControl
	_ = json.Unmarshal(c.TimeControl, &tc)
	view := playsbot.GameListChallenges_ChallengeView{
		ChallengeId:    c.ID,
		Challenger:     c.ChallengerDID,
		GameType:       c.GameType,
		Rated:          c.Rated,
		Status:         c.Status,
		SeatPreference: gt.Some(c.SeatPreference),
		TimeControl:    timeControlToLex(tc),
		CreatedAt:      RFC3339(derefTime(c.CreatedAt, time.Now())),
	}
	if c.OpponentDID != nil {
		view.Opponent = gt.Some(*c.OpponentDID)
	}
	if c.Variant != nil {
		view.Variant = gt.Some(*c.Variant)
	}
	if c.ExpiresAt != nil {
		view.ExpiresAt = gt.Some(RFC3339(*c.ExpiresAt))
	}
	if len(c.CommentaryDelay) > 0 {
		var d config.CommentaryDelay
		if json.Unmarshal(c.CommentaryDelay, &d) == nil {
			view.CommentaryDelay = gt.Some(playsbot.BotGame_CommentaryDelay{
				Plies:   int64(d.Plies),
				Seconds: int64(d.Seconds),
			})
		}
	}
	return view
}

// ---------------------------------------------------------------------------
// seeks

func matchSeek(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.MatchSeek_Input) (*playsbot.MatchSeek_Output, error) {
		id := auth.IdentityFromContext(ctx)
		seek, err := m.Seek(ctx, id.DID, SeekParams{
			GameType:      in.GameType,
			Variant:       in.Variant.ValOr(""),
			TimeControl:   timeControlFromLex(in.TimeControl.ValOr(playsbot.BotGame_TimeControl{})),
			Rated:         in.Rated.ValOr(true),
			RatingWindow:  in.RatingWindow.ValOr(0),
			MaxConcurrent: in.MaxConcurrent.ValOr(0),
			Mode:          in.Mode,
		})
		if err != nil {
			return nil, mapError(err)
		}
		// The pairing loop matches within the next tick; #matched carries
		// the game (spec §5.5a).
		return &playsbot.MatchSeek_Output{SeekId: seek.ID, Status: "queued"}, nil
	})
}

func matchCancelSeek(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Procedure(func(ctx context.Context, _ xrpcserver.Params, in *playsbot.MatchCancelSeek_Input) (*struct{}, error) {
		id := auth.IdentityFromContext(ctx)
		if err := m.CancelSeek(ctx, id.DID, in.SeekId); err != nil {
			return nil, mapError(err)
		}
		return nil, nil
	})
}

func matchGetSeeks(m *Matcher) xrpcserver.Handler {
	return xrpcserver.Query(func(ctx context.Context, _ xrpcserver.Params) (*playsbot.MatchGetSeeks_Output, error) {
		id := auth.IdentityFromContext(ctx)
		statuses, err := m.GetSeeks(ctx, id.DID)
		if err != nil {
			return nil, mapError(err)
		}
		out := &playsbot.MatchGetSeeks_Output{Seeks: make([]playsbot.MatchGetSeeks_SeekView, 0, len(statuses))}
		for _, st := range statuses {
			s := st.Seek
			var tc clock.TimeControl
			_ = json.Unmarshal(s.TimeControl, &tc)
			view := playsbot.MatchGetSeeks_SeekView{
				SeekId:        s.ID,
				GameType:      s.GameType,
				Mode:          s.Mode,
				Status:        "queued",
				QueuePosition: int64(st.QueuePosition),
				Rated:         gt.Some(s.Rated),
				TimeControl:   gt.Some(timeControlToLex(tc)),
				CreatedAt:     RFC3339(derefTime(s.CreatedAt, time.Now())),
			}
			if s.Variant != nil {
				view.Variant = gt.Some(*s.Variant)
			}
			if s.RatingWindow != nil {
				view.RatingWindow = gt.Some(int64(*s.RatingWindow))
			}
			if s.MaxConcurrent != nil {
				view.MaxConcurrent = gt.Some(int64(*s.MaxConcurrent))
			}
			out.Seeks = append(out.Seeks, view)
		}
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// conversion + error mapping + small helpers

func timeControlFromLex(t playsbot.BotGame_TimeControl) clock.TimeControl {
	return clock.TimeControl{
		Kind:             clock.Kind(t.Kind),
		PerMoveSeconds:   t.PerMoveSeconds.ValOr(0),
		InitialSeconds:   t.InitialSeconds.ValOr(0),
		IncrementSeconds: t.IncrementSeconds.ValOr(0),
	}
}

func timeControlToLex(t clock.TimeControl) playsbot.BotGame_TimeControl {
	out := playsbot.BotGame_TimeControl{Kind: string(t.Kind)}
	if t.PerMoveSeconds != 0 {
		out.PerMoveSeconds = gt.Some(t.PerMoveSeconds)
	}
	if t.InitialSeconds != 0 {
		out.InitialSeconds = gt.Some(t.InitialSeconds)
	}
	if t.IncrementSeconds != 0 {
		out.IncrementSeconds = gt.Some(t.IncrementSeconds)
	}
	return out
}

func commentaryDelayFromLex(o gt.Option[playsbot.BotGame_CommentaryDelay]) *config.CommentaryDelay {
	if !o.HasVal() {
		return nil
	}
	return &config.CommentaryDelay{Plies: int(o.Val().Plies), Seconds: int(o.Val().Seconds)}
}

// mapError converts a matcher error to an XRPC error envelope.
func mapError(err error) error {
	var me *Error
	if !errors.As(err, &me) {
		return err
	}
	status := http.StatusBadRequest
	switch me.Code {
	case CodeChallengeNotFound, CodeSeekNotFound:
		status = http.StatusNotFound
	case CodeForbidden:
		status = http.StatusForbidden
	}
	return &xrpc.Error{StatusCode: status, Name: me.Code, Message: me.Message}
}

func identityDID(id *auth.Identity) string {
	if id == nil {
		return ""
	}
	return id.DID
}

// RFC3339 formats a time in the lexicon datetime shape.
func RFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
