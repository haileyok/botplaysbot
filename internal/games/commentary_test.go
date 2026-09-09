package games

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// ---------------------------------------------------------------------------
// receiptToken (F.2)

func newTestSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func TestReceiptSignVerifyRoundtrip(t *testing.T) {
	priv := newTestSigningKey(t)
	receivedAt := time.Date(2026, 9, 8, 14, 3, 22, 115e6, time.UTC)
	tok, err := mintReceiptToken(priv, "did:web:bot.plays.bot", "did:plc:alpha",
		"at://did:web:bot.plays.bot/bot.plays.bot.game/abc", 3,
		ReceiptDigest([]byte("ciphertext")), receivedAt)
	if err != nil {
		t.Fatalf("mintReceiptToken: %v", err)
	}
	claims, err := VerifyReceiptToken(priv.Public().(ed25519.PublicKey), tok)
	if err != nil {
		t.Fatalf("VerifyReceiptToken: %v", err)
	}
	if claims.Issuer != "did:web:bot.plays.bot" ||
		claims.Subject != "did:plc:alpha" ||
		claims.Game != "at://did:web:bot.plays.bot/bot.plays.bot.game/abc" ||
		claims.Ply != 3 ||
		claims.Digest != ReceiptDigest([]byte("ciphertext")) ||
		claims.ReceivedAt != RFC3339Millis(receivedAt) {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

func TestReceiptClaimsExactness(t *testing.T) {
	// The payload carries exactly {iss, sub, game, ply, digest, rat, iat} —
	// no more, no less (Appendix B discipline applied to receipts).
	priv := newTestSigningKey(t)
	tok, err := mintReceiptToken(priv, "did:svc", "did:plc:p", "at://g", 0,
		"digest", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("receipt is not a compact JWS: %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"iss": true, "sub": true, "game": true, "ply": true, "digest": true, "rat": true, "iat": true}
	if len(m) != len(want) {
		t.Fatalf("payload claims = %v, want exactly %v", m, want)
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("payload missing claim %q: %v", k, m)
		}
	}
}

func TestReceiptDigestTamperDetected(t *testing.T) {
	// The receipt binds the ciphertext: verification of a modified
	// ciphertext against a receipt minted for the original fails at the
	// digest comparison (the indexer's caller-side check).
	priv := newTestSigningKey(t)
	original := []byte("original ciphertext")
	tok, err := mintReceiptToken(priv, "did:svc", "did:plc:p", "at://g", 2,
		ReceiptDigest(original), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyReceiptToken(priv.Public().(ed25519.PublicKey), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Digest == ReceiptDigest([]byte("original ciphertext TAMPERED")) {
		t.Fatal("digest collision for tampered ciphertext")
	}
}

func TestReceiptForgeryRejected(t *testing.T) {
	priv := newTestSigningKey(t)
	other := newTestSigningKey(t)
	tok, err := mintReceiptToken(other, "did:svc", "did:plc:p", "at://g", 1, "d", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReceiptToken(priv.Public().(ed25519.PublicKey), tok); err == nil {
		t.Fatal("token signed by a foreign key verified")
	}
	if _, err := VerifyReceiptToken(priv.Public().(ed25519.PublicKey), "not.a.token"); err == nil {
		t.Fatal("garbage token verified")
	}
}

// ---------------------------------------------------------------------------
// §4.6 field validation (F.2/F.3)

func TestValidateCommentaryFields(t *testing.T) {
	cases := []struct {
		name string
		f    CommentaryFields
		want bool // nil error?
	}{
		{"public ok", CommentaryFields{Visibility: VisibilityPublic, HasText: true}, true},
		{"public without text", CommentaryFields{Visibility: VisibilityPublic}, false},
		{"public with ciphertext", CommentaryFields{Visibility: VisibilityPublic, HasText: true, HasCiphertext: true}, false},
		{"delayed ok", CommentaryFields{Visibility: VisibilityDelayed, HasCiphertext: true, HasEscrowKey: true}, true},
		{"delayed without escrow", CommentaryFields{Visibility: VisibilityDelayed, HasCiphertext: true}, false},
		{"delayed without ciphertext", CommentaryFields{Visibility: VisibilityDelayed, HasEscrowKey: true}, false},
		{"sealed ok", CommentaryFields{Visibility: VisibilitySealed, HasCiphertext: true}, true},
		{"sealed with escrow ok", CommentaryFields{Visibility: VisibilitySealed, HasCiphertext: true, HasEscrowKey: true}, true},
		{"sealed without ciphertext", CommentaryFields{Visibility: VisibilitySealed}, false},
		{"unknown visibility", CommentaryFields{Visibility: "secret"}, false},
		{"empty visibility", CommentaryFields{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCommentaryFields(tc.f)
			if got := err == nil; got != tc.want {
				t.Fatalf("ValidateCommentaryFields(%+v) = %v, want valid=%v", tc.f, err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §8.2 reveal schedule (F.6 pure function)

func TestShouldReveal(t *testing.T) {
	delay := config.CommentaryDelay{Plies: 2, Seconds: 300}
	receivedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	S := time.Duration(delay.Seconds) * time.Second

	cases := []struct {
		name       string
		visibility string
		ply        int64
		currentPly int64
		now        time.Time
		want       bool
	}{
		{"delayed before ply bound", VisibilityDelayed, 10, 11, receivedAt.Add(time.Second), false},
		{"delayed at ply bound", VisibilityDelayed, 10, 12, receivedAt.Add(time.Second), true},
		{"delayed past ply bound", VisibilityDelayed, 10, 20, receivedAt.Add(time.Second), true},
		{"delayed before time bound", VisibilityDelayed, 10, 10, receivedAt.Add(S - time.Second), false},
		{"delayed at time bound", VisibilityDelayed, 10, 10, receivedAt.Add(S), true},
		{"delayed past time bound", VisibilityDelayed, 10, 10, receivedAt.Add(2 * S), true},
		{"sealed never (before end)", VisibilitySealed, 10, 50, receivedAt.Add(10 * S), false},
		{"public always", VisibilityPublic, 10, 10, receivedAt, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldReveal(tc.visibility, tc.ply, delay, receivedAt, tc.now, tc.currentPly)
			if got != tc.want {
				t.Fatalf("ShouldReveal(%s, ply=%d, currentPly=%d) = %v, want %v",
					tc.visibility, tc.ply, tc.currentPly, got, tc.want)
			}
		})
	}

	// "Whichever comes first": with a fast game, the ply bound hits long
	// before the time bound.
	fast := config.CommentaryDelay{Plies: 2, Seconds: 300}
	if !ShouldReveal(VisibilityDelayed, 4, fast, receivedAt, receivedAt.Add(time.Second), 6) {
		t.Fatal("ply-bound branch did not fire ahead of the time bound")
	}
	// And in a stalled game, the time bound fires without further moves.
	if !ShouldReveal(VisibilityDelayed, 4, fast, receivedAt, receivedAt.Add(301*time.Second), 5) {
		t.Fatal("time-bound branch did not fire without further moves")
	}
}

func TestDelayFor(t *testing.T) {
	def := config.CommentaryDelay{Plies: 2, Seconds: 300}
	if got := DelayFor(nil, def); got != def {
		t.Fatalf("DelayFor(nil) = %+v", got)
	}
	override := config.CommentaryDelay{Plies: 1, Seconds: 5}
	if got := DelayFor(&override, def); got != override {
		t.Fatalf("DelayFor(override) = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// key publication decoding

func TestDecodePublishedKey(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if got, ok := DecodePublishedKey(enc.EncodeToString(raw)); !ok || len(got) != 32 {
			t.Fatalf("DecodePublishedKey(%s) rejected", enc.EncodeToString(raw))
		}
	}
	// Short key rejected.
	if _, ok := DecodePublishedKey(base64.StdEncoding.EncodeToString(raw[:31])); ok {
		t.Fatal("31-byte key accepted")
	}
	// Non-base64 text rejected.
	if _, ok := DecodePublishedKey("I think e4 is strong here"); ok {
		t.Fatal("prose accepted as a key")
	}
	// Empty rejected.
	if _, ok := DecodePublishedKey(""); ok {
		t.Fatal("empty accepted as a key")
	}
}

// ---------------------------------------------------------------------------
// getState commentary summaries

func TestBuildCommentaryEntries(t *testing.T) {
	recv := time.Now().UTC()
	pub := "public"
	delayed := "delayed"
	ok := repo.EscrowOK
	invalid := repo.EscrowInvalid
	text := "e4! e5!!"
	reveals := recv.Add(time.Minute)
	ply2 := 2

	rows := []repo.Commentary{
		{URI: "at://x/1", Visibility: pub, Text: &text, RevealedAt: &recv, EscrowStatus: &ok, Ply: &ply2, PlayerDID: &pub},
		{URI: "at://x/2", Visibility: delayed, EscrowStatus: &ok, RevealsAt: &reveals, RevealsAtPly: &ply2, PlayerDID: &pub},
		{URI: "at://x/3", Visibility: delayed, EscrowStatus: &invalid, Text: &text, PlayerDID: &pub},
	}
	entries := buildCommentaryEntries(rows)
	if len(entries) != 2 {
		t.Fatalf("entries = %d (invalid row must not display), %+v", len(entries), entries)
	}
	pubEntry := entries[0]
	if !pubEntry.Revealed || pubEntry.Text == nil || *pubEntry.Text != text {
		t.Fatalf("public entry wrong: %+v", pubEntry)
	}
	if pubEntry.RevealsAt != nil || pubEntry.RevealsAtPly != nil {
		t.Fatalf("revealed entry carries bounds: %+v", pubEntry)
	}
	delayedEntry := entries[1]
	if delayedEntry.Revealed || delayedEntry.Text != nil {
		t.Fatalf("unrevealed delayed entry leaks text: %+v", delayedEntry)
	}
	if delayedEntry.RevealsAt == nil || delayedEntry.RevealsAtPly == nil || *delayedEntry.RevealsAtPly != 2 {
		t.Fatalf("delayed entry missing bounds: %+v", delayedEntry)
	}
}

// ---------------------------------------------------------------------------
// reveal record reason mapping

func TestRevealReason(t *testing.T) {
	cases := map[string]string{
		"timeout":        "abandonment",
		"abandonment":    "abandonment",
		"adjudication":   "adjudication",
		"checkmate":      "gameEnd",
		"resignation":    "gameEnd",
		"agreement":      "gameEnd",
		"fiftyMove":      "gameEnd",
		"(nil result)":   "gameEnd",
	}
	for reason, want := range cases {
		var r *events.Result
		if reason != "(nil result)" {
			r = &events.Result{Reason: reason}
		}
		if got := revealReason(r); got != want {
			t.Fatalf("revealReason(%s) = %s, want %s", reason, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// subscribe event rendering

func TestGameMessageCommentaryKinds(t *testing.T) {
	reveals := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	ply := int64(4)

	ev := events.Event{Kind: events.KindCommentaryPosted, GameURI: "at://g"}
	ev.CommentaryPosted.Ply = ply
	ev.CommentaryPosted.Player = "did:plc:p"
	ev.CommentaryPosted.Visibility = VisibilityDelayed
	ev.CommentaryPosted.RevealsAt = &reveals
	ev.CommentaryPosted.RevealsAtPly = &ply

	msg, fragment := gameMessage(ev)
	if fragment != "#commentaryPosted" || msg == nil || !msg.GameSubscribe_CommentaryPosted.HasVal() {
		t.Fatalf("commentaryPosted mapping failed: %s %+v", fragment, msg)
	}
	if msg.GameSubscribe_CommentaryPosted.Val().RevealsAt.ValOr("") != RFC3339Millis(reveals) {
		t.Fatalf("revealsAt not rendered: %+v", msg.GameSubscribe_CommentaryPosted.Val())
	}

	ev2 := events.Event{Kind: events.KindCommentaryRevealed, GameURI: "at://g"}
	ev2.CommentaryRevealed.Ply = ply
	ev2.CommentaryRevealed.Player = "did:plc:p"
	ev2.CommentaryRevealed.Text = "e4 was best"
	msg2, fragment2 := gameMessage(ev2)
	if fragment2 != "#commentaryRevealed" || msg2 == nil || !msg2.GameSubscribe_CommentaryRevealed.HasVal() {
		t.Fatalf("commentaryRevealed mapping failed: %s %+v", fragment2, msg2)
	}
	if msg2.GameSubscribe_CommentaryRevealed.Val().Text == "" {
		t.Fatal("revealed event dropped text")
	}
}

// ---------------------------------------------------------------------------
// client IP

func TestClientIPFromAddr(t *testing.T) {
	if got := ClientIPFromAddr("192.0.2.7:54321"); got != "192.0.2.7" {
		t.Fatalf("ClientIPFromAddr = %q", got)
	}
	if got := ClientIPFromAddr("192.0.2.7"); got != "192.0.2.7" {
		t.Fatalf("ClientIPFromAddr(no port) = %q", got)
	}
}
