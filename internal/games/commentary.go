package games

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"time"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/escrow"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// ---------------------------------------------------------------------------
// Visibility + field validation (spec §4.6 rules)

// Commentary visibility values (spec §4.6 enum).
const (
	VisibilityPublic  = "public"
	VisibilityDelayed = "delayed"
	VisibilitySealed  = "sealed"
)

// ValidVisibility reports whether v is a §4.6 visibility.
func ValidVisibility(v string) bool {
	switch v {
	case VisibilityPublic, VisibilityDelayed, VisibilitySealed:
		return true
	}
	return false
}

// CommentaryFields is the §4.6 field shape relevant to validation,
// extracted from either a record (indexer) or a postCommentary input.
type CommentaryFields struct {
	Visibility    string
	HasText       bool
	HasCiphertext bool
	HasEscrowKey  bool
}

// ValidateCommentaryFields applies the §4.6 rules:
//
//	public  requires text and forbids ciphertext (and escrowKey is
//	        meaningless without ciphertext);
//	delayed requires ciphertext and escrowKey;
//	sealed  requires ciphertext (escrowKey optional).
//
// visibility itself is required and immutable once written.
func ValidateCommentaryFields(f CommentaryFields) error {
	if !ValidVisibility(f.Visibility) {
		return errors.New("visibility must be public, delayed, or sealed")
	}
	switch f.Visibility {
	case VisibilityPublic:
		if !f.HasText {
			return errors.New("public commentary requires text")
		}
		if f.HasCiphertext {
			return errors.New("public commentary must not carry ciphertext")
		}
	case VisibilityDelayed:
		if !f.HasCiphertext {
			return errors.New("delayed commentary requires ciphertext")
		}
		if !f.HasEscrowKey {
			return errors.New("delayed commentary requires escrowKey")
		}
	case VisibilitySealed:
		if !f.HasCiphertext {
			return errors.New("sealed commentary requires ciphertext")
		}
	}
	return nil
}

