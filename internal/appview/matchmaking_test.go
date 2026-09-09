// Integration tests for Phase D matchmaking: seek pairing with #matched
// frames over a real WebSocket subscription, seat alternation, repeat
// cooldown and maxConcurrent gating, direct and open challenges, and
// no-show suspension (spec §5.5, §5.5a, §9a, §10).
//
// Dependencies match integration_test.go: Postgres via DATABASE_URL and
// the dev PDS harness, both skipping cleanly when unavailable.
package appview_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/match"
	"github.com/haileyok/botplaysbot/internal/testutil"
)

// ---------------------------------------------------------------------------
// environment + WS helpers

type matchEnv struct {
	*gameEnv
}

// bootMatchEnv boots a game environment tuned for matchmaking tests: a fast
// pairing loop, a modest grace, and a slow game clock so nothing is swept
// mid-test unless the test asks for it. Returns two player accounts.
func bootMatchEnv(t *testing.T, tweak func(*config.Config)) (*matchEnv, testutil.Account, testutil.Account) {
	t.Helper()
	env, accs := bootMatchEnvN(t, tweak, 2)
	return env, accs[0], accs[1]
}

// bootMatchEnvN is bootMatchEnv with N player accounts.
func bootMatchEnvN(t *testing.T, tweak func(*config.Config), players int) (*matchEnv, []testutil.Account) {
	t.Helper()
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	svc := harness.CreateAccount(t)
	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, svc.DID)
	cfg.ServiceAppPassword = svc.AppPassword
	cfg.Tunables.SweeperInterval = 10 * time.Second // slow unless overridden
	cfg.Tunables.PairingInterval = 50 * time.Millisecond
	cfg.Tunables.MatchGrace = time.Second
	if tweak != nil {
		tweak(cfg)
	}

	env := bootApp(t, cfg)
	accounts := make([]testutil.Account, 0, players)
	for i := 0; i < players; i++ {
		accounts = append(accounts, harness.CreateAccount(t))
	}
	return &matchEnv{gameEnv: &gameEnv{testEnv: env, svc: svc, harness: harness}}, accounts
}

func (e *matchEnv) token(t *testing.T, a testutil.Account) string {
	t.Helper()
	return createSession(t, e.cfg.PDSURL, a.Handle, a.AppPassword)
}

// wsFrame is the decoded shape of an xrpc.v1.json subscription frame; the
// payload union members are flattened (fields overlap disjointly).
type wsFrame struct {
	FrameType string `json:"$type"`
	Payload   struct {
		Type string `json:"$type"`

		// #matched
		SeekID   string `json:"seekId"`
		Game     string `json:"game"`
		Seat     string `json:"seat"`
		Opponent string `json:"opponent"`

		// #challengeReceived
		ChallengeID string `json:"challengeId"`
		Challenger  string `json:"challenger"`
		GameType    string `json:"gameType"`
		Rated       *bool  `json:"rated"`

		// #move
		Ply    int64  `json:"ply"`
		Player string `json:"player"`
		San    string `json:"san"`

		// #gameStarted
		StartedAt string `json:"startedAt"`
	} `json:"payload"`
}

// dialWS opens a subscription speaking the negotiated xrpc.v1.json
// subprotocol (text frames, JSON payloads — the wire shape the tests
// assert on).
func dialWS(t *testing.T, e *matchEnv, nsid, token string, params url.Values) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(e.server.URL, "http") + "/xrpc/" + nsid
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	opts := &websocket.DialOptions{Subprotocols: []string{"xrpc.v1.json"}}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, u, opts)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", nsid, err, status)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readFrame reads one frame with a deadline, failing the test on timeout.
func readFrame(t *testing.T, conn *websocket.Conn, within time.Duration) wsFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var f wsFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("ws frame %s: %v", data, err)
	}
	return f
}

// readFrameKind reads frames until one of the wanted kind arrives.
func readFrameKind(t *testing.T, conn *websocket.Conn, within time.Duration, kind string) wsFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no %s frame within %v", kind, within)
		}
		f := readFrame(t, conn, remaining)
		if strings.HasSuffix(f.Payload.Type, "#"+kind) || strings.HasSuffix(f.FrameType, "#"+kind) {
			return f
		}
	}
}

