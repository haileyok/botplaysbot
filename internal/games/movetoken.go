package games

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// moveTokenSigningAlg is the compact JWS algorithm for moveTokens: EdDSA
// over the service's Ed25519 signing key (spec Appendix B: match the
// service DID key; the service key is Ed25519 per internal/keys).
var moveTokenSigningAlg = jwt.SigningMethodEdDSA

// RFC3339Millis formats a receivedAt the way moveTokens and acceptance
// responses carry it: ISO 8601 UTC with millisecond precision (spec
// Appendix B example: 2026-09-08T14:03:22.115Z).
func RFC3339Millis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// MoveTokenClaims is the exact moveToken payload (spec Appendix B). There
// are deliberately no extra claims: agents and the indexer verify these
// fields against the repo move record, and anything more would widen what a
// stolen token attests.
type MoveTokenClaims struct {
	// Issuer is the service DID.
	Issuer string `json:"iss,omitempty"`
	// Subject is the moving player's DID.
	Subject string `json:"sub,omitempty"`
	// Game is the game record AT-URI.
	Game string `json:"game"`
	// Ply is the accepted ply number (1-based).
	Ply int64 `json:"ply"`
	// Payload is the accepted move payload object, including its $type.
	Payload json.RawMessage `json:"payload"`
	// ReceivedAt is the AppView receipt time (rat), ISO 8601 UTC ms.
	ReceivedAt string `json:"rat,omitempty"`
	// IssuedAt is the unix-seconds issue time (iat).
	IssuedAt int64 `json:"iat,omitempty"`
}

// registeredClaims bridges golang-jwt's required method set; the actual
// serialization comes from the explicit fields above so the token carries
// exactly {iss, sub, game, ply, payload, rat, iat} per Appendix B.
func (c *MoveTokenClaims) GetExpirationTime() (*jwt.NumericDate, error) { return nil, nil }
func (c *MoveTokenClaims) GetNotBefore() (*jwt.NumericDate, error)      { return nil, nil }
func (c *MoveTokenClaims) GetIssuedAt() (*jwt.NumericDate, error) {
	return jwt.NewNumericDate(time.Unix(c.IssuedAt, 0)), nil
}
func (c *MoveTokenClaims) GetIssuer() (string, error)             { return c.Issuer, nil }
func (c *MoveTokenClaims) GetSubject() (string, error)            { return c.Subject, nil }
func (c *MoveTokenClaims) GetAudience() (jwt.ClaimStrings, error) { return nil, nil }

// mintMoveToken signs a moveToken. priv is the service Ed25519 key.
func mintMoveToken(priv ed25519.PrivateKey, serviceDID, playerDID, gameURI string, ply int64, payload json.RawMessage, receivedAt time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("games: moveToken requires an Ed25519 private key")
	}
	tok := jwt.NewWithClaims(moveTokenSigningAlg, &MoveTokenClaims{
		Issuer:     serviceDID,
		Subject:    playerDID,
		Game:       gameURI,
		Ply:        ply,
		Payload:    payload,
		ReceivedAt: RFC3339Millis(receivedAt),
		IssuedAt:   time.Now().Unix(),
	})
	signed, err := tok.SignedString(priv)
	if err != nil {
		return "", fmt.Errorf("games: sign moveToken: %w", err)
	}
	return signed, nil
}

// ErrInvalidMoveToken is returned for tokens that do not verify.
var ErrInvalidMoveToken = errors.New("games: invalid moveToken")

// VerifyMoveToken verifies a moveToken against the service public key and
// returns the claims. Every field is checked by the signature; callers
// cross-check the returned claims against the record they came from (spec
// §4.5 indexer validation).
func VerifyMoveToken(pub ed25519.PublicKey, token string) (*MoveTokenClaims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("games: verify requires an Ed25519 public key")
	}
	parsed, err := jwt.ParseWithClaims(token, &MoveTokenClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("%w: unexpected alg %v", ErrInvalidMoveToken, t.Header["alg"])
		}
		return ed25519.PublicKey(pub), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMoveToken, err)
	}
	claims, ok := parsed.Claims.(*MoveTokenClaims)
	if !parsed.Valid || !ok {
		return nil, ErrInvalidMoveToken
	}
	return claims, nil
}
