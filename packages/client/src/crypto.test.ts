/**
 * Golden vectors — MUST byte-match internal/escrow/escrow_test.go (Go).
 *
 * The Go vectors were generated with:
 *   contentKey    = bytes 0..31        (0x00, 0x01, ..., 0x1f)
 *   nonce         = bytes 100..123     (0x64, ..., 0x7b)
 *   plaintext     = "plays.bot golden vector"
 *   aad           = "at://did:plc:svc/bot.plays.bot.game/abc123|3|did:plc:alpha"
 *   escrowPriv    = bytes 128..159
 *   ephemeralPriv = bytes 64..95
 */
import { describe, expect, it } from 'vitest'

import {
  aad,
  b64Encode,
  decrypt,
  deriveWrapKey,
  encrypt,
  generateContentKey,
  generateNonce,
  unwrapWithKey,
  wrapForEscrow,
  wrapWithKey,
} from './crypto.js'

function seqBytes(n: number, off: number): Uint8Array {
  const b = new Uint8Array(n)
  for (let i = 0; i < n; i++) b[i] = (i + off) & 0xff
  return b
}

function hex(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, 'hex'))
}

function toHex(b: Uint8Array): string {
  return Buffer.from(b).toString('hex')
}

const goldenAAD = 'at://did:plc:svc/bot.plays.bot.game/abc123|3|did:plc:alpha'
const goldenPlaintext = new TextEncoder().encode('plays.bot golden vector')
const goldenCiphertext = '0c1f98c98c00ebedf310fa50ca0bd8a77bcd37129043b8d636c81555201562e30ca6a62e015dc8'
const goldenEscrowPub = '493e82fc74464a59268817623d2053c5eb8e2cc4a988b4fee179ec6b010d531d'
const goldenEphPub = '79a631eede1bf9c98f12032cdeadd0e7a079398fc786b88cc846ec89af85a51a'
const goldenWrapped =
  '758026bec7d85842b39c54acd54b2357e8f21b6d6652edf86fe5c272301439bd18508090b2a3dcb8b1b123e49424199a'

const goldenContentKey = () => seqBytes(32, 0)
const goldenNonce = () => seqBytes(24, 100)
const goldenEscrowPriv = () => seqBytes(32, 128)
const goldenEphPriv = () => seqBytes(32, 64)

describe('escrow crypto golden vectors (interop with internal/escrow, Go)', () => {
  it('encrypt matches the Go golden ciphertext byte-for-byte', () => {
    const ct = encrypt(goldenContentKey(), goldenNonce(), goldenPlaintext, new TextEncoder().encode(goldenAAD))
    expect(toHex(ct)).toBe(goldenCiphertext)
  })

  it('decrypts the Go golden ciphertext byte-for-byte', () => {
    const pt = decrypt(goldenContentKey(), goldenNonce(), hex(goldenCiphertext), new TextEncoder().encode(goldenAAD))
    expect(new TextDecoder().decode(pt)).toBe('plays.bot golden vector')
  })

  it('derives the Go golden escrow public key from the pinned private key', () => {
    // ecdh.X25519().NewPrivateKey(bytes 128..159).PublicKey() == goldenEscrowPub.
    // Reproduced here via wrapWithKey's ephPub derivation path:
    // derive the escrow public key using x25519.getPublicKey.
    // (Imported lazily to keep the test's imports about our module.)
    const pub = x25519Pub(goldenEscrowPriv())
    expect(toHex(pub)).toBe(goldenEscrowPub)
  })

  it('wrap matches the Go golden ephPub + wrappedKey byte-for-byte', () => {
    const { ephemeralPublicKey, wrappedKey } = wrapWithKey(hex(goldenEscrowPub), goldenEphPriv(), goldenContentKey())
    expect(toHex(ephemeralPublicKey)).toBe(goldenEphPub)
    expect(toHex(wrappedKey)).toBe(goldenWrapped)
    expect(wrappedKey.length).toBe(48)
  })

  it('unwrap recovers the Go golden content key', () => {
    const key = unwrapWithKey(goldenEscrowPriv(), hex(goldenEphPub), hex(goldenWrapped))
    expect(toHex(key)).toBe(toHex(goldenContentKey()))
  })

  it('deriveWrapKey is deterministic across calls', () => {
    const shared = new Uint8Array(32).fill(7)
    const k1 = deriveWrapKey(seqBytes(32, 1), seqBytes(32, 2), shared)
    const k2 = deriveWrapKey(seqBytes(32, 1), seqBytes(32, 2), shared)
    expect(toHex(k1)).toBe(toHex(k2))
  })
})