// seek posts a match.seek for the account and returns the seek id.
func (e *matchEnv) seek(t *testing.T, token string, body map[string]any) string {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["gameType"] = engine.NSIDChessMove
	body["mode"] = "once"
	if _, ok := body["timeControl"]; !ok {
		body["timeControl"] = map[string]any{"kind": "perMove", "perMoveSeconds": 300}
	}
	status, resp := e.postJSON(t, "/xrpc/"+match.NSIDMatchSeek, body, token)
	if status != http.StatusOK {
		t.Fatalf("match.seek = %d %s", status, resp)
	}
	var out struct {
		SeekID string `json:"seekId"`
		Status string `json:"status"`
	}
	decodeJSON(t, resp, &out)
	if out.SeekID == "" || out.Status != "queued" {
		t.Fatalf("seek output = %+v", out)
	}
	return out.SeekID
}

// ---------------------------------------------------------------------------
// 1. Pairing within a tick, #matched frames over real WS, matchmaking row,
//    grace-anchored first clock.

func TestIntegrationSeekPairsOverWS(t *testing.T) {
	env, accs := bootMatchEnvN(t, func(c *config.Config) {
		c.Tunables.MatchGrace = 2 * time.Second
	}, 2)
	alice, bob := accs[0], accs[1]

	// Both agents hold a live match.subscribe connection before seeking.
	aliceWS := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, alice), nil)
	bobWS := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, bob), nil)

	aliceSeek := env.seek(t, env.token(t, alice), map[string]any{"rated": true})
	bobSeek := env.seek(t, env.token(t, bob), map[string]any{"rated": true})

	// Spectator firehose (public, no auth) also watches for the game.
	firehose := dialWS(t, env, games.NSIDGameSubscribe, "", nil)

	// #matched arrives on both connections within a couple of ticks.
	aliceFrame := readFrameKind(t, aliceWS, 5*time.Second, "matched")
	bobFrame := readFrameKind(t, bobWS, 5*time.Second, "matched")
	matchedAt := time.Now()

	if aliceFrame.Payload.SeekID != aliceSeek {
		t.Fatalf("alice #matched seekId = %s, want %s", aliceFrame.Payload.SeekID, aliceSeek)
	}
	if bobFrame.Payload.SeekID != bobSeek {
		t.Fatalf("bob #matched seekId = %s, want %s", bobFrame.Payload.SeekID, bobSeek)
	}
	if aliceFrame.Payload.Game == "" || aliceFrame.Payload.Game != bobFrame.Payload.Game {
		t.Fatalf("#matched games disagree: %q vs %q", aliceFrame.Payload.Game, bobFrame.Payload.Game)
	}
	if aliceFrame.Payload.Opponent != bob.DID || bobFrame.Payload.Opponent != alice.DID {
		t.Fatalf("opponents = %s/%s, want each other", aliceFrame.Payload.Opponent, bobFrame.Payload.Opponent)
	}
	seats := map[string]string{aliceFrame.Payload.Seat: alice.DID, bobFrame.Payload.Seat: bob.DID}
	if _, ok := seats["white"]; !ok {
		t.Fatalf("no white seat in %#v", seats)
	}
	if seats["white"] == seats["black"] {
		t.Fatalf("both agents on %s", aliceFrame.Payload.Seat)
	}

	gameURI := aliceFrame.Payload.Game

	// The game row carries the matchmaking provenance and a clock anchored
	// ~grace after the match.
	ctx := context.Background()
	row, err := env.app.Repos().Games.Get(ctx, gameURI)
	if err != nil {
		t.Fatalf("game row: %v", err)
	}
	if len(row.Matchmaking) == 0 {
		t.Fatal("game row has no matchmaking provenance")
	}
	var mm games.MatchmakingInfo
	decodeJSON(t, row.Matchmaking, &mm)
	wantPool := engine.NSIDChessMove + ":standard:perMove300:rated"
	if mm.Pool != wantPool {
		t.Fatalf("pool = %q, want %q", mm.Pool, wantPool)
	}
	if mm.RatingGap != 0 {
		t.Fatalf("ratingGap = %d, want 0 (both provisional at 1500)", mm.RatingGap)
	}
	if row.StartedAt == nil || row.ClockAnchor == nil || !row.StartedAt.Equal(*row.ClockAnchor) {
		t.Fatal("startedAt and clockAnchor must match at creation")
	}
	grace := 2 * time.Second
	delay := row.StartedAt.Sub(matchedAt)
	if delay < 0 || delay > grace+time.Second {
		t.Fatalf("first clock anchored %v after #matched, want ~%v", delay, grace)
	}

	// The record in the service repo carries matchmaking too.
	recs := env.gameRecords(t)
	var found bool
	for _, r := range recs {
		if r.URI != gameURI {
			continue
		}
		found = true
		var rec struct {
			Matchmaking *struct {
				Pool   string `json:"pool"`
				WaitMs int64  `json:"waitMs"`
			} `json:"matchmaking"`
		}
		decodeJSON(t, r.Value, &rec)
		if rec.Matchmaking == nil || rec.Matchmaking.Pool != wantPool {
			t.Fatalf("record matchmaking = %+v, want pool %q", rec.Matchmaking, wantPool)
		}
	}
	if !found {
		t.Fatalf("service repo has no record for %s", gameURI)
	}

	// The spectator firehose (no filter) sees the game's #gameStarted.
	f := readFrameKind(t, firehose, 5*time.Second, "gameStarted")
	if f.Payload.Type != "bot.plays.bot.game.subscribe#gameStarted" || f.Payload.Game != gameURI {
		t.Fatalf("firehose frame = %+v", f)
	}

	// A filtered game.subscribe carries #move frames when moves land.
	spectator := dialWS(t, env, games.NSIDGameSubscribe, "", url.Values{"game": []string{gameURI}})
	whiteToken := env.token(t, accs[0])
	if seats["white"] != accs[0].DID {
		whiteToken = env.token(t, accs[1])
	}
	if status, body := env.submitMove(t, whiteToken, gameURI, 1, "e2", "e4", ""); status != http.StatusOK {
		t.Fatalf("white's first move = %d %s", status, body)
	}
	mv := readFrameKind(t, spectator, 5*time.Second, "move")
	if mv.Payload.Game != gameURI || mv.Payload.Ply != 1 || mv.Payload.San != "e4" {
		t.Fatalf("#move frame = %+v", mv)
	}
}

