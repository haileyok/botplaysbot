/**
 * Commentary content-key cryptography for plays.bot (spec §8.1).
 *
 * This module MUST byte-match the Go reference implementation in
 * `internal/escrow/escrow.go`:
 *
 *   - Content encryption: XChaCha20-Poly1305 over the note text with a
 *     per-note 24-byte nonce and AAD = "${gameUri}|${ply}|${playerDid}".
 *
 *   - Escrow wrap (ECIES-style): a fresh ephemeral X25519 keypair per wrap;
 *     shared = X25519(ephemeralPriv, escrowPub); wrap key = HKDF-SHA256
 *     (ikm = shared, salt = ephPub(32B) || escrowPub(32B), info =
 *     "plays.bot/escrow/v1", 32 output bytes); the 32-byte content key is
 *     XChaCha20-Poly1305-sealed with an all-zero 24-byte nonce.
 *     wrappedKey = ciphertext(32B) || tag(16B) = 48 bytes.
 *
 * Golden vectors in `escrow.test.ts` and `internal/escrow/escrow_test.go`
 * pin identical outputs on both sides — the hard TS/Go interop guarantee.
 */
import { xchacha20poly1305 } from '@noble/ciphers/chacha.js'
import { x25519 } from '@noble/curves/ed25519.js'
import { hkdf } from '@noble/hashes/hkdf.js'
import { sha256 } from '@noble/hashes/sha2.js'

export const KEY_SIZE = 32
export const NONCE_SIZE = 24
export const PUBLIC_KEY_SIZE = 32
export const WRAPPED_KEY_SIZE = KEY_SIZE + 16 // 48

/** HKDF info string for the escrow wrap key derivation (spec §8.1). */
export const HKDF_INFO = 'plays.bot/escrow/v1'

/** The fixed all-zero nonce for the escrow wrap AEAD (see Go WrapContentKey). */
const WRAP_NONCE = new Uint8Array(NONCE_SIZE).fill(0)

/** The record-ready escrowKey object (spec §4.6 escrowKey ref). */
export interface EscrowKey {
  rotationId: string
  /** base64 (lexicon bytes in JSON records are base64). */
  ephemeralPublicKey: string
  /** base64, 48 bytes. */
  wrappedKey: string
}

/** Decoded escrowKey bytes, as embedded in a lexicon record. */
export interface EscrowKeyBytes {
  rotationId: string
  /** raw 32 bytes. */
  ephemeralPublicKeyBytes: Uint8Array
  /** raw 48 bytes. */
  wrappedKeyBytes: Uint8Array
}

export function b64Encode(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64')
}

export function b64Decode(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, 'base64'))
}

export function b64urlEncode(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64url')
}

export function b64urlDecode(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, 'base64url'))
}

function assertLen(name: string, got: number, want: number): void {
  if (got !== want) {
    throw new Error(`crypto: ${name} is ${got} bytes, want ${want}`)
  }
}

/** Canonical additional authenticated data (spec §8.1). ply 0 when the record omits ply. */
export function aad(gameUri: string, ply: number, playerDid: string): Uint8Array {
  return new TextEncoder().encode(`${gameUri}|${ply}|${playerDid}`)
}

/** Generate a random 32-byte content key. */
export function generateContentKey(): Uint8Array {
  const k = new Uint8Array(KEY_SIZE)
  crypto.getRandomValues(k)
  return k
}

/** Generate a random 24-byte AEAD nonce. */
export function generateNonce(): Uint8Array {
  const n = new Uint8Array(NONCE_SIZE)
  crypto.getRandomValues(n)
  return n
}

function xchacha(
  key: Uint8Array,
  nonce: Uint8Array,
  aadBytes: Uint8Array,
): ReturnType<typeof xchacha20poly1305> {
  assertLen('content key', key.length, KEY_SIZE)
  return xchacha20poly1305(key, nonce, aadBytes)
}

/**
 * Encrypt (seal) plaintext under contentKey with the explicit nonce and aad.
 * Returns the AEAD output (ciphertext || tag).
 */
export function encrypt(
  contentKey: Uint8Array,
  nonce: Uint8Array,
  plaintext: Uint8Array,
  aadBytes: Uint8Array,
): Uint8Array {
  assertLen('nonce', nonce.length, NONCE_SIZE)
  return xchacha(contentKey, nonce, aadBytes).encrypt(plaintext)
}

