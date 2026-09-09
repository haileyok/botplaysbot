package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/escrow"
	"github.com/haileyok/botplaysbot/internal/events"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/repo"
)

// This file implements commentary ingest (spec §4.6, §8.2, §8.4):
//
//  1. Validate visibility/field constraints; invalid records are indexed
//     with escrow status "invalid" and never displayed.
//  2. escrowKey present → unwrap against any published rotation; failure
//     marks the row escrowFailed, raises an info flag, and the record is
//     treated as sealed-without-escrow.
//  3. Store ciphertext, nonce, keyId, content key, ingest receivedAt, and
//     the reveal bounds revealsAtPly = ply+P / revealsAt = receivedAt+S
//     (P/S from the game's commentaryDelay override or config defaults).
//  4. A record may carry a receiptToken (§5.7 open-property extension —
//     not a lexicon field, read from the raw record JSON). Verified
//     receipts keep their rat as provenance; missing or mismatched
//     receipts fall back to ingest receivedAt. No flag either way.
//  5. #commentaryPosted is emitted on ingest (no text, ever).
//  6. A public record whose text is exactly base64 of 32 bytes and that
//     carries keyId is a key publication (§4.7 note): constant-time
//     compare against the escrowed content key → agentPublished, or a
//     keyMismatch flag.

// receiptCarrier is retired: receiptToken is declared in the commentary
// lexicon (open-property extension) and survives both transports as the
// typed GameCommentary.ReceiptToken field.

// ingestCommentary applies §8.4 to one commentary record event.
func (in *Ingestor) ingestCommentary(ctx context.Context, ev RepoEvent) {
	if ev.Kind == KindDelete {
		// The agent withdrew their commentary record. Visibility is
		// immutable and reveals are permanent once they happened; v1
		// retains the indexed row (spec §1.3 principle 3: the repo log is
		// append-only from the AppView's perspective).
		in.log.Debug("indexer: commentary record deleted; row retained", "uri", ev.repoURI())
		return
	}
	if in.cfg.ServiceDID != "" && ev.DID == in.cfg.ServiceDID {
		// The service repo never holds commentary records (§4.2).
		in.stats.IgnoredForeign.Add(1)
		return
	}

	var rec playsbot.GameCommentary
	if err := json.Unmarshal(ev.Record, &rec); err != nil {
		in.log.Debug("indexer: undecodable commentary record", "uri", ev.repoURI(), "err", err)
		return
	}
	gameURI := rec.Game.URI
	if gameURI == "" {
		in.log.Debug("indexer: commentary record missing game strongRef", "uri", ev.repoURI())
		return
	}

	ply := rec.Ply.ValOr(0)
	now := in.now()

	// §8.4 step 1: field validation.
	fields := games.CommentaryFields{
		Visibility:    rec.Visibility,
		HasText:       rec.Text.HasVal(),
		HasCiphertext: len(rec.Ciphertext) > 0,
		HasEscrowKey:  rec.EscrowKey.HasVal(),
	}
	if err := games.ValidateCommentaryFields(fields); err != nil {
		in.indexInvalidCommentary(ctx, ev, &rec, gameURI, ply, now, err)
		return
	}

	// §8.4 step 2: unwrap when an escrowKey is present.
	var contentKey []byte
	escrowStatus := repo.EscrowOK
	if rec.EscrowKey.HasVal() {
		ek := rec.EscrowKey.Val()
		key, err := escrow.UnwrapVia(ctx, in.escrow, ek.RotationId, ek.EphemeralPublicKey, ek.WrappedKey)
		if err != nil {
			escrowStatus = repo.EscrowFailed
			in.log.Warn("indexer: escrow unwrap failed",
				"uri", ev.repoURI(), "game", gameURI, "rotation", ek.RotationId, "err", err)
			if in.emitter != nil {
				detail := fmt.Sprintf("commentary %s: escrow key for rotation %s did not unwrap: %v", ev.repoURI(), ek.RotationId, err)
				if _, ferr := in.emitter.EmitFlag(ctx, ev.DID, &gameURI, FlagEscrowFailed, SeverityInfo, detail); ferr != nil {
					in.log.Error("indexer: escrowFailed flag failed", "game", gameURI, "err", ferr)
				}
			}
		} else {
			contentKey = append([]byte(nil), key[:]...)
		}
	} else if rec.Visibility == games.VisibilitySealed {
		// Valid sealed record without escrow: revealable only if the agent
		// later publishes the key (§8.2).
		escrowStatus = repo.EscrowNone
	}

	// Reveal bounds (§8.2): delayed rows get revealsAtPly/revealsAt; escrow
	// failures are treated as sealed-without-escrow (no scheduled reveal).
	receivedAt := now
	plyInt := int(ply)
	revealsAtPly, revealsAt := in.revealBounds(ctx, gameURI, int64(ply), fields.Visibility, escrowStatus, receivedAt)

	// §8.4 step 4 / §5.7: receipt verification. Present-but-invalid →
	// receipt_valid=false and provenance falls back to ingest receivedAt
	// (no flag); absent → same fallback; valid → the receipt's rat is
	// provenance.
	receiptToken := rec.ReceiptToken.ValOr("")
	var receiptValid *bool
	var receiptRat *time.Time
	if receiptToken != "" {
		receiptValid, receiptRat = in.verifyReceipt(receiptToken, rec.Ciphertext, gameURI, int64(ply), ev.DID)
	}

	row := &repo.Commentary{
		URI:          ev.repoURI(),
		CID:          &ev.CID,
		GameURI:      gameURI,
		Ply:          &plyInt,
		PlayerDID:    &ev.DID,
		Visibility:   rec.Visibility,
		Ciphertext:   rec.Ciphertext,
		Nonce:        rec.Nonce,
		EscrowStatus: &escrowStatus,
		ReceivedAt:   &receivedAt,
		RevealsAtPly: revealsAtPly,
		RevealsAt:    revealsAt,
		ReceiptValid: receiptValid,
		ReceiptRat:   receiptRat,
	}
	if rec.Text.HasVal() {
		t := rec.Text.Val()
		row.Text = &t
	}
	if rec.KeyId.HasVal() {
		k := rec.KeyId.Val()
		row.KeyID = &k
	}
	row.ContentKey = contentKey
	if row.Visibility == games.VisibilityPublic {
		// Public commentary is revealed at ingest (spec §5.2: revealed
		// immediately).
		row.RevealedAt = &receivedAt
	}
	if err := in.repos.Commentary.Insert(ctx, row); err != nil {
		in.log.Error("indexer: commentary insert failed", "uri", ev.repoURI(), "err", err)
		return
	}
	in.stats.CommentaryIndexed.Add(1)

	// §8.4 step 5: #commentaryPosted (never carries text).
	in.publishCommentaryPosted(gameURI, row)

	// §8.4 step 6 / §4.7: key publication detection — in both arrival
	// orders (publication before or after the escrowed notes).
	in.detectKeyPublication(ctx, gameURI, ev.DID, rowKeyID(row))
}