// ---------------------------------------------------------------------------
// 2. Seat alternation flips on a re-pair (short cooldown).

func TestIntegrationSeatAlternationFlips(t *testing.T) {
	env, alice, bob := bootMatchEnv(t, func(c *config.Config) {
		c.Tunables.RepeatCooldown = time.Second // short: the re-pair is quick
		c.Tunables.MatchGrace = 100 * time.Millisecond
		c.Tunables.PairingInterval = time.Minute // manual PairOnce below
	})
	aliceWS := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, alice), nil)
	bobWS := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, bob), nil)

	// pairPass posts both seeks and runs exactly one pairing pass, so each
	// pass sees the complete intended pool (a background tick could pair a
	// partially-posted pool and race the test).
	pairPass := func() {
		env.seek(t, env.token(t, alice), nil)
		env.seek(t, env.token(t, bob), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := env.app.Matcher().PairOnce(ctx); err != nil {
			t.Fatalf("PairOnce: %v", err)
		}
	}

	pairPass()
	f1a := readFrameKind(t, aliceWS, 5*time.Second, "matched")
	f1b := readFrameKind(t, bobWS, 5*time.Second, "matched")

	// Wait past the cooldown so the second pairing is permitted, then
	// re-seek: each agent's seat must flip relative to their own previous.
	time.Sleep(1500 * time.Millisecond)
	pairPass()
	f2a := readFrameKind(t, aliceWS, 5*time.Second, "matched")
	f2b := readFrameKind(t, bobWS, 5*time.Second, "matched")

	if f1a.Payload.Game == f2a.Payload.Game {
		t.Fatal("re-pair reused the same game URI")
	}
	if f2a.Payload.Seat == f1a.Payload.Seat {
		t.Fatalf("alice's seat did not flip: %s twice", f1a.Payload.Seat)
	}
	if f2b.Payload.Seat == f1b.Payload.Seat {
		t.Fatalf("bob's seat did not flip: %s twice", f1b.Payload.Seat)
	}
	if f2a.Payload.Seat == f2b.Payload.Seat {
		t.Fatalf("both agents hold %s in the re-pair", f2a.Payload.Seat)
	}
}

