// Package escrow implements the commentary content-key cryptography
// (spec §8.1):
//
//   - Content encryption: XChaCha20-Poly1305 over the note text with a
//     per-note agent-generated nonce and AAD = "${gameUri}|${ply}|${playerDid}"
//     (canonical context binding, spec §8.1 "AAD" rule).
//
//   - Escrow wrap: ECIES-style. The agent generates a fresh ephemeral
//     X25519 keypair per wrapped key, computes the shared secret against
//     the AppView escrow public key for rotationId, derives the wrap key
//     with HKDF-SHA256 (salt = ephemeralPub || escrowPub, info =
//     "plays.bot/escrow/v1"), and XChaCha20-Poly1305-seals the 32-byte
//     content key.
//
// Everything here is pure and deterministic given explicit key material:
// the golden test vectors in escrow_test.go pin exact outputs so library
// drift is caught at test time.
package escrow

import (
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/haileyok/botplaysbot/internal/keys"
)

// HKDFInfo is the HKDF info string for the escrow wrap key derivation
// (spec §8.1). Versioned so a future scheme change can't be cross-derived.
const HKDFInfo = "plays.bot/escrow/v1"

// KeySize is the content key size: an XChaCha20-Poly1305 key.
const KeySize = 32

// NonceSize is the XChaCha20-Poly1305 nonce size.
const NonceSize = 24

// PublicKeySize is the X25519 public key size.
const PublicKeySize = 32

// ContentKey is the per-game symmetric key the agent generates (spec §8.1).
type ContentKey [KeySize]byte

// ErrDecrypt is the constant-time error for any AEAD open failure (content
// or wrap). It deliberately carries no detail: callers cannot distinguish
// wrong key from tampered ciphertext.
var ErrDecrypt = errors.New("escrow: decryption failed")

// ErrBadInput rejects key material of the wrong length or unusable X25519
// input (low-order peer key, malformed private key).
type ErrBadInput struct{ Msg string }

func (e *ErrBadInput) Error() string { return "escrow: " + e.Msg }

// GenerateContentKey returns a random 32-byte content key.
func GenerateContentKey() (ContentKey, error) {
	var k ContentKey
	if _, err := rand.Read(k[:]); err != nil {
		return ContentKey{}, fmt.Errorf("escrow: generate content key: %w", err)
	}
	return k, nil
}

// AAD builds the canonical additional authenticated data binding a
// ciphertext to its game context: "${gameUri}|${ply}|${playerDid}" (spec
// §8.1). ply follows the record convention: 0 when the record omits ply.
func AAD(gameURI string, ply int64, playerDID string) []byte {
	return []byte(gameURI + "|" + strconv.FormatInt(ply, 10) + "|" + playerDID)
}

// ---------------------------------------------------------------------------
// Content encryption

// Encrypt seals plaintext under key with the explicit 24-byte nonce and
// aad. Exactly the XChaCha20-Poly1305 construction; the nonce is the
// caller's responsibility (agents generate one per note; the indexer and
// reveal scheduler read it back from the record).
func Encrypt(key ContentKey, nonce [NonceSize]byte, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("escrow: init XChaCha20-Poly1305: %w", err)
	}
	return aead.Seal(nil, nonce[:], plaintext, aad), nil
}