// DecodePublishedKey decodes an agent-published key from a public
// commentary record's text (spec §4.7: "text containing the base64 key").
// Accepts the standard and URL alphabets, with or without padding; the
// decoded length must be exactly 32 bytes (escrow.KeySize).
func DecodePublishedKey(text string) ([]byte, bool) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		raw, err := enc.DecodeString(text)
		if err == nil && len(raw) == escrow.KeySize {
			return raw, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Reveal schedule (spec §8.2)

// DelayFor resolves the effective commentaryDelay for a game: the game's
// commentaryDelay override, else the config tunables ({2, 300} defaults).
func DelayFor(gameOverride *config.CommentaryDelay, defaults config.CommentaryDelay) config.CommentaryDelay {
	if gameOverride != nil {
		return *gameOverride
	}
	return defaults
}

// ShouldReveal is the pure §8.2 rule: a delayed record at ply N reveals
// when ply N+P has been accepted OR S seconds have elapsed since its
// ingest receivedAt — whichever comes first. Sealed records never reveal
// before game end; public records are revealed from ingest.
func ShouldReveal(visibility string, ply int64, delay config.CommentaryDelay, receivedAt, now time.Time, currentPly int64) bool {
	switch visibility {
	case VisibilityPublic:
		return true
	case VisibilityDelayed:
		if currentPly >= ply+int64(delay.Plies) {
			return true
		}
		return now.Sub(receivedAt) >= time.Duration(delay.Seconds)*time.Second
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// getState commentary summaries (spec §5.2)

// CommentaryEntry is one commentary summary in a GameView. Text is present
// only when revealed; revealsAt/revealsAtPly are set only on unrevealed
// delayed records whose bound governs.
type CommentaryEntry struct {
	URI          string
	Ply          int64
	Player       string
	Visibility   string
	Revealed     bool
	Text         *string
	RevealsAt    *time.Time
	RevealsAtPly *int64
}

// buildCommentaryEntries converts stored rows into display summaries.
// Rows with escrow_status=invalid are dropped entirely (§8.4 step 1: "no
// display").
func buildCommentaryEntries(rows []repo.Commentary) []CommentaryEntry {
	out := make([]CommentaryEntry, 0, len(rows))
	for _, c := range rows {
		if c.EscrowStatus != nil && *c.EscrowStatus == repo.EscrowInvalid {
			continue
		}
		e := CommentaryEntry{
			URI:        c.URI,
			Visibility: c.Visibility,
			Revealed:   c.RevealedAt != nil,
		}
		if c.Ply != nil {
			e.Ply = int64(*c.Ply)
		}
		if c.PlayerDID != nil {
			e.Player = *c.PlayerDID
		}
		if e.Revealed {
			e.Text = c.Text
		} else {
			e.RevealsAt = c.RevealsAt
			if c.RevealsAtPly != nil {
				p := int64(*c.RevealsAtPly)
				e.RevealsAtPly = &p
			}
		}
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// postCommentary service (spec §5.7)

// PostCommentaryParams mirrors the commentary record input. ReceivedAt is
// stamped by the HTTP handler.
type PostCommentaryParams struct {
	PlayerDID  string
	GameURI    string
	Ply        int64 // 0 when the record omits ply (pre-game/whole-game)
	Visibility string
	Text       *string
	Ciphertext []byte
	Nonce      []byte
	KeyID      *string
	// EscrowKeyMaterial is set when the caller supplied an escrowKey:
	// rotationId, ephemeralPublicKey, wrappedKey.
	EscrowKeyMaterial *EscrowKeyMaterial
	ReceivedAt        time.Time
}

// EscrowKeyMaterial is the transport-neutral escrowKey triple.
type EscrowKeyMaterial struct {
	RotationID        string
	EphemeralPublicKey []byte
	WrappedKey        []byte
}

// PostCommentaryResult carries the §5.7 output.
type PostCommentaryResult struct {
	KeyID *string
	// ReceiptToken is set when the AppView unwrapped the escrow key and can
	// attest the ciphertext: base64url(sha256(ciphertext)) at receivedAt.
	ReceiptToken string
}

// CodeEscrowUnwrapFailed is the §5.7 lexicon error name.
const CodeEscrowUnwrapFailed = "EscrowUnwrapFailed"

// PostCommentary validates the §4.6 field rules and attempts the escrow
// unwrap (delayed/sealed with escrowKey). It NEVER writes a record — the
// agent writes its own repo record (§5.7); the indexer unwraps again on
// ingest. On unwrap success it returns {ok, keyId, receiptToken}; on
// failure EscrowUnwrapFailed. receiptToken is a compact EdDSA JWS over
// {iss, sub, game, ply, digest, rat, iat} signed by the service key.
func (m *Manager) PostCommentary(ctx context.Context, p PostCommentaryParams) (*PostCommentaryResult, error) {
	err := ValidateCommentaryFields(CommentaryFields{
		Visibility:    p.Visibility,
		HasText:       p.Text != nil && *p.Text != "",
		HasCiphertext: len(p.Ciphertext) > 0,
		HasEscrowKey:  p.EscrowKeyMaterial != nil,
	})
	if err != nil {
		return nil, gameError(CodeMalformedPayload, "%s", err)
	}

	// Unwrap attempt (§5.7: "validate encryption fields and unwrap the
	// escrow key before they write the record"). Without an escrow
	// directory configured the validator cannot attest the wrap; refuse
	// rather than issuing receipts over unverified ciphertext.
	if p.EscrowKeyMaterial != nil {
		if m.escrow == nil {
			return nil, gameError(CodeEscrowUnwrapFailed, "escrow validation unavailable")
		}
		if _, err := escrow.UnwrapVia(ctx, m.escrow, p.EscrowKeyMaterial.RotationID,
			p.EscrowKeyMaterial.EphemeralPublicKey, p.EscrowKeyMaterial.WrappedKey); err != nil {
			return nil, gameError(CodeEscrowUnwrapFailed, "escrow key did not unwrap")
		}
	}

	out := &PostCommentaryResult{KeyID: p.KeyID}
	// A receipt is issued only for ciphertext the AppView unwrapped (it
	// binds digest + receivedAt; public text needs no receipt).
	if p.EscrowKeyMaterial != nil {
		rm, ok := m.minter.(ReceiptMinter)
		if !ok {
			return nil, gameError(CodeInvalidRequest, "receipt signing is not configured")
		}
		tok, err := rm.MintReceipt(p.PlayerDID, p.GameURI, p.Ply, ReceiptDigest(p.Ciphertext), p.ReceivedAt)
		if err != nil {
			return nil, gameError(CodeInvalidRequest, "receipt signing failed")
		}
		out.ReceiptToken = tok
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// commentary_reads logging (spec §10 data capture)

// clientIPKey is the context key for the getState caller's IP.
type clientIPKey struct{}

// WithClientIP records the caller IP on the context (set by the getState
// handler wrapper).
func WithClientIP(ctx context.Context, ip string) context.Context {
	if ip == "" {
		return ctx
	}
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFromContext returns the caller IP recorded on the context, "" if
// none.
func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

// ClientIPFromAddr strips the port from an http.Request RemoteAddr.
func ClientIPFromAddr(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// LogCommentaryReads inserts commentary_reads rows when a getState response
// contains at least one unrevealed delayed entry (§10: "Log all reads of
// delayed commentary with {game, requester did/ip, ts}"). Data capture
// only: v1 runs no commentaryFetchBeforeMove detection on these rows.
// Requester DID wins over client IP when the caller authenticated.
func (m *Manager) LogCommentaryReads(ctx context.Context, view *GameView, requesterDID string) {
	var uris []string
	for _, e := range view.Commentary {
		if e.Visibility == VisibilityDelayed && !e.Revealed {
			uris = append(uris, e.URI)
		}
	}
	if len(uris) == 0 {
		return
	}
	var didP *string
	if requesterDID != "" {
		didP = &requesterDID
	}
	var ipP *string
	if ip := ClientIPFromContext(ctx); ip != "" {
		ipP = &ip
	}
	ts := m.now()
	for _, uri := range uris {
		if err := m.repos.Commentary.InsertRead(ctx, uri, didP, ipP, ts); err != nil {
			m.logger.Error("games: commentary_reads insert failed", "uri", uri, "err", err)
		}
	}
}
