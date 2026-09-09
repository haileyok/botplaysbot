package games

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/botplaysbot/internal/keys"
)

func base64URLEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func testPayload(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"$type": "bot.plays.bot.chess.move",
		"from":  "e2",
		"to":    "e4",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestMoveTokenRoundtrip: sign → verify with the same key; claims carry
// exactly the Appendix B fields.
func TestMoveTokenRoundtrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	receivedAt := time.Date(2026, 9, 8, 14, 3, 22, 115_000_000, time.UTC)
	payload := testPayload(t)

	token, err := mintMoveToken(priv, "did:web:bot.plays.bot", "did:plc:player", "at://did:web:bot.plays.bot/bot.plays.bot.game/3kab", 23, payload, receivedAt)
	if err != nil {
		t.Fatal(err)
	}

	// Compact JWS: three dot-separated b64url segments.
	if parts := strings.Split(token, "."); len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	claims, err := VerifyMoveToken(pub, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Issuer != "did:web:bot.plays.bot" {
		t.Fatalf("iss = %q", claims.Issuer)
	}
	if claims.Subject != "did:plc:player" {
		t.Fatalf("sub = %q", claims.Subject)
	}
	if claims.Game != "at://did:web:bot.plays.bot/bot.plays.bot.game/3kab" {
		t.Fatalf("game = %q", claims.Game)
	}
	if claims.Ply != 23 {
		t.Fatalf("ply = %d", claims.Ply)
	}
	if string(claims.Payload) != string(payload) {
		t.Fatalf("payload = %s, want %s", claims.Payload, payload)
	}
	if claims.ReceivedAt != "2026-09-08T14:03:22.115Z" {
		t.Fatalf("rat = %q, want ISO-8601 UTC ms", claims.ReceivedAt)
	}
	if claims.IssuedAt == 0 || claims.IssuedAt > time.Now().Unix()+5 {
		t.Fatalf("iat = %d, want roughly now", claims.IssuedAt)
	}

	// The exact claim set: no extras beyond Appendix B.
	payloadB64 := strings.Split(token, ".")[1]
	raw, err := base64URLDecode(payloadB64)
	if err != nil {
		t.Fatal(err)
	}
	var claimSet map[string]json.RawMessage
	if err := json.Unmarshal(raw, &claimSet); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"iss", "sub", "game", "ply", "payload", "rat", "iat"}
	if len(claimSet) != len(wantKeys) {
		t.Fatalf("claim set has %d keys (%v), want exactly %v", len(claimSet), claimSet, wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := claimSet[k]; !ok {
			t.Fatalf("claim set missing %q: %v", k, claimSet)
		}
	}
}

// TestMoveTokenTamper: mutating any covered claim breaks verification.
func TestMoveTokenTamper(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	receivedAt := time.Now().UTC()
	payload := testPayload(t)

	token, err := mintMoveToken(priv, "did:web:bot.plays.bot", "did:plc:player", "at://svc/game/1", 4, payload, receivedAt)
	if err != nil {
		t.Fatal(err)
	}
	seg := strings.Split(token, ".")

	decodeSeg := func(i int, into any) {
		t.Helper()
		raw, err := base64URLDecode(seg[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatal(err)
		}
	}
	encodeSeg := func(i int, v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		seg[i] = base64URLEncode(raw)
		return strings.Join(seg, ".")
	}

	// Sanity: the untampered token verifies.
	if _, err := VerifyMoveToken(pub, strings.Join(seg, ".")); err != nil {
		t.Fatalf("baseline verify: %v", err)
	}

	tamper := func(name string, segIdx int, mutate func(m map[string]any)) {
		t.Helper()
		var m map[string]any
		decodeSeg(segIdx, &m)
		mutate(m)
		bad := encodeSeg(segIdx, m)
		if _, err := VerifyMoveToken(pub, bad); err == nil {
			t.Fatalf("%s: tampered token verified", name)
		}
	}

	tamper("mutated payload", 1, func(m map[string]any) { m["payload"].(map[string]any)["to"] = "e5" })
	tamper("mutated ply", 1, func(m map[string]any) { m["ply"] = float64(5) })
	tamper("mutated sub", 1, func(m map[string]any) { m["sub"] = "did:plc:someone-else" })
	tamper("mutated rat", 1, func(m map[string]any) { m["rat"] = "2020-01-01T00:00:00.000Z" })

	// Wrong key (different service) fails.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMoveToken(otherPub, token); err == nil {
		t.Fatal("token verified under the wrong service key")
	}
}

// TestMoveTokenWithServiceKey: the token verifies against a key loaded via
// internal/keys, the same path /.well-known/plays-bot/service.json serves.
func TestMoveTokenWithServiceKey(t *testing.T) {
	path := t.TempDir() + "/signing.key"
	priv, err := keys.LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}

	token, err := mintMoveToken(priv, "did:web:bot.plays.bot", "did:plc:p", "at://g/1", 1, testPayload(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("not an ed25519 public key")
	}
	if _, err := VerifyMoveToken(pub, token); err != nil {
		t.Fatalf("verify with service key: %v", err)
	}

	// Re-loading the key file yields the same identity.
	priv2, err := keys.LoadOrCreateSigningKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !priv2.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("key file reload produced a different key")
	}
}