// Decrypt opens a ciphertext produced by Encrypt. Any wrong key, wrong
// nonce, tampered bytes, or wrong aad yields ErrDecrypt (the poly1305 tag
// check is constant-time).
func Decrypt(key ContentKey, nonce [NonceSize]byte, ciphertext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("escrow: init XChaCha20-Poly1305: %w", err)
	}
	plaintext, err := aead.Open(nil, nonce[:], ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// ---------------------------------------------------------------------------
// Escrow wrap

// WrappedKey is the escrowKey object stored on delayed/sealed commentary
// records (spec §4.6): which escrow rotation was targeted, the agent's
// ephemeral public key, and the wrapped content key.
type WrappedKey struct {
	RotationID         string
	EphemeralPublicKey [PublicKeySize]byte
	// WrappedKey is the XChaCha20-Poly1305 seal of the 32-byte content
	// key: 48 bytes (32 key + 16 tag).
	WrappedKey []byte
}

// wrapNonce is the fixed all-zero nonce used for the escrow wrap step.
//
// A fixed nonce is normally catastrophic for an AEAD — but only when a key
// is reused across messages. Here the wrap key is HKDF output over the
// ECDH shared secret of a *fresh ephemeral keypair per wrap*, so every
// sealed wrap is encrypted under a key that has never been used before and
// will never be used again (identical inputs — same ephemeral key replayed
// — produce the identical wrap, leaking only equality, which the repo
// already shows). The (key, nonce) pair is therefore globally unique with
// overwhelming probability, which is precisely the security argument AEADs
// make for random nonces; fixing the nonce removes the agent-side failure
// mode of reusing a random nonce within one ECDH derivation.
var wrapNonce [NonceSize]byte // all zeros

// deriveWrapKey computes the escrow wrap key: HKDF-SHA256 with
// ikm = X25519(ephemeralPriv, escrowPub), salt = ephemeralPub || escrowPub
// (both raw 32-byte public keys), info = "plays.bot/escrow/v1". Salt
// context-separates the derivation across agent/AppView keypairs so
// unrelated wraps cannot collide even under shared-secret reuse.
func deriveWrapKey(ephemeralPub, escrowPub []byte, shared []byte) ([KeySize]byte, error) {
	var out [KeySize]byte
	salt := make([]byte, 0, len(ephemeralPub)+len(escrowPub))
	salt = append(salt, ephemeralPub...)
	salt = append(salt, escrowPub...)
	key, err := hkdfKey(shared, salt, []byte(HKDFInfo))
	if err != nil {
		return out, fmt.Errorf("escrow: hkdf: %w", err)
	}
	copy(out[:], key)
	return out, nil
}

// WrapWithKey seals contentKey under an explicit wrap key and nonce
// (low-level; exported so golden vectors can pin the raw AEAD layer).
func WrapWithKey(wrapKey [KeySize]byte, nonce [NonceSize]byte, contentKey ContentKey) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(wrapKey[:])
	if err != nil {
		return nil, fmt.Errorf("escrow: init XChaCha20-Poly1305: %w", err)
	}
	return aead.Seal(nil, nonce[:], contentKey[:], nil), nil
}

// OpenWrap opens a wrap produced by WrapWithKey (low-level; exported for
// golden vectors and the constant-time test of tag checking).
func OpenWrap(wrapKey [KeySize]byte, nonce [NonceSize]byte, wrapped []byte) (ContentKey, error) {
	aead, err := chacha20poly1305.NewX(wrapKey[:])
	if err != nil {
		return ContentKey{}, fmt.Errorf("escrow: init XChaCha20-Poly1305: %w", err)
	}
	var key ContentKey
	plaintext, err := aead.Open(nil, nonce[:], wrapped, nil)
	if err != nil || len(plaintext) != KeySize {
		return ContentKey{}, ErrDecrypt
	}
	copy(key[:], plaintext)
	return key, nil
}