// indexInvalidCommentary stores a §4.6-invalid record with escrow status
// invalid and no display (§8.4 step 1).
func (in *Ingestor) indexInvalidCommentary(ctx context.Context, ev RepoEvent, rec *playsbot.GameCommentary, gameURI string, ply int64, now time.Time, why error) {
	status := repo.EscrowInvalid
	plyCopy := int(ply)
	player := ev.DID
	row := &repo.Commentary{
		URI:          ev.repoURI(),
		CID:          &ev.CID,
		GameURI:      gameURI,
		Ply:          &plyCopy,
		PlayerDID:    &player,
		Visibility:   rec.Visibility,
		EscrowStatus: &status,
		ReceivedAt:   &now,
	}
	if rec.Text.HasVal() {
		t := rec.Text.Val()
		row.Text = &t
	}
	if rec.KeyId.HasVal() {
		k := rec.KeyId.Val()
		row.KeyID = &k
	}
	if err := in.repos.Commentary.Insert(ctx, row); err != nil {
		in.log.Error("indexer: invalid commentary insert failed", "uri", ev.repoURI(), "err", err)
		return
	}
	in.log.Debug("indexer: invalid commentary record (not displayed)",
		"uri", ev.repoURI(), "why", why)
}

// revealBounds computes revealsAtPly = ply+P and revealsAt = receivedAt+S
// from the game's commentaryDelay override or the config defaults (§8.2).
// Only valid delayed rows are scheduled; escrowFailed rows are treated as
// sealed-without-escrow.
func (in *Ingestor) revealBounds(ctx context.Context, gameURI string, ply int64, visibility, escrowStatus string, receivedAt time.Time) (*int, *time.Time) {
	if visibility != games.VisibilityDelayed || escrowStatus != repo.EscrowOK {
		return nil, nil
	}
	delay := in.delayFor(ctx, gameURI)
	revealsAtPly := int(ply + int64(delay.Plies))
	revealsAt := receivedAt.Add(time.Duration(delay.Seconds) * time.Second)
	return &revealsAtPly, &revealsAt
}

// delayFor resolves P/S: the game's commentaryDelay override when its game
// row carries one, else the configured defaults.
func (in *Ingestor) delayFor(ctx context.Context, gameURI string) config.CommentaryDelay {
	delay := in.cfg.Tunables.CommentaryDelay
	if g, err := in.repos.Games.Get(ctx, gameURI); err == nil && len(g.CommentaryDelay) > 0 {
		var override config.CommentaryDelay
		if json.Unmarshal(g.CommentaryDelay, &override) == nil {
			delay = override
		}
	}
	return delay
}

