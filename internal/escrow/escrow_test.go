package escrow

import (
	"context"
	"crypto/ecdh"
	cryptorand "crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/haileyok/botplaysbot/internal/keys"
)

// seqBytes returns n bytes starting at off (deterministic test material).
func seqBytes(n int, off byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) + off
	}
	return b
}

// Golden vectors: generated once with this exact implementation
// (XChaCha20-Poly1305 via golang.org/x/crypto, X25519 via crypto/ecdh,
// HKDF-SHA256 via crypto/hkdf) and pinned forever. If these change without
// a deliberate scheme-version bump, a library update has drifted.
//
//	contentKey    = bytes 0..31
//	nonce         = bytes 100..123
//	plaintext     = "plays.bot golden vector"
//	aad           = "at://did:plc:svc/bot.plays.bot.game/abc123|3|did:plc:alpha"
//	escrowPriv    = bytes 128..159 (escrowPub derived below)
//	ephemeralPriv = bytes 64..95   (ephPub derived below)
const (
	goldenAAD        = "at://did:plc:svc/bot.plays.bot.game/abc123|3|did:plc:alpha"
	goldenPlaintext  = "plays.bot golden vector"
	goldenCiphertext = "0c1f98c98c00ebedf310fa50ca0bd8a77bcd37129043b8d636c81555201562e30ca6a62e015dc8"
	goldenEscrowPub  = "493e82fc74464a59268817623d2053c5eb8e2cc4a988b4fee179ec6b010d531d"
	goldenEphPub     = "79a631eede1bf9c98f12032cdeadd0e7a079398fc786b88cc846ec89af85a51a"
	goldenWrapped    = "758026bec7d85842b39c54acd54b2357e8f21b6d6652edf86fe5c272301439bd18508090b2a3dcb8b1b123e49424199a"
)

func goldenContentKey() ContentKey {
	var k ContentKey
	copy(k[:], seqBytes(32, 0))
	return k
}

func goldenNonce() [NonceSize]byte {
	var n [NonceSize]byte
	copy(n[:], seqBytes(24, 100))
	return n
}

// goldenEscrowKeys derives the pinned escrow keypair and ephemeral key.
func goldenEscrowKeys(t *testing.T) (priv, pub [32]byte, eph *ecdh.PrivateKey) {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(seqBytes(32, 128))
	if err != nil {
		t.Fatalf("escrow priv: %v", err)
	}
	copy(priv[:], k.Bytes())
	copy(pub[:], k.PublicKey().Bytes())
	e, err := ecdh.X25519().NewPrivateKey(seqBytes(32, 64))
	if err != nil {
		t.Fatalf("eph priv: %v", err)
	}
	return priv, pub, e
}

func mustEphemeral(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatalf("ephemeral key: %v", err)
	}
	return k
}

func TestEncryptGoldenVector(t *testing.T) {
	ct, err := Encrypt(goldenContentKey(), goldenNonce(), []byte(goldenPlaintext), []byte(goldenAAD))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := hex.EncodeToString(ct); got != goldenCiphertext {
		t.Fatalf("ciphertext drifted\ngot:  %s\nwant: %s", got, goldenCiphertext)
	}
}