// WrapContentKey escrow-wraps contentKey to escrowPub using the provided
// ephemeral private key (fresh per wrap; tests pass a fixed one for
// deterministic vectors). Returns the record-ready escrowKey object.
func WrapContentKey(escrowPub [PublicKeySize]byte, ephemeral *ecdh.PrivateKey, contentKey ContentKey) (*WrappedKey, error) {
	if ephemeral == nil {
		return nil, &ErrBadInput{Msg: "nil ephemeral key"}
	}
	escrowKey, err := ecdh.X25519().NewPublicKey(escrowPub[:])
	if err != nil {
		return nil, &ErrBadInput{Msg: "bad escrow public key: " + err.Error()}
	}
	shared, err := ephemeral.ECDH(escrowKey)
	if err != nil {
		return nil, &ErrBadInput{Msg: "ecdh failed: " + err.Error()}
	}
	ephPub := ephemeral.PublicKey().Bytes()
	wrapKey, err := deriveWrapKey(ephPub, escrowPub[:], shared)
	if err != nil {
		return nil, err
	}
	sealed, err := WrapWithKey(wrapKey, wrapNonce, contentKey)
	if err != nil {
		return nil, err
	}
	wk := &WrappedKey{WrappedKey: sealed}
	copy(wk.EphemeralPublicKey[:], ephPub)
	return wk, nil
}

// Unwrap inverts WrapContentKey: the AppView's escrow private key for the
// rotation recovers the content key. Failures are ErrDecrypt (wrong key or
// tampered wrap) or *ErrBadInput (malformed inputs); tag verification is
// constant-time.
func Unwrap(escrowPriv [PublicKeySize]byte, ephemeralPub [PublicKeySize]byte, wrapped []byte) (ContentKey, error) {
	priv, err := ecdh.X25519().NewPrivateKey(escrowPriv[:])
	if err != nil {
		return ContentKey{}, &ErrBadInput{Msg: "bad escrow private key: " + err.Error()}
	}
	peer, err := ecdh.X25519().NewPublicKey(ephemeralPub[:])
	if err != nil {
		return ContentKey{}, &ErrBadInput{Msg: "bad ephemeral public key: " + err.Error()}
	}
	shared, err := priv.ECDH(peer)
	if err != nil {
		// Low-order peer point: the shared secret is unusable. Treat exactly
		// like a failed open (garbage ephemeral = garbage wrap).
		return ContentKey{}, ErrDecrypt
	}
	wrapKey, err := deriveWrapKey(ephemeralPub[:], priv.PublicKey().Bytes(), shared)
	if err != nil {
		return ContentKey{}, err
	}
	return OpenWrap(wrapKey, wrapNonce, wrapped)
}

// UnwrapVia resolves rotationID through the escrow key directory and
// unwraps. Shared by the postCommentary validator and the indexer ingest
// path so both attempt exactly the same operation (spec §5.7/§8.4).
func UnwrapVia(ctx context.Context, dir keys.EscrowKeyDirectory, rotationID string, ephemeralPub, wrappedKey []byte) (ContentKey, error) {
	if dir == nil {
		return ContentKey{}, errors.New("escrow: no escrow key directory")
	}
	if len(ephemeralPub) != PublicKeySize {
		return ContentKey{}, &ErrBadInput{Msg: fmt.Sprintf("ephemeral public key is %d bytes, want 32", len(ephemeralPub))}
	}
	if len(wrappedKey) != KeySize+16 {
		return ContentKey{}, &ErrBadInput{Msg: fmt.Sprintf("wrappedKey is %d bytes, want 48", len(wrappedKey))}
	}
	k, err := dir.Get(ctx, rotationID)
	if err != nil {
		return ContentKey{}, fmt.Errorf("escrow: rotation %s: %w", rotationID, err)
	}
	if k.PrivateKey == nil {
		return ContentKey{}, errors.New("escrow: rotation " + rotationID + " holds no private key")
	}
	var eph [PublicKeySize]byte
	copy(eph[:], ephemeralPub)
	return Unwrap(*k.PrivateKey, eph, wrappedKey)
}

// EqualKeys compares two content keys in constant time (key publication
// detection, spec §4.7 note / §10 keyMismatch).
func EqualKeys(a, b ContentKey) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// EqualBytes compares two byte slices in constant time, false when either
// is empty or the lengths differ.
func EqualBytes(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// hkdfKey runs the HKDF-SHA256 extractor/expander (standard library
// crypto/hkdf).
func hkdfKey(secret, salt, info []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, salt, string(info), KeySize)
}
