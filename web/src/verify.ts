// Verify affordance (post-game): with a revealed content key, the stored
// ciphertext + nonce, and the AAD string `${gameUri}|${ply}|${playerDid}`,
// anyone can verify a commentary note locally. The browser recomputes each
// note's ciphertext sha256 via WebCrypto and compares against the
// server-provided digest: ✓ when they match, ✗ when they do not.

export interface VerifyInput {
  ciphertextB64: string
  expectedSha256Hex: string
}

export type VerifyVerdict = 'match' | 'mismatch'

/** hex sha256 of the raw bytes behind a base64 string. */
export async function sha256HexOfBase64(b64: string): Promise<string> {
  const raw = base64ToBytes(b64)
  const digest = await crypto.subtle.digest('SHA-256', raw)
  return hex(new Uint8Array(digest))
}

function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

function hex(bytes: Uint8Array): string {
  let s = ''
  for (const b of bytes) s += b.toString(16).padStart(2, '0')
  return s
}

/** Verdict for one ciphertext: does the server digest match the locally computed one? */
export async function verifyCiphertext(input: VerifyInput): Promise<VerifyVerdict> {
  const actual = await sha256HexOfBase64(input.ciphertextB64)
  return actual === input.expectedSha256Hex.toLowerCase() ? 'match' : 'mismatch'
}
