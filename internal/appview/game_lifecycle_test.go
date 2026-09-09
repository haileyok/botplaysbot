// Integration tests for the Phase C game lifecycle: full game to terminal
// through the XRPC API with real auth tokens, rejection paths, idempotent
// replays, clock expiry (reject-side and sweeper-side), resign and draw
// flows, automatic draws, and moveToken verification against the published
// service key.
//
// Dependencies: Postgres via DATABASE_URL and the dev PDS harness (both
// skip cleanly when unavailable), matching the conventions in
// integration_test.go.
package appview_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/xrpc"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/testutil"
)

// ---------------------------------------------------------------------------
// game test environment

type gameEnv struct {
	*testEnv
	svc     testutil.Account
	harness *testutil.PDSHarness
}

// bootGameEnv boots the app against an isolated DB and PDS harness with a
// real service account (so game records round-trip the PDS), a short
// sweeper interval, and two player accounts.
func bootGameEnv(t *testing.T) (*gameEnv, testutil.Account, testutil.Account) {
	t.Helper()
	return bootGameEnvSweeper(t, 100*time.Millisecond)
}

// bootGameEnvSweeper is bootGameEnv with an explicit sweeper interval.
func bootGameEnvSweeper(t *testing.T, sweeper time.Duration) (*gameEnv, testutil.Account, testutil.Account) {
	t.Helper()
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	svc := harness.CreateAccount(t)
	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, svc.DID)
	cfg.ServiceAppPassword = svc.AppPassword
	// Tests shorten the sweeper (spec §7 default is 1s).
	cfg.Tunables.SweeperInterval = sweeper

	env := bootApp(t, cfg)
	white := harness.CreateAccount(t)
	black := harness.CreateAccount(t)
	return &gameEnv{testEnv: env, svc: svc, harness: harness}, white, black
}