// verifyReceipt checks an optional receiptToken carried on the record: the
// JWS must verify against the service key and every claim must match the
// record (game, ply, player) and the record's ciphertext digest.
func (in *Ingestor) verifyReceipt(token string, ciphertext []byte, gameURI string, ply int64, playerDID string) (*bool, *time.Time) {
	claims, err := games.VerifyReceiptToken(in.pub, token)
	valid := err == nil &&
		claims.Game == gameURI &&
		claims.Ply == ply &&
		claims.Subject == playerDID &&
		claims.Digest == games.ReceiptDigest(ciphertext)
	if !valid {
		in.log.Debug("indexer: receiptToken did not verify; provenance falls back",
			"game", gameURI, "ply", ply)
		return &valid, nil
	}
	var rat *time.Time
	if parsed, perr := time.Parse(time.RFC3339, claims.ReceivedAt); perr == nil {
		rat = &parsed
	}
	return &valid, rat
}

// publishCommentaryPosted emits #commentaryPosted on the bus (no text,
// spec §5.4); game.subscribe forwards it to WebSocket subscribers.
func (in *Ingestor) publishCommentaryPosted(gameURI string, row *repo.Commentary) {
	if in.bus == nil {
		return
	}
	ply := int64(0)
	if row.Ply != nil {
		ply = int64(*row.Ply)
	}
	player := ""
	if row.PlayerDID != nil {
		player = *row.PlayerDID
	}
	ev := events.Event{Kind: events.KindCommentaryPosted, GameURI: gameURI}
	ev.CommentaryPosted.Ply = ply
	ev.CommentaryPosted.Player = player
	ev.CommentaryPosted.Visibility = row.Visibility
	ev.CommentaryPosted.RevealsAt = row.RevealsAt
	if row.RevealsAtPly != nil {
		p := int64(*row.RevealsAtPly)
		ev.CommentaryPosted.RevealsAtPly = &p
	}
	in.bus.Publish(ev)
}

// detectKeyPublication compares agent-published keys against escrowed
// content keys for one (game, player, keyId) (§4.7 note, §10 keyMismatch):
//
//   - a public record whose text is exactly base64 of 32 bytes with keyId
//     set is a key publication;
//   - a match against any escrowed content key of that (player, keyId)
//     marks agentPublished (no flag);
//   - published keys with no escrowed counterpart (or all mismatching)
//     mark key_mismatch and raise the keyMismatch flag (warning).
//
// Rows are marked with OR semantics; the flag is exactly-once per
// (subject, kind, game) via the emitter.
func (in *Ingestor) detectKeyPublication(ctx context.Context, gameURI, playerDID string, keyID string) {
	if keyID == "" {
		return
	}
	rows, err := in.repos.Commentary.ListByGame(ctx, gameURI)
	if err != nil {
		in.log.Error("indexer: publication detection failed", "game", gameURI, "err", err)
		return
	}

	var publishedKeys [][]byte
	var escrowedKeys [][]byte
	for _, c := range rows {
		if c.PlayerDID == nil || *c.PlayerDID != playerDID || c.KeyID == nil || *c.KeyID != keyID {
			continue
		}
		if c.Visibility == games.VisibilityPublic && c.Text != nil {
			if raw, ok := games.DecodePublishedKey(*c.Text); ok {
				publishedKeys = append(publishedKeys, raw)
			}
		}
		if len(c.ContentKey) == escrow.KeySize {
			escrowedKeys = append(escrowedKeys, c.ContentKey)
		}
	}
	if len(publishedKeys) == 0 || len(escrowedKeys) == 0 {
		return // nothing to compare yet
	}

	matched := false
	for _, pk := range publishedKeys {
		for _, ck := range escrowedKeys {
			if escrow.EqualBytes(pk, ck) {
				matched = true
			}
		}
	}

	if err := in.repos.Commentary.MarkPublication(ctx, gameURI, playerDID, keyID, matched, !matched); err != nil {
		in.log.Error("indexer: publication mark failed", "game", gameURI, "err", err)
	}
	if matched {
		in.log.Debug("indexer: agent key publication verified", "game", gameURI, "player", playerDID, "keyId", keyID)
		return
	}
	in.stats.KeyMismatches.Add(1)
	in.log.Warn("indexer: agent-published key does not match escrowed key",
		"game", gameURI, "player", playerDID, "keyId", keyID)
	if in.emitter != nil {
		detail := fmt.Sprintf("published key for keyId %q in game %s differs from the escrowed content key", keyID, gameURI)
		if _, ferr := in.emitter.EmitFlag(ctx, playerDID, &gameURI, FlagKeyMismatch, SeverityWarning, detail); ferr != nil {
			in.log.Error("indexer: keyMismatch flag failed", "game", gameURI, "err", ferr)
		}
	}
}

// rowKeyID extracts the keyId of the just-inserted row ("", when absent).
func rowKeyID(row *repo.Commentary) string {
	if row == nil || row.KeyID == nil {
		return ""
	}
	return *row.KeyID
}