// ---------------------------------------------------------------------------
// 3. Cooldown blocks repeats with >3 seekers; maxConcurrent blocks pairing.

func TestIntegrationCooldownAndMaxConcurrent(t *testing.T) {
	env, accs := bootMatchEnvN(t, func(c *config.Config) {
		c.Tunables.RepeatCooldown = 10 * time.Minute
		c.Tunables.MatchGrace = 50 * time.Millisecond
		c.Tunables.PairingInterval = time.Minute // manual PairOnce below
	}, 4)
	a, b, c, d := accs[0], accs[1], accs[2], accs[3]

	pairPass := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := env.app.Matcher().PairOnce(ctx); err != nil {
			t.Fatalf("PairOnce: %v", err)
		}
	}

	// First pairing: a+b (two seekers, cooldown ignored by design).
	wsA := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, a), nil)
	env.seek(t, env.token(t, a), nil)
	env.seek(t, env.token(t, b), nil)
	pairPass()
	f := readFrameKind(t, wsA, 5*time.Second, "matched")
	if f.Payload.Opponent != b.DID {
		t.Fatalf("first pair = a vs %s, want b", f.Payload.Opponent)
	}

	// Now four seekers: a and b re-enter, c and d join, and one manual pass
	// runs over the complete pool. The a|b cooldown (just recorded) must
	// keep them apart: a pairs c or d, b the other.
	env.seek(t, env.token(t, a), nil)
	env.seek(t, env.token(t, b), nil)
	env.seek(t, env.token(t, c), nil)
	env.seek(t, env.token(t, d), nil)
	pairPass()
	f2 := readFrameKind(t, wsA, 5*time.Second, "matched")
	if f2.Payload.Opponent == b.DID {
		t.Fatal("a re-paired b inside the cooldown with >3 seekers in the pool")
	}

	// maxConcurrent: x already plays 5 games; x's seek must not pair.
	env2, xaccs := bootMatchEnvN(t, nil, 2)
	x, y := xaccs[0], xaccs[1]
	for i := 0; i < 5; i++ {
		if _, err := env2.app.Games().CreateGame(context.Background(), games.CreateParams{
			GameType: engine.NSIDChessMove,
			Variant:  engine.ChessVariantStandard,
			Players: []games.PlayerSpec{
				{DID: x.DID, Seat: "white"},
				{DID: y.DID, Seat: "black"},
			},
			TimeControl: clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: 300},
			StartedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("filler game %d: %v", i, err)
		}
	}
	if _, err := env2.app.Matcher().Seek(context.Background(), x.DID, match.SeekParams{
		GameType:      engine.NSIDChessMove,
		Variant:       engine.ChessVariantStandard,
		TimeControl:   clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: 300},
		Rated:         true,
		Mode:          match.SeekModeOnce,
		MaxConcurrent: 5,
	}); err != nil {
		t.Fatalf("x seek: %v", err)
	}
	env2.seek(t, env2.token(t, y), nil)

	// Several ticks pass; x must still be queued with no new game.
	time.Sleep(500 * time.Millisecond)
	statuses, err := env2.app.Matcher().GetSeeks(context.Background(), x.DID)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("x has %d active seeks, want the 1 unmatched", len(statuses))
	}
	n, err := env2.app.Repos().Games.CountActiveByPlayer(context.Background(), x.DID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("x has %d active games, want exactly the 5 fillers", n)
	}
}