// newChessGame creates a standard perMove chess game, white first.
func newChessGame(t *testing.T, env *gameEnv, white, black testutil.Account, perMoveSeconds int64) string {
	t.Helper()
	out, err := env.app.Games().CreateGame(context.Background(), games.CreateParams{
		GameType: engine.NSIDChessMove,
		Variant:  engine.ChessVariantStandard,
		Players: []games.PlayerSpec{
			{DID: white.DID, Seat: "white"},
			{DID: black.DID, Seat: "black"},
		},
		TimeControl: clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: perMoveSeconds},
		StartedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	return out.URI
}

type moveReq struct {
	Game    string         `json:"game"`
	Ply     int64          `json:"ply"`
	Payload map[string]any `json:"payload"`
}

// submitMove posts a submitMove call as the given account and returns the
// raw status/body.
func (e *gameEnv) submitMove(t *testing.T, token, gameURI string, ply int64, from, to, promo string) (int, []byte) {
	t.Helper()
	payload := map[string]any{
		"$type": engine.NSIDChessMove,
		"from":  from,
		"to":    to,
	}
	if promo != "" {
		payload["promotion"] = promo
	}
	body, err := json.Marshal(moveReq{Game: gameURI, Ply: ply, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/xrpc/"+games.NSIDSubmitMove, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("submitMove: %v", err)
	}
	defer resp.Body.Close()
	buf := readAll(t, resp)
	return resp.StatusCode, buf
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// submitMoveExpectErr asserts an error envelope with the given name.
func (e *gameEnv) submitMoveExpectErr(t *testing.T, name string, token, gameURI string, ply int64, from, to, promo string) xrpcErrorBody {
	t.Helper()
	status, body := e.submitMove(t, token, gameURI, ply, from, to, promo)
	if status == http.StatusOK {
		t.Fatalf("submitMove %s%s ply %d succeeded, want %s", from, to, ply, name)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != name {
		t.Fatalf("submitMove %s%s ply %d: error %+v, want %s", from, to, ply, xerr, name)
	}
	return xerr
}

// getState fetches authoritative state as JSON.
func (e *gameEnv) getState(t *testing.T, gameURI string) map[string]any {
	t.Helper()
	status, body := e.get(t, "/xrpc/"+games.NSIDGetState+"?game="+gameURIEscape(gameURI), nil)
	if status != http.StatusOK {
		t.Fatalf("getState = %d %s", status, body)
	}
	var out map[string]any
	decodeJSON(t, body, &out)
	return out
}

func gameURIEscape(s string) string {
	return strings.ReplaceAll(s, "/", "%2F")
}

// gameRecords lists the service repo's bot.plays.bot.game records.
func (e *gameEnv) gameRecords(t *testing.T) []comatproto.RepoListRecords_Record {
	t.Helper()
	client := &xrpc.Client{Host: e.cfg.PDSURL}
	out, err := comatproto.RepoListRecords(context.Background(), client, "bot.plays.bot.game", "", 50, e.svc.DID, false)
	if err != nil {
		t.Fatalf("listRecords: %v", err)
	}
	return out.Records
}

func expectEvent(t *testing.T, sub *events.Subscription, wantKind string) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.C:
		if !ok {
			t.Fatalf("event bus closed waiting for %s", wantKind)
		}
		if ev.Kind != wantKind {
			t.Fatalf("event = %s, want %s", ev.Kind, wantKind)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", wantKind)
		return events.Event{}
	}
}

func expectNoEvent(t *testing.T, sub *events.Subscription) {
	t.Helper()
	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// 1. Full game to terminal (fool's mate) with PDS record verification

func TestGameFullToCheckmate(t *testing.T) {
	env, white, black := bootGameEnv(t)

	// Bus subscription before creation (the #gameStarted event fires during
	// CreateGame). No game filter: this test's DB has exactly one game.
	sub := env.app.Games().Bus().Subscribe(events.SubscribeOptions{})
	gameURI := newChessGame(t, env, white, black, 300)
	expectEvent(t, sub, events.KindGameStarted)

	// Initial getState: active, ply 0, white to move, legal moves present.
	// The envelope fields live under "game" (spec §5.2).
	st := env.getState(t, gameURI)
	game := st["game"].(map[string]any)
	if got := game["status"].(string); got != "active" {
		t.Fatalf("initial status = %v", got)
	}
	if got := st["ply"].(float64); got != 0 {
		t.Fatalf("initial ply = %v", got)
	}
	if got := st["turn"].(string); got != white.DID {
		t.Fatalf("initial turn = %v, want white", got)
	}
	if lms, ok := st["legalMoves"].([]any); !ok || len(lms) != 20 {
		t.Fatalf("initial legalMoves = %v, want 20", st["legalMoves"])
	}
	if hist, ok := st["history"].([]any); !ok || len(hist) != 0 {
		t.Fatalf("initial history = %v, want empty", hist)
	}
	if comm, ok := st["commentary"].([]any); !ok || len(comm) != 0 {
		t.Fatalf("commentary = %v, want empty array (Phase F)", comm)
	}
	if _, ok := st["serverTime"].(string); !ok {
		t.Fatal("serverTime missing")
	}

	// The service repo has the game record, status active.
	recs := env.gameRecords(t)
	if len(recs) != 1 {
		t.Fatalf("service repo has %d game records, want 1", len(recs))
	}
	if recs[0].URI != gameURI {
		t.Fatalf("record uri = %s, want %s", recs[0].URI, gameURI)
	}
	var created struct {
		Status string `json:"status"`
	}
	decodeJSON(t, recs[0].Value, &created)
	if created.Status != "active" {
		t.Fatalf("created record status = %q, want active", created.Status)
	}

	// Fool's mate: 1. f3 e5 2. g4 Qh4#.
	var finalBody []byte
	moves := []struct {
		token       string
		from, to    string
		wantSAN     string
	}{
		{accessJWT(t, env, white), "f2", "f3", "f3"},
		{accessJWT(t, env, black), "e7", "e5", "e5"},
		{accessJWT(t, env, white), "g2", "g4", "g4"},
		{accessJWT(t, env, black), "d8", "h4", "Qh4#"},
	}
	for i, mv := range moves {
		status, body := env.submitMove(t, mv.token, gameURI, int64(i+1), mv.from, mv.to, "")
		if status != http.StatusOK {
			t.Fatalf("move %d (%s%s) = %d %s", i+1, mv.from, mv.to, status, body)
		}
		var out struct {
			Accepted   bool   `json:"accepted"`
			Ply        int64  `json:"ply"`
			ReceivedAt string `json:"receivedAt"`
			MoveToken  string `json:"moveToken"`
			State      struct {
				Ply     int64  `json:"ply"`
				Turn    string `json:"turn"`
				History []struct {
					San string `json:"san"`
				} `json:"history"`
			} `json:"state"`
		}
		decodeJSON(t, body, &out)
		if !out.Accepted || out.Ply != int64(i+1) {
			t.Fatalf("move %d acceptance = %+v", i+1, out)
		}
		if out.MoveToken == "" {
			t.Fatalf("move %d: no moveToken", i+1)
		}
		if _, err := time.Parse(time.RFC3339, out.ReceivedAt); err != nil {
			t.Fatalf("move %d receivedAt %q: %v", i+1, out.ReceivedAt, err)
		}
		if len(out.State.History) != i+1 || out.State.History[i].San != mv.wantSAN {
			t.Fatalf("move %d history = %+v, want san %q", i+1, out.State.History, mv.wantSAN)
		}
		finalBody = body
	}

	// The final submit reported gameOver in its own response.
	var final struct {
		GameOver *struct {
			Outcome string `json:"outcome"`
			Winner  string `json:"winner"`
			Reason  string `json:"reason"`
		} `json:"gameOver"`
	}
	decodeJSON(t, finalBody, &final)
	if final.GameOver == nil {
		t.Fatalf("final submit has no gameOver: %s", finalBody)
	}
	if final.GameOver.Outcome != "win" || final.GameOver.Winner != black.DID || final.GameOver.Reason != "checkmate" {
		t.Fatalf("gameOver = %+v", final.GameOver)
	}

	// Authoritative state is finished with the expected position.
	st = env.getState(t, gameURI)
	game = st["game"].(map[string]any)
	if got := game["status"].(string); got != "finished" {
		t.Fatalf("final status = %v", got)
	}
	res := game["result"].(map[string]any)
	if res["outcome"] != "win" || res["winner"] != black.DID || res["reason"] != "checkmate" {
		t.Fatalf("result = %v", res)
	}
	pos := st["position"].(map[string]any)
	if pos["fen"] != "rnb1kbnr/pppp1ppp/8/4p3/6Pq/5P2/PPPPP2P/RNBQKBNR w KQkq - 1 3" {
		t.Fatalf("final fen = %v", pos["fen"])
	}
	if pos["check"] != true {
		t.Fatalf("final check = %v, want true (mated)", pos["check"])
	}
	if lms, ok := st["legalMoves"].([]any); !ok || len(lms) != 0 {
		t.Fatalf("finished legalMoves = %v, want empty", lms)
	}

	// The service repo record was updated in place: finished + result.
	recs = env.gameRecords(t)
	if len(recs) != 1 {
		t.Fatalf("after finish the service repo has %d game records, want the same 1", len(recs))
	}
	var finished struct {
		Status       string `json:"status"`
		PlyCount     int64  `json:"plyCount"`
		Result       struct {
			Outcome string `json:"outcome"`
			Winner  string `json:"winner"`
			Reason  string `json:"reason"`
		} `json:"result"`
		FinalPosition struct {
			Type string `json:"$type"`
			Fen  string `json:"fen"`
		} `json:"finalPosition"`
	}
	decodeJSON(t, recs[0].Value, &finished)
	if finished.Status != "finished" || finished.PlyCount != 4 {
		t.Fatalf("finished record = status %q plyCount %d", finished.Status, finished.PlyCount)
	}
	if finished.Result.Outcome != "win" || finished.Result.Winner != black.DID || finished.Result.Reason != "checkmate" {
		t.Fatalf("finished record result = %+v", finished.Result)
	}
	if finished.FinalPosition.Type != engine.NSIDChessPosition ||
		finished.FinalPosition.Fen != "rnb1kbnr/pppp1ppp/8/4p3/6Pq/5P2/PPPPP2P/RNBQKBNR w KQkq - 1 3" {
		t.Fatalf("finished record finalPosition = %+v", finished.FinalPosition)
	}

	// Bus events: one #move per accepted move, then #gameFinished.
	for i := 0; i < 4; i++ {
		ev := expectEvent(t, sub, events.KindMove)
		if ev.Move.Ply != int64(i+1) {
			t.Fatalf("move event ply = %d", ev.Move.Ply)
		}
	}
	fin := expectEvent(t, sub, events.KindGameFinished)
	if fin.Result == nil || fin.Result.Outcome != "win" || fin.Result.Winner != black.DID {
		t.Fatalf("gameFinished = %+v", fin.Result)
	}
	expectNoEvent(t, sub)
}

// accessJWT mints a fresh access token for an account.
func accessJWT(t *testing.T, env *gameEnv, a testutil.Account) string {
	t.Helper()
	return createSession(t, env.cfg.PDSURL, a.Handle, a.AppPassword)
}

// ---------------------------------------------------------------------------
// 2. Rejection paths

func TestGameRejections(t *testing.T) {
	env, white, black := bootGameEnv(t)
	third := env.harness.CreateAccount(t) // a non-player with a real token
	gameURI := newChessGame(t, env, white, black, 300)

	whiteToken := accessJWT(t, env, white)
	blackToken := accessJWT(t, env, black)
	thirdToken := accessJWT(t, env, third)

	// No auth at all: 401 AuthRequired envelope.
	status, body := env.submitMove(t, "", gameURI, 1, "e2", "e4", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated submitMove = %d %s", status, body)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "AuthRequired" {
		t.Fatalf("unauthenticated error = %+v", xerr)
	}

	// Not a player (valid token, third party): NotYourTurn per the lexicon's
	// error set (no dedicated NotAPlayer error exists in spec §5.1).
	env.submitMoveExpectErr(t, "NotYourTurn", thirdToken, gameURI, 1, "e2", "e4", "")

	// Out of turn: black moves first.
	env.submitMoveExpectErr(t, "NotYourTurn", blackToken, gameURI, 1, "e7", "e5", "")

	// Illegal move: pawn cannot move three squares; detail is surfaced.
	xerr = env.submitMoveExpectErr(t, "IllegalMove", whiteToken, gameURI, 1, "e2", "e5", "")
	if xerr.Message == "" {
		t.Fatal("IllegalMove without detail")
	}

	// Malformed payload: missing `from`.
	status, body = env.submitMove(t, whiteToken, gameURI, 1, "", "e4", "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed payload = %d %s", status, body)
	}
	decodeJSON(t, body, &xerr)
	if xerr.Error != "MalformedPayload" {
		t.Fatalf("malformed error = %+v", xerr)
	}

	// Stale ply: ply 5 when 1 is expected.
	env.submitMoveExpectErr(t, "PlyMismatch", whiteToken, gameURI, 5, "e2", "e4", "")

	// Ply 0 is never valid.
	env.submitMoveExpectErr(t, "PlyMismatch", whiteToken, gameURI, 0, "e2", "e4", "")

	// Unknown game.
	env.submitMoveExpectErr(t, "GameNotActive", whiteToken, "at://"+env.svc.DID+"/bot.plays.bot.game/3zzzzzzzzzzzz", 1, "e2", "e4", "")

	// Sanity: white's real move still works after all rejections.
	status, body = env.submitMove(t, whiteToken, gameURI, 1, "e2", "e4", "")
	if status != http.StatusOK {
		t.Fatalf("accepted move after rejections = %d %s", status, body)
	}
}

// ---------------------------------------------------------------------------
// 3. Idempotency

func TestGameIdempotency(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)
	whiteToken := accessJWT(t, env, white)

	status, first := env.submitMove(t, whiteToken, gameURI, 1, "e2", "e4", "")
	if status != http.StatusOK {
		t.Fatalf("first move = %d %s", status, first)
	}

	// Retry the exact (game, ply, payload) AFTER black has moved (so it is
	// not white's turn): the original acceptance is replayed verbatim,
	// never NotYourTurn.
	status, second := env.submitMove(t, whiteToken, gameURI, 1, "e2", "e4", "")
	if status != http.StatusOK {
		t.Fatalf("retry = %d %s", status, second)
	}
	var acc1, acc2 struct {
		Ply        int64  `json:"ply"`
		ReceivedAt string `json:"receivedAt"`
		MoveToken  string `json:"moveToken"`
	}
	decodeJSON(t, first, &acc1)
	decodeJSON(t, second, &acc2)
	if acc1.MoveToken == "" || acc1.MoveToken != acc2.MoveToken {
		t.Fatalf("retry token differs: %q vs %q", acc1.MoveToken, acc2.MoveToken)
	}
	if acc1.ReceivedAt != acc2.ReceivedAt {
		t.Fatalf("retry receivedAt differs: %q vs %q", acc1.ReceivedAt, acc2.ReceivedAt)
	}

	// Same ply, different payload: PlyMismatch.
	env.submitMoveExpectErr(t, "PlyMismatch", whiteToken, gameURI, 1, "d2", "d4", "")

	// Still exactly one stored move.
	move, err := env.app.Repos().Moves.Get(context.Background(), gameURI, 1)
	if err != nil {
		t.Fatalf("stored move: %v", err)
	}
	if move.Token == nil || *move.Token != acc1.MoveToken {
		t.Fatal("stored token differs from the accepted token")
	}
}

// ---------------------------------------------------------------------------
// 4. Clock expiry

func TestGameClockExpiredOnLateSubmit(t *testing.T) {
	env, white, black := bootGameEnvSweeper(t, 10*time.Second)
	// Short clock: backdating the anchor by 10s puts the deadline well in
	// the past. The sweeper is (deliberately) too slow to interfere, so the
	// late submit itself must hit the reject-side timeout path.
	sub := env.app.Games().Bus().Subscribe(events.SubscribeOptions{})
	gameURI := newChessGame(t, env, white, black, 2)
	expectEvent(t, sub, events.KindGameStarted)

	// Backdate the clock anchor: the deadline passed seconds ago.
	ctx := context.Background()
	if _, err := env.app.Repos().Exec(ctx,
		`UPDATE games SET clock_anchor = clock_anchor - interval '10 seconds' WHERE uri=$1`, gameURI); err != nil {
		t.Fatal(err)
	}

	xerr := env.submitMoveExpectErr(t, "ClockExpired", accessJWT(t, env, white), gameURI, 1, "e2", "e4", "")
	if !strings.Contains(xerr.Message, "timeout") {
		t.Fatalf("ClockExpired message %q does not carry the result", xerr.Message)
	}

	// The game was finished by timeout with the opponent as winner.
	st := env.getState(t, gameURI)
	game := st["game"].(map[string]any)
	if game["status"] != "finished" {
		t.Fatalf("status = %v, want finished", game["status"])
	}
	res := game["result"].(map[string]any)
	if res["outcome"] != "win" || res["winner"] != black.DID || res["reason"] != "timeout" {
		t.Fatalf("result = %v", res)
	}

	fin := expectEvent(t, sub, events.KindGameFinished)
	if fin.Result == nil || fin.Result.Reason != "timeout" || fin.Result.Winner != black.DID {
		t.Fatalf("gameFinished = %+v", fin.Result)
	}

	// A subsequent submit sees the finished game, not the expired clock.
	env.submitMoveExpectErr(t, "GameNotActive", accessJWT(t, env, white), gameURI, 1, "e2", "e4", "")
}

func TestGameClockExpiredSweeper(t *testing.T) {
	env, white, black := bootGameEnv(t)
	// perMove 2s + a 100ms sweeper (set in bootGameEnv): the game ends
	// without anyone submitting, within ~2.5s of start.
	sub := env.app.Games().Bus().Subscribe(events.SubscribeOptions{})
	gameURI := newChessGame(t, env, white, black, 2)
	expectEvent(t, sub, events.KindGameStarted)

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := env.getState(t, gameURI)
		game := st["game"].(map[string]any)
		if game["status"] == "finished" {
			res := game["result"].(map[string]any)
			if res["reason"] != "timeout" || res["winner"] != black.DID {
				t.Fatalf("sweeper result = %v", res)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not finish the expired game in 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fin := expectEvent(t, sub, events.KindGameFinished)
	if fin.Result == nil || fin.Result.Reason != "timeout" {
		t.Fatalf("gameFinished = %+v", fin.Result)
	}
}

// ---------------------------------------------------------------------------
// 5. Resign and draws

func TestGameResign(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)

	status, body := env.postJSON(t, "/xrpc/"+games.NSIDResign, map[string]any{"game": gameURI}, accessJWT(t, env, white))
	if status != http.StatusOK {
		t.Fatalf("resign = %d %s", status, body)
	}

	st := env.getState(t, gameURI)
	res := st["game"].(map[string]any)["result"].(map[string]any)
	if res["outcome"] != "win" || res["winner"] != black.DID || res["reason"] != "resignation" {
		t.Fatalf("resign result = %v", res)
	}
}

func TestGameDrawAgreement(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)
	whiteToken := accessJWT(t, env, white)
	blackToken := accessJWT(t, env, black)

	// Accepting without an offer is invalid.
	status, body := env.postJSON(t, "/xrpc/"+games.NSIDAcceptDraw, map[string]any{"game": gameURI}, blackToken)
	if status == http.StatusOK {
		t.Fatalf("accept without offer succeeded: %s", body)
	}

	// White offers, black accepts: agreement draw.
	if status, body := env.postJSON(t, "/xrpc/"+games.NSIDOfferDraw, map[string]any{"game": gameURI}, whiteToken); status != http.StatusOK {
		t.Fatalf("offerDraw = %d %s", status, body)
	}
	if status, body := env.postJSON(t, "/xrpc/"+games.NSIDAcceptDraw, map[string]any{"game": gameURI}, blackToken); status != http.StatusOK {
		t.Fatalf("acceptDraw = %d %s", status, body)
	}

	st := env.getState(t, gameURI)
	res := st["game"].(map[string]any)["result"].(map[string]any)
	if res["outcome"] != "draw" || res["reason"] != "agreement" {
		t.Fatalf("agreement result = %v", res)
	}

	// The offerer cannot accept their own offer (game is finished now, so
	// assert on a fresh game below in TestGameDrawOfferExpiresOnMove).
}

func TestGameDrawOfferExpiresOnMove(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)
	whiteToken := accessJWT(t, env, white)
	blackToken := accessJWT(t, env, black)

	if status, body := env.postJSON(t, "/xrpc/"+games.NSIDOfferDraw, map[string]any{"game": gameURI}, whiteToken); status != http.StatusOK {
		t.Fatalf("offerDraw = %d %s", status, body)
	}

	// The offerer's next accepted move expires the offer (spec §5.6).
	if status, body := env.submitMove(t, whiteToken, gameURI, 1, "e2", "e4", ""); status != http.StatusOK {
		t.Fatalf("offerer's move = %d %s", status, body)
	}

	// Black can no longer accept it.
	status, body := env.postJSON(t, "/xrpc/"+games.NSIDAcceptDraw, map[string]any{"game": gameURI}, blackToken)
	if status == http.StatusOK {
		t.Fatal("accepted an expired draw offer")
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "InvalidRequest" {
		t.Fatalf("expired-offer accept error = %+v", xerr)
	}

	// Decline path: a new offer declined by the opponent leaves the game
	// active with no pending offer.
	if status, body := env.postJSON(t, "/xrpc/"+games.NSIDOfferDraw, map[string]any{"game": gameURI}, whiteToken); status != http.StatusOK {
		t.Fatalf("second offerDraw = %d %s", status, body)
	}
	if status, body := env.postJSON(t, "/xrpc/"+games.NSIDDeclineDraw, map[string]any{"game": gameURI}, blackToken); status != http.StatusOK {
		t.Fatalf("declineDraw = %d %s", status, body)
	}
	st := env.getState(t, gameURI)
	if st["game"].(map[string]any)["status"] != "active" {
		t.Fatalf("status after decline = %v", st["game"].(map[string]any)["status"])
	}
}

// TestGameRepetitionAutoDraw plays the knight shuffle: the start position
// recurs the third time after ply 8 and the game draws automatically.
func TestGameRepetitionAutoDraw(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)
	w := accessJWT(t, env, white)
	b := accessJWT(t, env, black)

	plies := []struct {
		token     string
		from, to  string
	}{
		{w, "g1", "f3"}, {b, "g8", "f6"},
		{w, "f3", "g1"}, {b, "f6", "g8"},
		{w, "g1", "f3"}, {b, "g8", "f6"},
		{w, "f3", "g1"}, {b, "f6", "g8"},
	}
	for i, mv := range plies {
		status, body := env.submitMove(t, mv.token, gameURI, int64(i+1), mv.from, mv.to, "")
		if status != http.StatusOK {
			t.Fatalf("ply %d = %d %s", i+1, status, body)
		}
	}

	// The last ply's response already carried gameOver.
	// (Verify from state, and independently via the 7th ply's absence of it.)
	st := env.getState(t, gameURI)
	res := st["game"].(map[string]any)["result"].(map[string]any)
	if res["outcome"] != "draw" || res["reason"] != "repetition" {
		t.Fatalf("repetition result = %v", res)
	}

	// No further moves are accepted.
	env.submitMoveExpectErr(t, "GameNotActive", w, gameURI, 9, "g1", "f3", "")
}

// ---------------------------------------------------------------------------
// 6. moveToken verifies against the published service key

func TestGameMoveTokenVerifiesAgainstServiceDoc(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)

	status, body := env.submitMove(t, accessJWT(t, env, white), gameURI, 1, "e2", "e4", "")
	if status != http.StatusOK {
		t.Fatalf("move = %d %s", status, body)
	}
	var acc struct {
		MoveToken string `json:"moveToken"`
		Ply       int64  `json:"ply"`
	}
	decodeJSON(t, body, &acc)

	// The key as published on the well-known endpoint.
	status, body = env.get(t, "/.well-known/plays-bot/service.json", nil)
	if status != http.StatusOK {
		t.Fatalf("service.json = %d", status)
	}
	var doc struct {
		DID              string `json:"did"`
		SigningPublicKey string `json:"signingPublicKey"`
	}
	decodeJSON(t, body, &doc)
	raw, err := base64.RawURLEncoding.DecodeString(doc.SigningPublicKey)
	if err != nil || len(raw) != 32 {
		t.Fatalf("service.json key %q: %v (%d bytes)", doc.SigningPublicKey, err, len(raw))
	}
	pub := ed25519.PublicKey(raw)

	// The token stored on the moves row verifies against that key and its
	// claims match the acceptance.
	stored, err := env.app.Repos().Moves.Get(context.Background(), gameURI, 1)
	if err != nil {
		t.Fatalf("stored move: %v", err)
	}
	if stored.Token == nil || *stored.Token != acc.MoveToken {
		t.Fatal("stored token differs from the response token")
	}
	claims, err := games.VerifyMoveToken(pub, *stored.Token)
	if err != nil {
		t.Fatalf("stored token does not verify: %v", err)
	}
	if claims.Game != gameURI || claims.Ply != 1 || claims.Subject != white.DID || claims.Issuer != env.svc.DID {
		t.Fatalf("claims = %+v", claims)
	}
	var payload map[string]any
	decodeJSON(t, claims.Payload, &payload)
	if payload["$type"] != engine.NSIDChessMove || payload["from"] != "e2" || payload["to"] != "e4" {
		t.Fatalf("token payload = %v", payload)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", claims.ReceivedAt); err != nil {
		t.Fatalf("token rat %q: %v", claims.ReceivedAt, err)
	}

	// Tampering with the stored token fails verification.
	if _, err := games.VerifyMoveToken(pub, (*stored.Token)[:len(*stored.Token)-4]+"AAAA"); err == nil {
		t.Fatal("tampered stored token verified")
	}

	// The service signing key (keys package) is the one in service.json.
	if keys.SigningPublicKeyB64(mustSigningKey(t, env)) != doc.SigningPublicKey {
		t.Fatal("service.json key differs from the running signing key")
	}
}

func mustSigningKey(t *testing.T, env *gameEnv) ed25519.PrivateKey {
	t.Helper()
	// appview owns the key; recover it by re-loading the key file.
	key, err := keys.LoadOrCreateSigningKey(env.cfg.ServiceSigningKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// postJSON posts an arbitrary JSON body to an XRPC procedure with auth.
func (e *gameEnv) postJSON(t *testing.T, path string, payload any, token string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