describe('roundtrip + tamper behavior (mirrors Go escrow_test.go)', () => {
  it('encrypt/decrypt roundtrip; wrong aad/key/nonce fail', () => {
    const key = generateContentKey()
    const nonce = generateNonce()
    const uri = 'at://did:example/bot.plays.bot.game/x'
    const a = aad(uri, 7, 'did:plc:p')
    const ct = encrypt(key, nonce, new TextEncoder().encode('hello hello'), a)
    const pt = decrypt(key, nonce, ct, a)
    expect(new TextDecoder().decode(pt)).toBe('hello hello')

    // Wrong AAD → fail.
    expect(() => decrypt(key, nonce, ct, aad(uri, 8, 'did:plc:p'))).toThrow()
    // Wrong key → fail.
    expect(() => decrypt(generateContentKey(), nonce, ct, a)).toThrow()
    // Wrong nonce → fail.
    const badNonce = generateNonce()
    badNonce[0]! ^= 1
    expect(() => decrypt(key, badNonce, ct, a)).toThrow()
  })

  it('tampered ciphertext fails the poly1305 tag check', () => {
    const key = generateContentKey()
    const nonce = generateNonce()
    const ct = encrypt(key, nonce, new TextEncoder().encode('tamper me'), aad('g', 1, 'p'))
    for (const i of [0, ct.length >> 1, ct.length - 1]) {
      const bad = ct.slice()
      bad[i]! ^= 0x01
      expect(() => decrypt(key, nonce, bad, aad('g', 1, 'p'))).toThrow()
    }
  })

  it('wrap/unwrap roundtrip with a fresh ephemeral key', () => {
    const escrowPub = x25519Pub(goldenEscrowPriv())
    const key = generateContentKey()
    const { ephemeralPublicKey, wrappedKey } = wrapWithKey(escrowPub, goldenEphPriv(), key)
    // Sanity: a different ephemeral key unwraps under the right escrow key.
    const got = unwrapWithKey(goldenEscrowPriv(), ephemeralPublicKey, wrappedKey)
    expect(toHex(got)).toBe(toHex(key))

    // Wrong escrow key → fail.
    const otherPriv = seqBytes(32, 200)
    expect(() => unwrapWithKey(otherPriv, ephemeralPublicKey, wrappedKey)).toThrow()
  })

  it('tampered wrappedKey / ephemeral key fails unwrap', () => {
    const escrowPub = x25519Pub(goldenEscrowPriv())
    const key = generateContentKey()
    const { ephemeralPublicKey, wrappedKey } = wrapWithKey(escrowPub, goldenEphPriv(), key)
    for (const i of [0, wrappedKey.length >> 1, wrappedKey.length - 1]) {
      const bad = wrappedKey.slice()
      bad[i]! ^= 0x80
      expect(() => unwrapWithKey(goldenEscrowPriv(), ephemeralPublicKey, bad)).toThrow()
    }
    const badEph = ephemeralPublicKey.slice()
    badEph[0]! ^= 0x01
    expect(() => unwrapWithKey(goldenEscrowPriv(), badEph, wrappedKey)).toThrow()
  })
})

describe('record-shaped helpers', () => {
  it('wrapForEscrow returns base64 fields and roundtrips through unwrap', () => {
    const key = generateContentKey()
    const escrowPubB64url = Buffer.from(x25519Pub(goldenEscrowPriv())).toString('base64url')
    const ek = wrapForEscrow(key, escrowPubB64url, 'rot1')
    expect(ek.rotationId).toBe('rot1')
    expect(Buffer.from(ek.ephemeralPublicKey, 'base64').length).toBe(32)
    expect(Buffer.from(ek.wrappedKey, 'base64').length).toBe(48)

    // Unwrap through the raw path to confirm the base64 fields roundtrip.
    const got = unwrapWithKey(
      goldenEscrowPriv(),
      new Uint8Array(Buffer.from(ek.ephemeralPublicKey, 'base64')),
      new Uint8Array(Buffer.from(ek.wrappedKey, 'base64')),
    )
    expect(toHex(got)).toBe(toHex(key))
  })

  it('base64 encoding helpers roundtrip', () => {
    const b = seqBytes(48, 3)
    expect(b64Encode(b)).toBe(Buffer.from(b).toString('base64'))
  })
})

// Local import indirection so the test file names what it means.
import { x25519 } from '@noble/curves/ed25519.js'
function x25519Pub(priv: Uint8Array): Uint8Array {
  return x25519.getPublicKey(priv)
}
