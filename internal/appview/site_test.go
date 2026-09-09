// Integration tests for the Phase G site JSON APIs (AC.13): boot the
// appview + PDS + isolated DB, create a game, seed commentary rows in
// various states through the real indexer path, and assert the /api/games,
// /api/game, /api/actors, and /api/challenges responses — plus the
// spectator WS → #move frame → API state pull round-trip and the
// finished-game reveal summary.
package appview_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/haileyok/botplaysbot/internal/games"
)

// siteGet fetches one of the site JSON endpoints.
func (e *gameEnv) siteGet(t *testing.T, path string) (int, []byte) {
	t.Helper()
	return e.get(t, path, nil)
}

// TestSiteGamesListAndDetail (AC.13): the site APIs reflect real game state,
// with the receipt enrichment and the spectator WS round-trip.
func TestSiteGamesListAndDetail(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)

	// /api/games lists the active game with players + seats and clocks.
	status, body := env.siteGet(t, "/api/games")
	if status != http.StatusOK {
		t.Fatalf("/api/games = %d %s", status, body)
	}
	var games struct {
		Games []struct {
			URI     string `json:"uri"`
			Status  string `json:"status"`
			Ply     int64  `json:"ply"`
			Players []struct {
				DID  string `json:"did"`
				Seat string `json:"seat"`
			} `json:"players"`
			Clocks []struct {
				DID string `json:"did"`
			} `json:"clocks"`
		} `json:"games"`
	}
	decodeJSON(t, body, &games)
	var found bool
	for _, g := range games.Games {
		if g.URI == gameURI {
			found = true
			if g.Status != "active" || g.Ply != 0 {
				t.Fatalf("listed game = %+v", g)
			}
			if len(g.Players) != 2 || g.Players[0].Seat != "white" || g.Players[1].Seat != "black" {
				t.Fatalf("players = %+v", g.Players)
			}
		}
	}
	if !found {
		t.Fatalf("game %s not in /api/games: %s", gameURI, body)
	}

	// /api/game returns the full state before any move.
	status, body = env.siteGet(t, "/api/game?uri="+gameURI)
	if status != http.StatusOK {
		t.Fatalf("/api/game = %d %s", status, body)
	}
	var detail struct {
		FEN      string `json:"-"` // via position
		Position *struct {
			FEN string `json:"fen"`
		} `json:"position"`
		Ply     int64  `json:"ply"`
		Turn    string `json:"turn"`
		History []any  `json:"history"`
		Reveal  *struct {
			Keys []map[string]any `json:"keys"`
		} `json:"reveal"`
		ServerTime string `json:"serverTime"`
	}
	decodeJSON(t, body, &detail)
	if detail.Position == nil || detail.Position.FEN == "" {
		t.Fatalf("no initial FEN: %s", body)
	}
	t.Logf("detail dump: %s", body)
	if detail.Ply != 0 || detail.Turn != white.DID || len(detail.History) != 0 {
		t.Fatalf("detail state = %+v", detail)
	}
	if detail.Reveal != nil {
		t.Fatal("active game must not carry a reveal summary")
	}
	if detail.ServerTime == "" {
		t.Fatal("serverTime missing")
	}

	// Spectator WS: connect unfiltered (firehose), drive a move via XRPC,
	// expect the #move frame, then assert the next /api/game pull reflects
	// the move.
	wsURL := "ws" + env.server.URL[len("http"):] + "/xrpc/bot.plays.bot.game.subscribe"
	wsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, wsURL, &websocket.DialOptions{Subprotocols: []string{"xrpc.v1.json"}})
	if err != nil {
		t.Fatalf("spectator dial: %v", err)
	}
	defer conn.CloseNow()

	wToken := accessJWT(t, env, white)
	if st, b := env.submitMove(t, wToken, gameURI, 1, "e2", "e4", ""); st != http.StatusOK {
		t.Fatalf("submitMove = %d %s", st, b)
	}

	// Read frames until the #move for ply 1 arrives.
	deadline := time.Now().Add(5 * time.Second)
	var moveFrame struct {
		Type    string `json:"$type"`
		Payload struct {
			Type  string `json:"$type"`
			Game  string `json:"game"`
			Ply   int64  `json:"ply"`
			San   string `json:"san"`
			Pos   *struct {
				FEN string `json:"fen"`
			} `json:"position"`
		} `json:"payload"`
	}
	sawMove := false
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithTimeout(wsCtx, time.Second)
		_, data, err := conn.Read(readCtx)
		readCancel()
		if err != nil {
			continue
		}
		var frame struct {
			Type    string          `json:"$type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &frame) != nil || frame.Type != "message" {
			continue
		}
		if json.Unmarshal(frame.Payload, &moveFrame.Payload) != nil {
			continue
		}
		moveFrame.Type = moveFrame.Payload.Type
		if moveFrame.Payload.Type == "bot.plays.bot.game.subscribe#move" && moveFrame.Payload.Ply == 1 && moveFrame.Payload.Game == gameURI {
			sawMove = true
			if moveFrame.Payload.San != "e4" {
				t.Fatalf("move frame san = %q", moveFrame.Payload.San)
			}
			if moveFrame.Payload.Pos == nil || moveFrame.Payload.Pos.FEN == "" {
				t.Fatal("move frame carries no position")
			}
			break
		}
	}
	if !sawMove {
		t.Fatal("spectator WS never received the #move frame")
	}
	conn.CloseNow()

	// The subsequent API pull reflects the move.
	status, body = env.siteGet(t, "/api/game?uri="+gameURI)
	if status != http.StatusOK {
		t.Fatalf("/api/game after move = %d %s", status, body)
	}
	decodeJSON(t, body, &detail)
	if detail.Ply != 1 || len(detail.History) != 1 {
		t.Fatalf("after-move detail ply/history = %d/%d", detail.Ply, len(detail.History))
	}
}

// TestSiteGameCommentaryAndReveal (AC.13 cont.): commentary rows in various
// states surface through /api/game with the receipt enrichment, and the
// finished game carries the reveal summary for the verify affordance.
func TestSiteGameCommentaryAndReveal(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, black, 300)
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)
	wToken := accessJWT(t, env, white)

	// Agent role: seal a note, validate via postCommentary (gets a receipt
	// token), write the record with the receipt attached.
	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "planned Nf3")
	body := map[string]any{
		"game":       map[string]any{"uri": gameURI, "cid": cid},
		"ply":        ply,
		"visibility": "delayed",
		"ciphertext": bytesField(note.ciphertext),
		"nonce":      bytesField(note.nonce[:]),
		"keyId":      "k1",
		"escrowKey":  note.escrowKey,
	}
	status, respBody := env.postCommentary(t, wToken, body)
	if status != http.StatusOK {
		t.Fatalf("postCommentary = %d %s", status, respBody)
	}
	var post struct {
		Ok           bool   `json:"ok"`
		KeyId        string `json:"keyId"`
		ReceiptToken string `json:"receiptToken"`
	}
	decodeJSON(t, respBody, &post)
	if !post.Ok || post.ReceiptToken == "" {
		t.Fatalf("postCommentary output = %+v", post)
	}
	rec := note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1")
	rec["receiptToken"] = post.ReceiptToken
	uri := writeCommentary(t, env, white, rec)
	waitForCommentaryRow(t, pool, uri)

	// /api/game: the delayed note is ingested with the receipt enrichment
	// (receiptValid=true, receiptAt stamped). With no moves played yet, the
	// timer bound may already have revealed it — accept either the hidden
	// form (revealsAtPly=3) or the revealed form.
	eventually(t, 10*time.Second, "site /api/game shows the delayed note with receipt", func() bool {
		st, b := env.siteGet(t, "/api/game?uri="+gameURI)
		if st != http.StatusOK {
			return false
		}
		var d struct {
			Commentary []struct {
				Visibility   string  `json:"visibility"`
				Revealed     bool    `json:"revealed"`
				Text         *string `json:"text"`
				RevealsAtPly *int64  `json:"revealsAtPly"`
				ReceiptValid *bool   `json:"receiptValid"`
				ReceiptAt    *string `json:"receiptAt"`
			} `json:"commentary"`
		}
		decodeJSON(t, b, &d)
		for _, c := range d.Commentary {
			if c.Visibility == "delayed" && c.ReceiptValid != nil && *c.ReceiptValid && c.ReceiptAt != nil {
				if (!c.Revealed && c.RevealsAtPly != nil && *c.RevealsAtPly == 3) || (c.Revealed && c.Text != nil) {
					return true
				}
			}
		}
		return false
	})

	// Finish the game: white resigns. The reveal pass runs and the reveal
	// summary appears with the unwrapped key and per-note ciphertext digest.
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/xrpc/"+games.NSIDResign,
		strings.NewReader(`{"game":"`+gameURI+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wToken)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var detail struct {
		Status string `json:"status"`
		Result *struct {
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		} `json:"result"`
		Commentary []struct {
			Revealed bool    `json:"revealed"`
			Text     *string `json:"text"`
		} `json:"commentary"`
		Reveal *struct {
			Reason string `json:"reason"`
			Keys   []struct {
				Player  string `json:"player"`
				KeyID   string `json:"keyId"`
				Key     string `json:"key"`
			} `json:"keys"`
			Ciphertexts []struct {
				Ply        int64  `json:"ply"`
				Player     string `json:"player"`
				Ciphertext string `json:"ciphertext"`
				Nonce      string `json:"nonce"`
				KeyID      string `json:"keyId"`
				Sha256     string `json:"sha256"`
			} `json:"ciphertexts"`
		} `json:"reveal"`
	}
	eventually(t, 10*time.Second, "finished game carries the reveal summary", func() bool {
		st, b := env.siteGet(t, "/api/game?uri="+gameURI)
		if st != http.StatusOK {
			return false
		}
		detail = struct {
			Status string `json:"status"`
			Result *struct {
				Outcome string `json:"outcome"`
				Reason  string `json:"reason"`
			} `json:"result"`
			Commentary []struct {
				Revealed bool    `json:"revealed"`
				Text     *string `json:"text"`
			} `json:"commentary"`
			Reveal *struct {
				Reason string `json:"reason"`
				Keys   []struct {
					Player  string `json:"player"`
					KeyID   string `json:"keyId"`
					Key     string `json:"key"`
				} `json:"keys"`
				Ciphertexts []struct {
					Ply        int64  `json:"ply"`
					Player     string `json:"player"`
					Ciphertext string `json:"ciphertext"`
					Nonce      string `json:"nonce"`
					KeyID      string `json:"keyId"`
					Sha256     string `json:"sha256"`
				} `json:"ciphertexts"`
			} `json:"reveal"`
		}{}
		decodeJSON(t, b, &detail)
		return detail.Status == "finished" && detail.Reveal != nil && len(detail.Reveal.Keys) > 0
	})

	if detail.Status != "finished" || detail.Result == nil || detail.Result.Outcome != "win" {
		t.Fatalf("finished detail = %+v %+v", detail.Status, detail.Result)
	}

	// The escrowed note is revealed at game end with its plaintext.
	foundText := false
	for _, c := range detail.Commentary {
		if c.Revealed && c.Text != nil && *c.Text == "planned Nf3" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("note not revealed at game end: %+v", detail.Commentary)
	}

	// Verify affordance data: the key + ciphertext digest round-trip. The
	// server digest must equal the locally computed sha256 of the
	// ciphertext bytes (what the browser recomputes via WebCrypto).
	rev := detail.Reveal
	var key *struct {
		Player string `json:"player"`
		KeyID  string `json:"keyId"`
		Key    string `json:"key"`
	}
	for i := range rev.Keys {
		if rev.Keys[i].KeyID == "k1" {
			key = &rev.Keys[i]
		}
	}
	if key == nil {
		t.Fatalf("reveal keys missing k1: %+v", rev.Keys)
	}
	if _, err := base64.StdEncoding.DecodeString(key.Key); err != nil {
		t.Fatalf("reveal key is not base64: %v", err)
	}
	var noteCT *struct {
		Ply        int64  `json:"ply"`
		Player     string `json:"player"`
		Ciphertext string `json:"ciphertext"`
		Nonce      string `json:"nonce"`
		KeyID      string `json:"keyId"`
		Sha256     string `json:"sha256"`
	}
	for i := range rev.Ciphertexts {
		if rev.Ciphertexts[i].KeyID == "k1" && rev.Ciphertexts[i].Ply == 1 {
			noteCT = &rev.Ciphertexts[i]
		}
	}
	if noteCT == nil {
		t.Fatalf("reveal ciphertexts missing k1: %+v", rev.Ciphertexts)
	}
	rawCT, err := base64.StdEncoding.DecodeString(noteCT.Ciphertext)
	if err != nil {
		t.Fatalf("ciphertext not base64: %v", err)
	}
	sum := sha256.Sum256(rawCT)
	if got := hex.EncodeToString(sum[:]); got != noteCT.Sha256 {
		t.Fatalf("ciphertext digest = %s, want %s", got, noteCT.Sha256)
	}
}