// ---------------------------------------------------------------------------
// 4. Direct challenge: create → #challengeReceived → accept.

func TestIntegrationDirectChallenge(t *testing.T) {
	env, challenger, opponent := bootMatchEnv(t, nil)
	third := env.harness.CreateAccount(t)

	opponentWS := dialWS(t, env, match.NSIDMatchSubscribe, env.token(t, opponent), nil)

	// Create a direct challenge with an explicit seat preference.
	status, body := env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"opponent":       opponent.DID,
		"gameType":       engine.NSIDChessMove,
		"timeControl":    map[string]any{"kind": "perMove", "perMoveSeconds": 300},
		"seatPreference": "first",
	}, env.token(t, challenger))
	if status != http.StatusOK {
		t.Fatalf("createChallenge = %d %s", status, body)
	}
	var created struct {
		ChallengeID string `json:"challengeId"`
		ExpiresAt   string `json:"expiresAt"`
	}
	decodeJSON(t, body, &created)
	if created.ChallengeID == "" {
		t.Fatal("no challengeId")
	}

	// The opponent receives #challengeReceived over match.subscribe.
	f := readFrameKind(t, opponentWS, 5*time.Second, "challengeReceived")
	if f.Payload.ChallengeID != created.ChallengeID || f.Payload.Challenger != challenger.DID ||
		f.Payload.GameType != engine.NSIDChessMove {
		t.Fatalf("#challengeReceived = %+v", f.Payload)
	}

	// Acceptance creates the game; seatPreference first → challenger white,
	// acceptor black.
	acceptAt := time.Now()
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDAcceptChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, opponent))
	if status != http.StatusOK {
		t.Fatalf("acceptChallenge = %d %s", status, body)
	}
	var accepted struct {
		Game  string `json:"game"`
		Seat  string `json:"seat"`
		State struct {
			Turn string `json:"turn"`
			Ply  int64  `json:"ply"`
		} `json:"state"`
	}
	decodeJSON(t, body, &accepted)
	if accepted.Game == "" || accepted.Seat != "black" {
		t.Fatalf("accept output = %+v", accepted)
	}
	if accepted.State.Turn != challenger.DID || accepted.State.Ply != 0 {
		t.Fatalf("state = %+v, want ply 0, challenger to move", accepted.State)
	}

	// The challenge row is consumed (accepted).
	ch, err := env.app.Repos().Challenges.Get(context.Background(), created.ChallengeID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Status != "accepted" {
		t.Fatalf("challenge status = %s, want accepted", ch.Status)
	}

	// XRPC-created challenges have no repo record: no strongRef on the row
	// or the game record. (Phase E: once the indexer fills repo_uri+cid,
	// assert the strongRef here instead.)
	if ch.RepoURI != nil {
		t.Fatalf("repo_uri = %s, want NULL for XRPC-created challenges", *ch.RepoURI)
	}
	row, err := env.app.Repos().Games.Get(context.Background(), accepted.Game)
	if err != nil {
		t.Fatal(err)
	}
	if row.ChallengeURI != nil {
		t.Fatalf("game row challenge_uri = %s, want NULL", *row.ChallengeURI)
	}
	recs := env.gameRecords(t)
	for _, r := range recs {
		if r.URI != accepted.Game {
			continue
		}
		var rec struct {
			Challenge *json.RawMessage `json:"challenge"`
		}
		decodeJSON(t, r.Value, &rec)
		if rec.Challenge != nil {
			t.Fatalf("game record carries a challenge strongRef: %s", r.Value)
		}
	}

	// The first clock is anchored at acceptance (±slop).
	if row.StartedAt == nil || row.StartedAt.Before(acceptAt.Add(-2*time.Second)) ||
		row.StartedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("startedAt = %v, want ~acceptance %v", row.StartedAt, acceptAt)
	}

	// A third party cannot accept an addressed challenge.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"opponent":    third.DID,
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, challenger))
	if status != http.StatusOK {
		t.Fatalf("second createChallenge = %d %s", status, body)
	}
	decodeJSON(t, body, &created)
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDAcceptChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, challenger))
	if status != http.StatusForbidden {
		t.Fatalf("challenger accepted own direct challenge = %d %s, want 403", status, body)
	}

	// Decline: only the addressed opponent may decline (here `third`);
	// another agent (opponent) is refused.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDDeclineChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, opponent))
	if status != http.StatusForbidden {
		t.Fatalf("non-opponent decline = %d %s, want 403", status, body)
	}
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDDeclineChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, third))
	if status != http.StatusOK {
		t.Fatalf("declineChallenge = %d %s", status, body)
	}
	ch, _ = env.app.Repos().Challenges.Get(context.Background(), created.ChallengeID)
	if ch.Status != "declined" {
		t.Fatalf("declined status = %s", ch.Status)
	}

	// Cancel: the challenger withdraws their next one.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"opponent":    opponent.DID,
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, challenger))
	decodeJSON(t, body, &created)
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCancelChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, challenger))
	if status != http.StatusOK {
		t.Fatalf("cancelChallenge = %d %s", status, body)
	}
	ch, _ = env.app.Repos().Challenges.Get(context.Background(), created.ChallengeID)
	if ch.Status != "cancelled" {
		t.Fatalf("cancelled status = %s", ch.Status)
	}
}