/** Decrypt a ciphertext produced by {@link encrypt}. Throws on any AEAD open failure. */
export function decrypt(
  contentKey: Uint8Array,
  nonce: Uint8Array,
  ciphertext: Uint8Array,
  aadBytes: Uint8Array,
): Uint8Array {
  assertLen('nonce', nonce.length, NONCE_SIZE)
  return xchacha(contentKey, nonce, aadBytes).decrypt(ciphertext)
}

/**
 * Derive the escrow wrap key: HKDF-SHA256(ikm = shared, salt = ephPub || escrowPub,
 * info = HKDF_INFO, 32 bytes). Exported for the golden-vector tests.
 */
export function deriveWrapKey(ephemeralPub: Uint8Array, escrowPub: Uint8Array, shared: Uint8Array): Uint8Array {
  assertLen('ephemeral public key', ephemeralPub.length, PUBLIC_KEY_SIZE)
  assertLen('escrow public key', escrowPub.length, PUBLIC_KEY_SIZE)
  const salt = new Uint8Array(PUBLIC_KEY_SIZE * 2)
  salt.set(ephemeralPub, 0)
  salt.set(escrowPub, PUBLIC_KEY_SIZE)
  return hkdf(sha256, shared, salt, new TextEncoder().encode(HKDF_INFO), KEY_SIZE)
}

/** Low-level wrap with an explicit ephemeral secret key (golden vectors use a fixed one). */
export function wrapWithKey(
  escrowPub: Uint8Array,
  ephemeralSecretKey: Uint8Array,
  contentKey: Uint8Array,
): { ephemeralPublicKey: Uint8Array; wrappedKey: Uint8Array } {
  assertLen('escrow public key', escrowPub.length, PUBLIC_KEY_SIZE)
  assertLen('ephemeral secret key', ephemeralSecretKey.length, PUBLIC_KEY_SIZE)
  assertLen('content key', contentKey.length, KEY_SIZE)

  const ephPub = x25519.getPublicKey(ephemeralSecretKey)
  const shared = x25519.getSharedSecret(ephemeralSecretKey, escrowPub)
  const wrapKey = deriveWrapKey(ephPub, escrowPub, shared)
  // No AAD on the wrap itself (matches Go WrapWithKey: aead.Open(nil, nonce, wrapped, nil)).
  const wrapped = xchacha20poly1305(wrapKey, WRAP_NONCE).encrypt(contentKey)
  assertLen('wrappedKey', wrapped.length, WRAPPED_KEY_SIZE)
  return { ephemeralPublicKey: ephPub, wrappedKey: wrapped }
}

/** Low-level unwrap with an explicit escrow secret key (golden vectors). */
export function unwrapWithKey(
  escrowSecretKey: Uint8Array,
  ephemeralPub: Uint8Array,
  wrappedKey: Uint8Array,
): Uint8Array {
  assertLen('escrow secret key', escrowSecretKey.length, PUBLIC_KEY_SIZE)
  assertLen('ephemeral public key', ephemeralPub.length, PUBLIC_KEY_SIZE)
  assertLen('wrappedKey', wrappedKey.length, WRAPPED_KEY_SIZE)

  const shared = x25519.getSharedSecret(escrowSecretKey, ephemeralPub)
  const wrapKey = deriveWrapKey(ephemeralPub, x25519.getPublicKey(escrowSecretKey), shared)
  const key = xchacha20poly1305(wrapKey, WRAP_NONCE).decrypt(wrappedKey)
  assertLen('unwrapped key', key.length, KEY_SIZE)
  return key
}

/**
 * Escrow-wrap a content key to the AppView's current escrow public key,
 * generating a fresh ephemeral keypair (production shape). Returns both the
 * base64 record shape and decoded bytes for direct record embedding.
 */
export function wrapForEscrow(
  contentKey: Uint8Array,
  escrowPublicKeyB64url: string,
  rotationId: string,
): EscrowKey & EscrowKeyBytes {
  const escrowPub = b64urlDecode(escrowPublicKeyB64url)
  const ephSecret = x25519.utils.randomSecretKey()
  const { ephemeralPublicKey, wrappedKey } = wrapWithKey(escrowPub, ephSecret, contentKey)
  return {
    rotationId,
    ephemeralPublicKey: b64Encode(ephemeralPublicKey),
    wrappedKey: b64Encode(wrappedKey),
    ephemeralPublicKeyBytes: ephemeralPublicKey,
    wrappedKeyBytes: wrappedKey,
  }
}
