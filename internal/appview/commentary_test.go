// Integration tests for Phase F: commentary escrow, reveal scheduling,
// receipt tokens, key publication, and commentary_reads capture (spec
// §4.6–§4.7, §5.4, §5.7, §8, §10).
//
// The Go test drives the agent role (generate key → encrypt → wrap → write
// the repo record through the harness PDS), mirroring what the TS SDK will
// do with the same escrow suite.
package appview_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/xrpc"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/escrow"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/testutil"
	"github.com/haileyok/botplaysbot/internal/tid"
)

const (
	commentaryNSID = "bot.plays.bot.game.commentary"
	revealNSID     = "bot.plays.bot.game.reveal"
)

// ---------------------------------------------------------------------------
// agent-side crypto helper (mirrors the planned TS SDK)

// bytesField renders a lexicon bytes value for repo JSON.
func bytesField(b []byte) map[string]any {
	return map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(b)}
}

// agentNote is one sealed commentary prepared the way an agent would.
type agentNote struct {
	contentKey escrow.ContentKey
	ciphertext []byte
	nonce      [24]byte
	escrowKey  map[string]any
	text       string
}

// sealCommentary generates a content key, encrypts text with the canonical
// AAD, and wraps the key to the given escrow rotation (spec §8.1) — the
// exact construction the TS SDK will implement.
func sealCommentary(t *testing.T, escrowKey *keys.EscrowKey, gameURI string, ply int64, playerDID, keyID, text string) *agentNote {
	t.Helper()

	ck, err := escrow.GenerateContentKey()
	if err != nil {
		t.Fatalf("content key: %v", err)
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	ct, err := escrow.Encrypt(ck, nonce, []byte(text), escrow.AAD(gameURI, ply, playerDID))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	wk, err := escrow.WrapContentKey(escrowKey.PublicKey, eph, ck)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return &agentNote{
		contentKey: ck,
		ciphertext: ct,
		nonce:      nonce,
		text:       text,
		escrowKey: map[string]any{
			"rotationId":         escrowKey.RotationID,
			"ephemeralPublicKey": bytesField(wk.EphemeralPublicKey[:]),
			"wrappedKey":         bytesField(wk.WrappedKey),
		},
	}
}

// commentaryRecord builds the repo record for a note.
func (n *agentNote) commentaryRecord(gameURI, gameCID string, ply *int64, visibility, keyID string) map[string]any {
	rec := map[string]any{
		"$type":      commentaryNSID,
		"game":       map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": gameCID},
		"visibility": visibility,
		"createdAt":  time.Now().UTC().Format(time.RFC3339),
	}
	if ply != nil {
		rec["ply"] = *ply
	}
	rec["ciphertext"] = bytesField(n.ciphertext)
	rec["nonce"] = bytesField(n.nonce[:])
	rec["escrowKey"] = n.escrowKey
	rec["keyId"] = keyID
	return rec
}

// writeCommentary writes the record to the account's repo.
func writeCommentary(t *testing.T, env *gameEnv, a testutil.Account, rec map[string]any) string {
	t.Helper()
	uri, _, err := testutil.WriteRecord(context.Background(), env.harness.Client(t, a),
		a.DID, commentaryNSID, tid.Next().String(), rec)
	if err != nil {
		t.Fatalf("write commentary record: %v", err)
	}
	return uri
}

// ---------------------------------------------------------------------------
// polling helpers

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// testPool opens a direct pool to the isolated DB for row assertions.
func testPool(t *testing.T, env *gameEnv) *pgxpool.Pool {
	t.Helper()
	pool, err := db.OpenPool(context.Background(), env.cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open assertion pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// commentaryRow fetches one indexed commentary row.
func commentaryRow(t *testing.T, pool *pgxpool.Pool, uri string) (visibility string, text *string, ciphertext []byte, keyID *string, contentKey []byte, escrowStatus *string, revealsAtPly *int, receiptValid *bool, agentPublished, keyMismatch bool, found bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT visibility, text, ciphertext, key_id, content_key, escrow_status,
		        reveals_at_ply, receipt_valid, agent_published, key_mismatch
		 FROM commentary WHERE uri=$1`, uri).
		Scan(&visibility, &text, &ciphertext, &keyID, &contentKey, &escrowStatus,
			&revealsAtPly, &receiptValid, &agentPublished, &keyMismatch)
	if err != nil {
		return "", nil, nil, nil, nil, nil, nil, nil, false, false, false
	}
	found = true
	return
}

// waitForCommentaryRow polls until the indexer stored the record.
func waitForCommentaryRow(t *testing.T, pool *pgxpool.Pool, uri string) {
	t.Helper()
	eventually(t, 15*time.Second, "commentary row "+uri, func() bool {
		_, _, _, _, _, _, _, _, _, _, ok := commentaryRow(t, pool, uri)
		return ok
	})
}

// gameStateCommentary fetches the commentary array from getState.
func gameStateCommentary(t *testing.T, env *gameEnv, token, gameURI string) []map[string]any {
	t.Helper()
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	status, body := env.get(t, "/xrpc/"+games.NSIDGetState+"?game="+gameURIEscape(gameURI), headers)
	if status != http.StatusOK {
		t.Fatalf("getState = %d %s", status, body)
	}
	var out struct {
		Commentary []map[string]any `json:"commentary"`
	}
	decodeJSON(t, body, &out)
	return out.Commentary
}

// flagsOfKind returns the flags table rows of one kind for a subject.
func flagsOfKind(t *testing.T, pool *pgxpool.Pool, subjectDID, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM flags WHERE subject_did=$1 AND kind=$2`, subjectDID, kind).Scan(&n); err != nil {
		t.Fatalf("flags query: %v", err)
	}
	return n
}

// revealRecords lists the service repo's reveal records, parsed.
func revealRecords(t *testing.T, env *gameEnv) []playsbot.GameReveal {
	t.Helper()
	out, err := comatproto.RepoListRecords(context.Background(),
		&xrpc.Client{Host: env.cfg.PDSURL}, revealNSID, "", 50, env.svc.DID, false)
	if err != nil {
		t.Fatalf("listRecords(reveal): %v", err)
	}
	var recs []playsbot.GameReveal
	for _, r := range out.Records {
		var rec playsbot.GameReveal
		if err := json.Unmarshal(r.Value, &rec); err != nil {
			t.Fatalf("decode reveal record: %v", err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// gameEnv extensions: chess game with a commentaryDelay override.
func newChessGameDelay(t *testing.T, env *gameEnv, white, black testutil.Account, perMoveSeconds int64, delay *config.CommentaryDelay) string {
	t.Helper()
	out, err := env.app.Games().CreateGame(context.Background(), games.CreateParams{
		GameType: engine.NSIDChessMove,
		Variant:  engine.ChessVariantStandard,
		Players: []games.PlayerSpec{
			{DID: white.DID, Seat: "white"},
			{DID: black.DID, Seat: "black"},
		},
		TimeControl:     clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: perMoveSeconds},
		CommentaryDelay: delay,
		StartedAt:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	return out.URI
}

// postCommentary calls the §5.7 procedure; returns status + raw body.
func (e *gameEnv) postCommentary(t *testing.T, token string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.server.URL+"/xrpc/"+games.NSIDPostCommentary, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("postCommentary: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

// currentEscrow returns the appview's current escrow rotation.
func currentEscrow(t *testing.T, env *gameEnv) *keys.EscrowKey {
	t.Helper()
	k, err := env.app.Escrow().Current(context.Background())
	if err != nil {
		t.Fatalf("escrow current: %v", err)
	}
	return k
}

// ---------------------------------------------------------------------------
// 1. Public commentary: visible immediately + #commentaryPosted

func TestPublicCommentaryImmediateVisibility(t *testing.T) {
	env, white, _ := bootGameEnv(t)
	gameURI := newChessGame(t, env, white, white, 300)
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)

	sub := env.app.Games().Bus().Subscribe(events.SubscribeOptions{GameURI: gameURI})
	defer env.app.Games().Bus().Close(sub)

	ply := int64(3)
	rec := map[string]any{
		"$type":      commentaryNSID,
		"game":       map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": cid},
		"ply":        ply,
		"visibility": "public",
		"text":       "e4 to control the center",
		"createdAt":  time.Now().UTC().Format(time.RFC3339),
	}
	uri := writeCommentary(t, env, white, rec)
	waitForCommentaryRow(t, pool, uri)

	// getState shows it revealed immediately.
	eventually(t, 20*time.Second, "public commentary visible in getState", func() bool {
		for _, e := range gameStateCommentary(t, env, "", gameURI) {
			if e["revealed"] == true && e["text"] == "e4 to control the center" && e["visibility"] == "public" {
				return true
			}
		}
		return false
	})

	// #commentaryPosted emitted, without text.
	ev := expectEvent(t, sub, events.KindCommentaryPosted)
	if ev.CommentaryPosted.Visibility != "public" || ev.CommentaryPosted.RevealsAt != nil {
		t.Fatalf("commentaryPosted = %+v", ev.CommentaryPosted)
	}
}

// ---------------------------------------------------------------------------
// 2. Delayed: ply-bound reveal, then the timer branch

func TestDelayedCommentaryPlyBoundReveal(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGameDelay(t, env, white, black, 300, &config.CommentaryDelay{Plies: 2, Seconds: 300})
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	sub := env.app.Games().Bus().Subscribe(events.SubscribeOptions{GameURI: gameURI})
	defer env.app.Games().Bus().Close(sub)

	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "I plan Nf3 next")
	uri := writeCommentary(t, env, white, note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1"))
	waitForCommentaryRow(t, pool, uri)

	// Hidden before the bound: no text, revealsAtPly = 1+2 = 3.
	_, _, _, _, _, _, revealsAtPly, _, _, _, _ := commentaryRow(t, pool, uri)
	if revealsAtPly == nil || *revealsAtPly != 3 {
		t.Fatalf("revealsAtPly = %v, want 3", revealsAtPly)
	}
	for _, e := range gameStateCommentary(t, env, "", gameURI) {
		if e["revealed"] != false || e["text"] != nil {
			t.Fatalf("delayed commentary leaked: %v", e)
		}
		if e["revealsAtPly"] == nil || e["revealsAtPly"].(float64) != 3 {
			t.Fatalf("revealsAtPly missing from getState entry: %v", e)
		}
	}

	// Accept plies 1-3; the reveal fires when ply 3 is accepted.
	plays := []struct{ token string; from, to string }{
		{accessJWT(t, env, white), "e2", "e4"},
		{accessJWT(t, env, black), "e7", "e5"},
		{accessJWT(t, env, white), "g1", "f3"},
	}
	for i, mv := range plays {
		if status, body := env.submitMove(t, mv.token, gameURI, int64(i+1), mv.from, mv.to, ""); status != http.StatusOK {
			t.Fatalf("move %d: %d %s", i+1, status, body)
		}
	}

	eventually(t, 20*time.Second, "delayed commentary revealed after ply N+P", func() bool {
		for _, e := range gameStateCommentary(t, env, "", gameURI) {
			if e["revealed"] == true && e["text"] == "I plan Nf3 next" {
				return true
			}
		}
		return false
	})
	waitEvent(t, sub, events.KindCommentaryRevealed, 20*time.Second)
}

// waitEvent reads events (skipping other kinds) until one of the wanted
// kind arrives or the timeout passes.
func waitEvent(t *testing.T, sub *events.Subscription, wantKind string, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatalf("event bus closed waiting for %s", wantKind)
			}
			if ev.Kind == wantKind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", wantKind)
			return events.Event{}
		}
	}
}

func TestDelayedCommentaryTimerBoundReveal(t *testing.T) {
	env, white, black := bootGameEnv(t)
	// Long ply bound, short seconds bound: the timer must do the reveal.
	gameURI := newChessGameDelay(t, env, white, black, 300, &config.CommentaryDelay{Plies: 50, Seconds: 2})
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "timer reveal")
	uri := writeCommentary(t, env, white, note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1"))
	waitForCommentaryRow(t, pool, uri)

	// No moves are played at all; the 1s scheduler reveals via the time
	// bound (ingest receivedAt + 2s).
	eventually(t, 15*time.Second, "timer-bound reveal without further moves", func() bool {
		for _, e := range gameStateCommentary(t, env, "", gameURI) {
			if e["revealed"] == true && e["text"] == "timer reveal" {
				return true
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// 3. Sealed: never before game end; reveal record at end

func TestSealedCommentaryRevealRecordAtGameEnd(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGameDelay(t, env, white, black, 300, &config.CommentaryDelay{Plies: 2, Seconds: 300})
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "sealed plan: rook lift")
	uri := writeCommentary(t, env, white, note.commentaryRecord(gameURI, cid, &ply, "sealed", "k1"))
	waitForCommentaryRow(t, pool, uri)

	// Moves continue; the sealed note stays hidden.
	tokenW := accessJWT(t, env, white)
	tokenB := accessJWT(t, env, black)
	if status, body := env.submitMove(t, tokenW, gameURI, 1, "e2", "e4", ""); status != http.StatusOK {
		t.Fatalf("move 1: %d %s", status, body)
	}
	if status, body := env.submitMove(t, tokenB, gameURI, 2, "e7", "e5", ""); status != http.StatusOK {
		t.Fatalf("move 2: %d %s", status, body)
	}
	for _, e := range gameStateCommentary(t, env, "", gameURI) {
		if e["revealed"] == true {
			t.Fatalf("sealed commentary revealed before game end: %v", e)
		}
	}

	// Game end (resignation): reveal + record.
	if _, err := env.app.Games().Resign(context.Background(), gameURI, white.DID); err != nil {
		t.Fatalf("resign: %v", err)
	}

	eventually(t, 10*time.Second, "sealed commentary revealed at game end", func() bool {
		for _, e := range gameStateCommentary(t, env, "", gameURI) {
			if e["revealed"] == true && e["text"] == "sealed plan: rook lift" {
				return true
			}
		}
		return false
	})

	var foundRec *playsbot.GameReveal
	eventually(t, 10*time.Second, "reveal record published", func() bool {
		for _, rec := range revealRecords(t, env) {
			if rec.Game.URI == gameURI {
				foundRec = &rec
				return true
			}
		}
		return false
	})
	if foundRec.Reason != "gameEnd" {
		t.Fatalf("reveal reason = %s, want gameEnd", foundRec.Reason)
	}
	if len(foundRec.Keys) != 1 {
		t.Fatalf("reveal keys = %+v, want 1", foundRec.Keys)
	}
	k := foundRec.Keys[0]
	if k.Player != white.DID || k.KeyId != "k1" {
		t.Fatalf("revealed key = %+v", k)
	}
	if string(k.Key) != string(note.contentKey[:]) {
		t.Fatalf("revealed key bytes = %x, want %x", k.Key, note.contentKey[:])
	}
	if k.AgentPublished.ValOr(true) || k.Mismatch.ValOr(true) {
		t.Fatalf("publication flags = %+v, want both false", k)
	}

	// The row records the reveal (text + revealed_at).
	_, text, _, _, _, status, _, _, _, _, _ := commentaryRow(t, pool, uri)
	if status == nil || *status != "ok" || text == nil || *text != "sealed plan: rook lift" {
		t.Fatalf("row after reveal: text=%v status=%v", text, status)
	}
}

// ---------------------------------------------------------------------------
// 4. Escrow failure: escrowFailed status + info flag

func TestEscrowFailureStatusAndFlag(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGameDelay(t, env, white, black, 300, nil)
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	// (a) Unknown rotation: the wrap targets a rotation the server never
	// published.
	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "lost key")
	note.escrowKey["rotationId"] = "does-not-exist"
	uriUnknown := writeCommentary(t, env, white, note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1"))

	// (b) Tampered wrappedKey bit: unwrap fails against the real rotation.
	note2 := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k2", "tampered wrap")
	tampered := map[string]any{}
	for k, v := range note2.escrowKey {
		tampered[k] = v
	}
	if raw, ok := tampered["wrappedKey"].(map[string]any); ok {
		b, err := base64.RawStdEncoding.DecodeString(raw["$bytes"].(string))
		if err != nil {
			t.Fatal(err)
		}
		b[0] ^= 0x80
		tampered["wrappedKey"] = bytesField(b)
	}
	rec2 := note2.commentaryRecord(gameURI, cid, &ply, "delayed", "k2")
	rec2["escrowKey"] = tampered
	uriTampered := writeCommentary(t, env, white, rec2)

	for _, uri := range []string{uriUnknown, uriTampered} {
		waitForCommentaryRow(t, pool, uri)
		_, _, _, _, contentKey, escrowStatus, revealsAtPly, _, _, _, _ := commentaryRow(t, pool, uri)
		if escrowStatus == nil || *escrowStatus != "escrowFailed" {
			t.Fatalf("%s: escrow_status = %v, want escrowFailed", uri, escrowStatus)
		}
		if contentKey != nil {
			t.Fatalf("%s: content_key stored despite failure", uri)
		}
		if revealsAtPly != nil {
			t.Fatalf("%s: escrowFailed row scheduled a reveal", uri)
		}
	}
	eventually(t, 10*time.Second, "escrowFailed info flag", func() bool {
		// Exactly-once per (subject, kind, game): two failed notes in one
		// game collapse into a single flag assertion.
		return flagsOfKind(t, pool, white.DID, "escrowFailed") >= 1
	})
}

// ---------------------------------------------------------------------------
// 5. Key publication: agentPublished and keyMismatch

func TestKeyPublicationMatchingAndMismatch(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGameDelay(t, env, white, black, 300, nil)
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	ply := int64(1)
	good := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "honest note")
	goodURI := writeCommentary(t, env, white, good.commentaryRecord(gameURI, cid, &ply, "delayed", "k1"))
	waitForCommentaryRow(t, pool, goodURI)

	// The agent publishes the matching key as public commentary (§4.7).
	pubRec := map[string]any{
		"$type":      commentaryNSID,
		"game":       map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": cid},
		"ply":        ply,
		"visibility": "public",
		"text":       base64.StdEncoding.EncodeToString(good.contentKey[:]),
		"keyId":      "k1",
		"createdAt":  time.Now().UTC().Format(time.RFC3339),
	}
	writeCommentary(t, env, white, pubRec)

	// A second, dishonest publication: same keyId, different key.
	other := sealCommentary(t, escrowKey, gameURI, ply, black.DID, "k9", "black note")
	otherURI := writeCommentary(t, env, black, other.commentaryRecord(gameURI, cid, &ply, "sealed", "k9"))
	waitForCommentaryRow(t, pool, otherURI)
	fakeKey := make([]byte, 32)
	fakeKey[0] = 0xAA
	fakeKey[31] = 0x55
	badPub := map[string]any{
		"$type":      commentaryNSID,
		"game":       map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": cid},
		"ply":        ply,
		"visibility": "public",
		"text":       base64.StdEncoding.EncodeToString(fakeKey),
		"keyId":      "k9",
		"createdAt":  time.Now().UTC().Format(time.RFC3339),
	}
	writeCommentary(t, env, black, badPub)

	// Publication detection must complete before the game ends (the marks
	// feed the reveal record).
	eventually(t, 15*time.Second, "agentPublished mark on escrowed row", func() bool {
		_, _, _, _, _, _, _, _, agentPublished, _, _ := commentaryRow(t, pool, goodURI)
		return agentPublished
	})
	eventually(t, 15*time.Second, "keyMismatch flag", func() bool {
		return flagsOfKind(t, pool, black.DID, "keyMismatch") >= 1
	})

	// Finish the game; the reveal record distinguishes the two keys.
	if _, err := env.app.Games().Resign(context.Background(), gameURI, white.DID); err != nil {
		t.Fatalf("resign: %v", err)
	}

	var rec *playsbot.GameReveal
	eventually(t, 10*time.Second, "reveal record", func() bool {
		for _, r := range revealRecords(t, env) {
			if r.Game.URI == gameURI {
				rec = &r
				return true
			}
		}
		return false
	})
	byKey := map[string]playsbot.GameReveal_RevealedKey{}
	for _, k := range rec.Keys {
		byKey[k.KeyId] = k
	}
	goodKey, ok := byKey["k1"]
	if !ok {
		t.Fatalf("k1 missing from reveal: %+v", rec.Keys)
	}
	if !goodKey.AgentPublished.ValOr(false) || goodKey.Mismatch.ValOr(true) {
		t.Fatalf("k1 = %+v, want agentPublished, no mismatch", goodKey)
	}
	badKey := byKey["k9"]
	if badKey.AgentPublished.ValOr(true) || !badKey.Mismatch.ValOr(false) {
		t.Fatalf("k9 = %+v, want mismatch", badKey)
	}
	if string(badKey.Key) != string(other.contentKey[:]) {
		t.Fatal("k9 reveal key is not the escrowed content key")
	}
	if flagsOfKind(t, pool, black.DID, "keyMismatch") < 1 {
		t.Fatal("keyMismatch flag not asserted")
	}
}

// ---------------------------------------------------------------------------
// 6. postCommentary roundtrip + receipt provenance

func TestPostCommentaryRoundtripAndReceipts(t *testing.T) {
	env, white, black := bootGameEnv(t)
	gameURI := newChessGameDelay(t, env, white, black, 300, nil)
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)
	token := accessJWT(t, env, white)

	// (a) Good call: ok, keyId, receiptToken that verifies.
	ply := int64(2)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "receipted note")
	body := map[string]any{
		"game":       map[string]any{"uri": gameURI, "cid": cid},
		"ply":        ply,
		"visibility": "delayed",
		"ciphertext": bytesField(note.ciphertext),
		"nonce":      bytesField(note.nonce[:]),
		"keyId":      "k1",
		"escrowKey":  note.escrowKey,
	}
	status, respBody := env.postCommentary(t, token, body)
	if status != http.StatusOK {
		t.Fatalf("postCommentary = %d %s", status, respBody)
	}
	var out struct {
		Ok           bool   `json:"ok"`
		KeyId        string `json:"keyId"`
		ReceiptToken string `json:"receiptToken"`
	}
	decodeJSON(t, respBody, &out)
	if !out.Ok || out.KeyId != "k1" || out.ReceiptToken == "" {
		t.Fatalf("postCommentary output = %+v", out)
	}
	priv, err := keys.LoadOrCreateSigningKey(env.cfg.ServiceSigningKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := games.VerifyReceiptToken(priv.Public().(ed25519.PublicKey), out.ReceiptToken)
	if err != nil {
		t.Fatalf("receipt does not verify: %v", err)
	}
	if claims.Subject != white.DID || claims.Game != gameURI || claims.Ply != ply ||
		claims.Digest != games.ReceiptDigest(note.ciphertext) {
		t.Fatalf("receipt claims = %+v", claims)
	}

	// (b) Bad wrap → EscrowUnwrapFailed.
	bad := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k2", "bad wrap")
	bad.escrowKey["rotationId"] = "no-such-rotation"
	bodyBad := map[string]any{
		"game":       map[string]any{"uri": gameURI, "cid": cid},
		"visibility": "delayed",
		"ciphertext": bytesField(bad.ciphertext),
		"nonce":      bytesField(bad.nonce[:]),
		"keyId":      "k2",
		"escrowKey":  bad.escrowKey,
	}
	status, respBody = env.postCommentary(t, token, bodyBad)
	if status == http.StatusOK {
		t.Fatalf("postCommentary with bad wrap succeeded: %s", respBody)
	}
	var xerr struct {
		Error string `json:"error"`
	}
	decodeJSON(t, respBody, &xerr)
	if xerr.Error != "EscrowUnwrapFailed" {
		t.Fatalf("error = %s, want EscrowUnwrapFailed", xerr.Error)
	}

	// (c) Record written WITH the receiptToken: receiptValid=true.
	recWithToken := note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1")
	recWithToken["receiptToken"] = out.ReceiptToken
	uriWith := writeCommentary(t, env, white, recWithToken)
	waitForCommentaryRow(t, pool, uriWith)
	_, _, _, _, _, _, _, receiptValid, _, _, _ := commentaryRow(t, pool, uriWith)
	if receiptValid == nil || !*receiptValid {
		t.Fatalf("receiptValid = %v, want true", receiptValid)
	}

	// (d) Ciphertext modified after the receipt: receiptValid=false.
	receiptForA := out.ReceiptToken
	noteB := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k3", "tampered provenance")
	recTampered := noteB.commentaryRecord(gameURI, cid, &ply, "delayed", "k3")
	// Receipt binds noteB's ciphertext; the record ships different bytes.
	recTampered["receiptToken"] = receiptForA
	recTampered["ciphertext"] = bytesField(append([]byte(nil), noteB.ciphertext...))
	if raw, ok := recTampered["ciphertext"].(map[string]any); ok {
		b, _ := base64.RawStdEncoding.DecodeString(raw["$bytes"].(string))
		b[3] ^= 0x40
		recTampered["ciphertext"] = bytesField(b)
	}
	uriTampered := writeCommentary(t, env, white, recTampered)
	waitForCommentaryRow(t, pool, uriTampered)
	_, _, _, _, _, _, _, receiptValidB, _, _, _ := commentaryRow(t, pool, uriTampered)
	if receiptValidB == nil || *receiptValidB {
		t.Fatalf("receiptValid = %v, want false for tampered record", receiptValidB)
	}

	// (e) No receiptToken: provenance falls back (receipt_valid IS NULL).
	plain := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k4", "no receipt")
	uriPlain := writeCommentary(t, env, white, plain.commentaryRecord(gameURI, cid, &ply, "delayed", "k4"))
	waitForCommentaryRow(t, pool, uriPlain)
	_, _, _, _, _, _, _, receiptValidC, _, _, _ := commentaryRow(t, pool, uriPlain)
	if receiptValidC != nil {
		t.Fatalf("receiptValid = %v, want NULL without receiptToken", receiptValidC)
	}
}

// ---------------------------------------------------------------------------
// 7. commentary_reads capture (§10)

func TestCommentaryReadsLogged(t *testing.T) {
	env, white, black := bootGameEnv(t)
	// Pin a long delay: the zero-value tunables in testConfig would reveal
	// immediately, and this test needs the note to stay unrevealed.
	gameURI := newChessGameDelay(t, env, white, black, 300, &config.CommentaryDelay{Plies: 50, Seconds: 3600})
	pool := testPool(t, env)
	cid := gameCID(t, env, gameURI)
	escrowKey := currentEscrow(t, env)

	ply := int64(1)
	note := sealCommentary(t, escrowKey, gameURI, ply, white.DID, "k1", "watched note")
	uri := writeCommentary(t, env, white, note.commentaryRecord(gameURI, cid, &ply, "delayed", "k1"))
	waitForCommentaryRow(t, pool, uri)

	// Authenticated read (the opponent).
	token := accessJWT(t, env, black)
	gameStateCommentary(t, env, token, gameURI)
	eventually(t, 20*time.Second, "authed commentary_reads row", func() bool {
		var did *string
		var ip *string
		err := pool.QueryRow(context.Background(),
			`SELECT requester_did, ip::text FROM commentary_reads WHERE commentary_uri=$1 AND requester_did IS NOT NULL LIMIT 1`, uri).
			Scan(&did, &ip)
		return err == nil && did != nil && *did == black.DID
	})

	// Anonymous read: captured by client IP instead.
	gameStateCommentary(t, env, "", gameURI)
	eventually(t, 20*time.Second, "anonymous commentary_reads row with IP", func() bool {
		var did *string
		var ip *string
		err := pool.QueryRow(context.Background(),
			`SELECT requester_did, ip::text FROM commentary_reads WHERE commentary_uri=$1 AND requester_did IS NULL LIMIT 1`, uri).
			Scan(&did, &ip)
		return err == nil && ip != nil && *ip != ""
	})

	// No reads are logged for revealed (public) commentary.
	pubRec := map[string]any{
		"$type":      commentaryNSID,
		"game":       map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": cid},
		"visibility": "public",
		"text":       "open note",
		"createdAt":  time.Now().UTC().Format(time.RFC3339),
	}
	pubURI := writeCommentary(t, env, white, pubRec)
	waitForCommentaryRow(t, pool, pubURI)
	gameStateCommentary(t, env, "", gameURI)
	time.Sleep(500 * time.Millisecond)
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM commentary_reads WHERE commentary_uri=$1`, pubURI).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("public commentary generated %d read rows, want 0", n)
	}
}