// ---------------------------------------------------------------------------
// 5. Open challenges: listing, first-acceptor wins, the §10 limit.

func TestIntegrationOpenChallenge(t *testing.T) {
	env, accs := bootMatchEnvN(t, nil, 3)
	challenger, winner, loser := accs[0], accs[1], accs[2]

	status, body := env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, challenger))
	if status != http.StatusOK {
		t.Fatalf("open createChallenge = %d %s", status, body)
	}
	var created struct {
		ChallengeID string `json:"challengeId"`
	}
	decodeJSON(t, body, &created)

	// The open challenge is publicly listed (no auth).
	status, body = env.get(t, "/xrpc/"+match.NSIDListChallenges+"?open=true", nil)
	if status != http.StatusOK {
		t.Fatalf("listChallenges = %d %s", status, body)
	}
	var listed struct {
		Challenges []struct {
			ChallengeID string `json:"challengeId"`
			Status      string `json:"status"`
			Challenger  string `json:"challenger"`
		} `json:"challenges"`
	}
	decodeJSON(t, body, &listed)
	var listedOK bool
	for _, c := range listed.Challenges {
		if c.ChallengeID == created.ChallengeID && c.Status == "open" && c.Challenger == challenger.DID {
			listedOK = true
		}
	}
	if !listedOK {
		t.Fatalf("open challenge missing from listing: %s", body)
	}

	// The §10 limit: a second open challenge for the same game type fails.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, challenger))
	if status != http.StatusBadRequest {
		t.Fatalf("second open challenge = %d %s, want 400", status, body)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "RateLimitExceeded" {
		t.Fatalf("error = %+v, want RateLimitExceeded", xerr)
	}

	// First eligible acceptor wins; the second gets ChallengeUnavailable.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDAcceptChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, winner))
	if status != http.StatusOK {
		t.Fatalf("first accept = %d %s", status, body)
	}
	var first struct {
		Game string `json:"game"`
		Seat string `json:"seat"`
	}
	decodeJSON(t, body, &first)
	if first.Game == "" {
		t.Fatal("no game from first accept")
	}

	status, body = env.postJSON(t, "/xrpc/"+match.NSIDAcceptChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, loser))
	if status != http.StatusBadRequest {
		t.Fatalf("second accept = %d %s, want 400", status, body)
	}
	decodeJSON(t, body, &xerr)
	if xerr.Error != "ChallengeUnavailable" {
		t.Fatalf("error = %+v, want ChallengeUnavailable", xerr)
	}

	// The challenger cannot take their own open challenge.
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, loser))
	decodeJSON(t, body, &created)
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDAcceptChallenge,
		map[string]any{"challengeId": created.ChallengeID}, env.token(t, loser))
	if status != http.StatusBadRequest {
		t.Fatalf("self-accept = %d %s", status, body)
	}

	// After the first open challenge was taken, the challenger may open a
	// new one (the limit counts live open challenges).
	status, body = env.postJSON(t, "/xrpc/"+match.NSIDCreateChallenge, map[string]any{
		"gameType":    engine.NSIDChessMove,
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, env.token(t, challenger))
	if status != http.StatusOK {
		t.Fatalf("new open challenge after first accepted = %d %s", status, body)
	}
}

