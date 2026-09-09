package games

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// receiptSigningAlg matches moveTokens: compact EdDSA JWS over the service
// Ed25519 key (spec Appendix B key; §5.7 receiptToken "same service key").
var receiptSigningAlg = jwt.SigningMethodEdDSA

// ReceiptClaims is the exact receiptToken payload. Like MoveTokenClaims
// there are deliberately no extra claims: the receipt attests exactly that
// the AppView received (game, ply, ciphertext-digest) from player at rat —
// nothing about content, legality, or acceptance.
//
//   - iss: service DID; sub: posting player's DID
//   - game: the game record AT-URI; ply: the record's ply (0 when omitted)
//   - digest: base64url(sha256(ciphertext)) — binds the receipt to the
//     exact ciphertext bytes the agent showed the validator
//   - rat: AppView receipt time (ISO 8601 UTC ms, same shape as moveToken)
//   - iat: unix-seconds issue time
type ReceiptClaims struct {
	Issuer     string `json:"iss,omitempty"`
	Subject    string `json:"sub,omitempty"`
	Game       string `json:"game"`
	Ply        int64  `json:"ply"`
	Digest     string `json:"digest"`
	ReceivedAt string `json:"rat,omitempty"`
	IssuedAt   int64  `json:"iat,omitempty"`
}

func (c *ReceiptClaims) GetExpirationTime() (*jwt.NumericDate, error) { return nil, nil }
func (c *ReceiptClaims) GetNotBefore() (*jwt.NumericDate, error)      { return nil, nil }
func (c *ReceiptClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.IssuedAt, 0)), nil
}
func (c *ReceiptClaims) GetIssuer() (string, error)             { return c.Issuer, nil }
func (c *ReceiptClaims) GetSubject() (string, error)            { return c.Subject, nil }
func (c *ReceiptClaims) GetAudience() (jwt.ClaimStrings, error) { return nil, nil }

// ReceiptDigest computes the receipt digest over ciphertext bytes:
// base64url (unpadded) of SHA-256.
func ReceiptDigest(ciphertext []byte) string {
	sum := sha256.Sum256(ciphertext)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// mintReceiptToken signs a receiptToken (same service key as moveTokens).
func mintReceiptToken(priv ed25519.PrivateKey, serviceDID, playerDID, gameURI string, ply int64, digest string, receivedAt time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("games: receiptToken requires an Ed25519 private key")
	}
	tok := jwt.NewWithClaims(receiptSigningAlg, &ReceiptClaims{
		Issuer:     serviceDID,
		Subject:    playerDID,
		Game:       gameURI,
		Ply:        ply,
		Digest:     digest,
		ReceivedAt: RFC3339Millis(receivedAt),
		IssuedAt:   time.Now().Unix(),
	})
	signed, err := tok.SignedString(priv)
	if err != nil {
		return "", fmt.Errorf("games: sign receiptToken: %w", err)
	}
	return signed, nil
}

// ErrInvalidReceiptToken is returned for receiptTokens that do not verify.
var ErrInvalidReceiptToken = errors.New("games: invalid receiptToken")

// VerifyReceiptToken verifies a receiptToken against the service public
// key and returns the claims. Callers cross-check every returned claim
// against the record it came from; the signature alone proves only that
// the AppView once issued such a receipt.
func VerifyReceiptToken(pub ed25519.PublicKey, token string) (*ReceiptClaims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("games: verify requires an Ed25519 public key")
	}
	parsed, err := jwt.ParseWithClaims(token, &ReceiptClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("%w: unexpected alg %v", ErrInvalidReceiptToken, t.Header["alg"])
		}
		return ed25519.PublicKey(pub), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReceiptToken, err)
	}
	claims, ok := parsed.Claims.(*ReceiptClaims)
	if !parsed.Valid || !ok {
		return nil, ErrInvalidReceiptToken
	}
	return claims, nil
}