func TestDecryptGoldenVector(t *testing.T) {
	raw, err := hex.DecodeString(goldenCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Decrypt(goldenContentKey(), goldenNonce(), raw, []byte(goldenAAD))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(pt) != goldenPlaintext {
		t.Fatalf("plaintext = %q, want %q", pt, goldenPlaintext)
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key, err := GenerateContentKey()
	if err != nil {
		t.Fatalf("GenerateContentKey: %v", err)
	}
	var nonce [NonceSize]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	aad := AAD("at://did:example/bot.plays.bot.game/x", 7, "did:plc:p")
	ct, err := Encrypt(key, nonce, []byte("hello hello"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := Decrypt(key, nonce, ct, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(pt) != "hello hello" {
		t.Fatalf("roundtrip = %q", pt)
	}

	// Wrong AAD → fail (context binding is enforced by the tag).
	if _, err := Decrypt(key, nonce, ct, AAD("at://did:example/bot.plays.bot.game/x", 8, "did:plc:p")); err == nil {
		t.Fatal("decrypt with wrong AAD succeeded")
	}
	// Wrong key → fail.
	other, _ := GenerateContentKey()
	if _, err := Decrypt(other, nonce, ct, aad); err == nil {
		t.Fatal("decrypt with wrong key succeeded")
	}
	// Wrong nonce → fail.
	nonce[0] ^= 1
	if _, err := Decrypt(key, nonce, ct, aad); err == nil {
		t.Fatal("decrypt with wrong nonce succeeded")
	}
}

func TestTamperCiphertext(t *testing.T) {
	key, _ := GenerateContentKey()
	var nonce [NonceSize]byte
	ct, err := Encrypt(key, nonce, []byte("tamper me"), AAD("g", 1, "p"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for _, i := range []int{0, len(ct) / 2, len(ct) - 1} {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x01
		if _, err := Decrypt(key, nonce, bad, AAD("g", 1, "p")); err == nil {
			t.Fatalf("flipping bit %d in ciphertext did not fail decryption", i)
		}
	}
}

func TestWrapGoldenVector(t *testing.T) {
	_, escrowPub, eph := goldenEscrowKeys(t)
	if hex.EncodeToString(escrowPub[:]) != goldenEscrowPub {
		t.Fatalf("escrow pub derivation drifted: %s", hex.EncodeToString(escrowPub[:]))
	}
	wk, err := WrapContentKey(escrowPub, eph, goldenContentKey())
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	if got := hex.EncodeToString(wk.EphemeralPublicKey[:]); got != goldenEphPub {
		t.Fatalf("ephemeral pub drifted\ngot:  %s\nwant: %s", got, goldenEphPub)
	}
	if got := hex.EncodeToString(wk.WrappedKey); got != goldenWrapped {
		t.Fatalf("wrappedKey drifted\ngot:  %s\nwant: %s", got, goldenWrapped)
	}
	if len(wk.WrappedKey) != 48 {
		t.Fatalf("wrappedKey is %d bytes, want 48 (32 key + 16 tag)", len(wk.WrappedKey))
	}
}

func TestUnwrapGoldenVector(t *testing.T) {
	priv, _, _ := goldenEscrowKeys(t)
	raw, err := hex.DecodeString(goldenWrapped)
	if err != nil {
		t.Fatal(err)
	}
	var ephPub [32]byte
	rawEph, err := hex.DecodeString(goldenEphPub)
	if err != nil {
		t.Fatal(err)
	}
	copy(ephPub[:], rawEph)

	key, err := Unwrap(priv, ephPub, raw)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if key != goldenContentKey() {
		t.Fatal("unwrapped key != content key")
	}
}

func TestWrapUnwrapRoundtrip(t *testing.T) {
	priv, pub, _ := goldenEscrowKeys(t)
	key, _ := GenerateContentKey()

	// A fresh ephemeral key per wrap (production shape).
	wk, err := WrapContentKey(pub, mustEphemeral(t), key)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	got, err := Unwrap(priv, wk.EphemeralPublicKey, wk.WrappedKey)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if got != key {
		t.Fatal("roundtrip mismatch")
	}

	// Unwrap under a different escrow key fails (wrong rotation).
	other, err := ecdh.X25519().GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatalf("escrow: %v", err)
	}
	var otherArr [32]byte
	copy(otherArr[:], other.Bytes())
	if _, err := Unwrap(otherArr, wk.EphemeralPublicKey, wk.WrappedKey); err == nil {
		t.Fatal("unwrap with wrong escrow key succeeded")
	}
}

func TestTamperWrappedKey(t *testing.T) {
	priv, pub, _ := goldenEscrowKeys(t)
	key, _ := GenerateContentKey()
	wk, err := WrapContentKey(pub, mustEphemeral(t), key)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}
	for _, i := range []int{0, len(wk.WrappedKey) / 2, len(wk.WrappedKey) - 1} {
		bad := append([]byte(nil), wk.WrappedKey...)
		bad[i] ^= 0x80
		if _, err := Unwrap(priv, wk.EphemeralPublicKey, bad); err == nil {
			t.Fatalf("flipping bit %d in wrappedKey did not fail unwrap", i)
		}
	}
	// Tampered ephemeral public key → different ECDH → open failure.
	badEph := wk.EphemeralPublicKey
	badEph[0] ^= 0x01
	if _, err := Unwrap(priv, badEph, wk.WrappedKey); err == nil {
		t.Fatal("unwrap with tampered ephemeral pub succeeded")
	}
}

func TestUnwrapGarbageInputs(t *testing.T) {
	priv, _, _ := goldenEscrowKeys(t)
	var eph [32]byte // all zeros: low-order point
	garbage := seqBytes(48, 200)
	if _, err := Unwrap(priv, eph, garbage); err == nil {
		t.Fatal("unwrap with low-order ephemeral key succeeded")
	}
	var wrongLen [32]byte
	copy(wrongLen[:], seqBytes(32, 5))
	if _, err := Unwrap(priv, wrongLen, seqBytes(47, 0)); err == nil {
		t.Fatal("unwrap with short wrappedKey succeeded")
	}
}

// fakeDirectory is a one-rotation EscrowKeyDirectory.
type fakeDirectory struct {
	rotationID string
	priv       *[32]byte
}

func (d *fakeDirectory) Current(_ context.Context) (*keys.EscrowKey, error) {
	if d.priv == nil {
		return nil, keys.ErrNotFound
	}
	return d.rotation(), nil
}

func (d *fakeDirectory) Get(_ context.Context, rotationID string) (*keys.EscrowKey, error) {
	if d.priv == nil || rotationID != d.rotationID {
		return nil, keys.ErrNotFound
	}
	return d.rotation(), nil
}

func (d *fakeDirectory) List(_ context.Context) ([]keys.EscrowKey, error) { return nil, nil }

func (d *fakeDirectory) rotation() *keys.EscrowKey {
	priv := *d.priv
	return &keys.EscrowKey{RotationID: d.rotationID, PrivateKey: &priv}
}

func TestUnwrapVia(t *testing.T) {
	_, pub, _ := goldenEscrowKeys(t)
	key, _ := GenerateContentKey()
	wk, err := WrapContentKey(pub, mustEphemeral(t), key)
	if err != nil {
		t.Fatalf("WrapContentKey: %v", err)
	}

	priv, _, _ := goldenEscrowKeys(t)
	dir := &fakeDirectory{rotationID: "rot1", priv: &priv}
	got, err := UnwrapVia(t.Context(), dir, "rot1", wk.EphemeralPublicKey[:], wk.WrappedKey)
	if err != nil {
		t.Fatalf("UnwrapVia: %v", err)
	}
	if got != key {
		t.Fatal("UnwrapVia roundtrip mismatch")
	}

	// Unknown rotation → error (the escrowFailed ingest path).
	if _, err := UnwrapVia(t.Context(), dir, "rot-missing", wk.EphemeralPublicKey[:], wk.WrappedKey); err == nil {
		t.Fatal("UnwrapVia with unknown rotation succeeded")
	}
	// Wrong-length wrappedKey → rejected before any crypto.
	if _, err := UnwrapVia(t.Context(), dir, "rot1", wk.EphemeralPublicKey[:], wk.WrappedKey[:40]); err == nil {
		t.Fatal("UnwrapVia with short wrappedKey succeeded")
	}
	// Garbage ephemeral → failure.
	if _, err := UnwrapVia(t.Context(), dir, "rot1", seqBytes(32, 9), wk.WrappedKey); err == nil {
		t.Fatal("UnwrapVia with garbage ephemeral key succeeded")
	}
	// Nil directory → error, not panic.
	if _, err := UnwrapVia(t.Context(), nil, "rot1", wk.EphemeralPublicKey[:], wk.WrappedKey); err == nil {
		t.Fatal("UnwrapVia with nil directory succeeded")
	}
}

func TestEqualKeysConstantTimeHelpers(t *testing.T) {
	a := goldenContentKey()
	b := goldenContentKey()
	if !EqualKeys(a, b) {
		t.Fatal("EqualKeys(a, a) = false")
	}
	b[31] ^= 1
	if EqualKeys(a, b) {
		t.Fatal("EqualKeys detected no difference")
	}
	if !EqualBytes([]byte{1, 2, 3}, []byte{1, 2, 3}) {
		t.Fatal("EqualBytes(a, a) = false")
	}
	if EqualBytes([]byte{1, 2, 3}, []byte{1, 2, 4}) {
		t.Fatal("EqualBytes(a, b) = true for differing inputs")
	}
	if EqualBytes(nil, nil) || EqualBytes([]byte{1}, nil) {
		t.Fatal("EqualBytes must reject empty/length-mismatched inputs")
	}
}

func TestAADShape(t *testing.T) {
	got := string(AAD("at://x/y/z", 12, "did:plc:q"))
	want := "at://x/y/z|12|did:plc:q"
	if got != want {
		t.Fatalf("AAD = %q, want %q", got, want)
	}
}