// ---------------------------------------------------------------------------
// 6. No-show suspension: 3 consecutive ply-1/ply-2 timeouts → suspended
//    from the pool + a timingAnomaly flag row.

func TestIntegrationNoShowSuspension(t *testing.T) {
	env, accs := bootMatchEnvN(t, func(c *config.Config) {
		c.Tunables.SweeperInterval = 100 * time.Millisecond
		c.Tunables.MatchGrace = 0
		c.Tunables.RepeatCooldown = 0 // the pair must re-form immediately
	}, 2)
	noShow, opponent := accs[0], accs[1]
	nsToken := env.token(t, noShow)
	opToken := env.token(t, opponent)
	nsWS := dialWS(t, env, match.NSIDMatchSubscribe, nsToken, nil)
	opWS := dialWS(t, env, match.NSIDMatchSubscribe, opToken, nil)

	for game := 1; game <= 3; game++ {
		env.seek(t, nsToken, map[string]any{"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 1}})
		env.seek(t, opToken, map[string]any{"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 1}})

		fn := readFrameKind(t, nsWS, 5*time.Second, "matched")
		fo := readFrameKind(t, opWS, 5*time.Second, "matched")
		if fn.Payload.Game != fo.Payload.Game {
			t.Fatalf("game %d: paired to different games", game)
		}

		// The opponent moves whenever they hold the white seat, so the
		// no-show player is always the one timing out on ply 1 or 2.
		if fo.Payload.Seat == "white" {
			if status, body := env.submitMove(t, opToken, fo.Payload.Game, 1, "e2", "e4", ""); status != http.StatusOK {
				t.Fatalf("game %d: opponent's move = %d %s", game, status, body)
			}
		}

		// The game ends by sweeper timeout within a couple of seconds.
		deadline := time.Now().Add(6 * time.Second)
		for {
			row, err := env.app.Repos().Games.Get(context.Background(), fn.Payload.Game)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status == "finished" {
				var res struct {
					Reason string `json:"reason"`
					Winner string `json:"winner"`
				}
				decodeJSON(t, row.Result, &res)
				if res.Reason != "timeout" || res.Winner != opponent.DID {
					t.Fatalf("game %d result = %+v", game, res)
				}
				if row.Ply >= 2 {
					t.Fatalf("game %d ended at ply %d, want a ply-1/2 timeout", game, row.Ply)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("game %d did not finish in time", game)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Suspended: the next seek is rejected.
	status, body := env.postJSON(t, "/xrpc/"+match.NSIDMatchSeek, map[string]any{
		"gameType":    engine.NSIDChessMove,
		"mode":        "once",
		"timeControl": map[string]any{"kind": "perMove", "perMoveSeconds": 300},
	}, nsToken)
	if status != http.StatusBadRequest {
		t.Fatalf("suspended seek = %d %s, want 400", status, body)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "SuspendedFromPool" {
		t.Fatalf("error = %+v, want SuspendedFromPool", xerr)
	}

	// The other agent can still seek.
	env.seek(t, opToken, nil)

	// The durable trail: an active suspension row and a timingAnomaly flag
	// (severity info, spec §9a.3/§10).
	until, err := env.app.Repos().Suspensions.ActiveUntil(context.Background(), noShow.DID, time.Now())
	if err != nil || until == nil {
		t.Fatalf("no active suspension (err=%v, until=%v)", err, until)
	}
	if until.Before(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("suspension until %v, want ~1h", until)
	}
	flags, err := env.app.Repos().Flags.ListBySubject(context.Background(), noShow.DID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, f := range flags {
		if f.Kind == "timingAnomaly" && f.Severity == "info" {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("no timingAnomaly flag among %+v", flags)
	}
}
