import { describe, expect, it } from 'vitest'
import { verifyCiphertext, sha256HexOfBase64 } from './verify'

describe('verify affordance', () => {
  it('computes the sha256 of base64 content', async () => {
    // sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
    const hex = await sha256HexOfBase64(btoa('hello'))
    expect(hex).toBe('2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824')
  })

  it('verdicts match when digests agree, mismatch otherwise', async () => {
    const ct = btoa('note ciphertext')
    const good = await sha256HexOfBase64(ct)
    expect(await verifyCiphertext({ ciphertextB64: ct, expectedSha256Hex: good })).toBe('match')
    expect(await verifyCiphertext({ ciphertextB64: ct, expectedSha256Hex: 'deadbeef' })).toBe('mismatch')
    // Case-insensitive hex comparison.
    expect(await verifyCiphertext({ ciphertextB64: ct, expectedSha256Hex: good.toUpperCase() })).toBe('match')
  })
})