// TestSiteActorsChallengesDocs covers the remaining site endpoints: actor
// page data, the open-challenge strip, and the docs index (policies +
// lexicon listing).
func TestSiteActorsChallengesDocs(t *testing.T) {
	env, white, _ := bootGameEnv(t)

	// Actor: the actors row only exists once the account wrote an
	// actor.profile record (the profile indexer ingests it). The harness
	// accounts don't write profiles, so the DID is unknown here — assert
	// the 404 shape; recent-games rendering is covered by the web unit
	// tests with a mocked row.
	st, b := env.siteGet(t, "/api/actors/"+white.DID)
	if st != http.StatusNotFound {
		t.Fatalf("unknown actor = %d %s, want 404", st, b)
	}

	// Challenges: none open → empty list, 200.
	st, b = env.siteGet(t, "/api/challenges?open=true")
	if st != http.StatusOK {
		t.Fatalf("/api/challenges = %d %s", st, b)
	}
	var ch struct {
		Challenges []map[string]any `json:"challenges"`
	}
	decodeJSON(t, b, &ch)
	if len(ch.Challenges) != 0 {
		t.Fatalf("challenges = %v, want empty", ch.Challenges)
	}

	// Docs: policies text present verbatim, lexicon listing non-empty.
	st, b = env.siteGet(t, "/docs")
	if st != http.StatusOK {
		t.Fatalf("/docs = %d %s", st, b)
	}
	var docs struct {
		Lexicons []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"lexicons"`
		Policies string `json:"policies"`
	}
	decodeJSON(t, b, &docs)
	if len(docs.Lexicons) == 0 {
		t.Fatal("docs lexicon listing is empty")
	}
	for _, want := range []string{
		"Deadline = `previousMove.receivedAt + perMoveSeconds`",
		"The arbiter holds escrowed keys solely to operate the broadcast delay",
		"Nothing stays private.",
		"The broadcast delay is policy-enforced, not cryptographic.",
	} {
		if !strings.Contains(docs.Policies, want) {
			t.Fatalf("policies missing verbatim %q", want)
		}
	}

	// One lexicon doc is served raw at its listed path.
	if st, _ := env.siteGet(t, docs.Lexicons[0].Path); st != http.StatusOK {
		t.Fatalf("lexicon doc %s = %d", docs.Lexicons[0].Path, st)
	}
}

