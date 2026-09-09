// Package servicerepo writes records to the AppView service account's repo
// through its PDS (spec §3: the service DID owns all verdict records).
//
// Authentication is the v1 development flow: com.atproto.server.createSession
// with PLAYSBOT_SERVICE_DID + PLAYSBOT_SERVICE_APP_PASSWORD against
// PLAYSBOT_PDS_URL, refreshed lazily when the access token expires.
//
// Writers never panic: every failure is a typed error, logged by the caller.
package servicerepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// ErrNotConfigured is returned when the service account is not configured
// (no DID or app password). Tests and local spectating-only deployments run
// without one.
var ErrNotConfigured = errors.New("servicerepo: service DID or app password not configured")

// WriteResult identifies the written record.
type WriteResult struct {
	URI string
	CID string
}

// WriteError wraps a failed record write with the operation context.
type WriteError struct {
	// Op is "create" or "put".
	Op string
	// Collection is the record NSID.
	Collection string
	// Rkey is the record key, if one was targeted.
	Rkey string
	Err  error
}

func (e *WriteError) Error() string {
	if e.Rkey != "" {
		return fmt.Sprintf("servicerepo: %s %s/%s: %v", e.Op, e.Collection, e.Rkey, e.Err)
	}
	return fmt.Sprintf("servicerepo: %s %s: %v", e.Op, e.Collection, e.Err)
}

func (e *WriteError) Unwrap() error { return e.Err }

// Writer is an authenticated com.atproto client for the service account.
// Safe for concurrent use.
type Writer struct {
	client   *xrpc.Client
	did      string
	password string
	logger   *slog.Logger

	mu      sync.Mutex
	session *xrpc.AuthInfo
}

// New creates a writer and establishes the initial PDS session. When the
// service account is not configured, an inert writer is returned whose
// writes fail with ErrNotConfigured (nil error) so the app can still boot.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*Writer, error) {
	w := &Writer{
		did:      cfg.ServiceDID,
		password: cfg.ServiceAppPassword,
		logger:   logger,
	}
	if !w.configured() {
		logger.Warn("servicerepo: no service DID/app password configured; record writes are disabled")
		return w, nil
	}

	w.client = &xrpc.Client{Host: cfg.PDSURL}
	if _, err := w.client.CreateSession(ctx, cfg.ServiceDID, cfg.ServiceAppPassword); err != nil {
		return nil, fmt.Errorf("servicerepo: create session: %w", err)
	}
	w.session = w.client.Auth()
	logger.Info("servicerepo: session established", "pds", cfg.PDSURL, "did", cfg.ServiceDID)
	return w, nil
}

func (w *Writer) configured() bool {
	return w.did != "" && w.password != ""
}

// DID returns the service DID.
func (w *Writer) DID() string { return w.did }

// write marshals rec and creates a record with a fresh TID rkey.
func (w *Writer) write(ctx context.Context, collection string, rec any) (*WriteResult, error) {
	if !w.configured() {
		return nil, ErrNotConfigured
	}

	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, &WriteError{Op: "create", Collection: collection, Err: fmt.Errorf("marshal record: %w", err)}
	}

	rkey := tid.Next().String()
	in := &comatproto.RepoCreateRecord_Input{
		Collection: collection,
		Record:     raw,
		Repo:       w.did,
		Rkey:       gt.Some(rkey),
	}

	out, err := w.doCreate(ctx, in)
	if err != nil {
		return nil, &WriteError{Op: "create", Collection: collection, Rkey: rkey, Err: err}
	}
	return &WriteResult{URI: out.URI, CID: out.CID}, nil
}

func (w *Writer) doCreate(ctx context.Context, in *comatproto.RepoCreateRecord_Input) (*comatproto.RepoCreateRecord_Output, error) {
	out, err := comatproto.RepoCreateRecord(ctx, w.client, in)
	if !isAuthExpired(err) {
		return out, err
	}
	// Access token expired: refresh once and retry.
	if err := w.refresh(ctx); err != nil {
		return nil, err
	}
	return comatproto.RepoCreateRecord(ctx, w.client, in)
}

// putRecord writes (create or update) the record at rkey.
func (w *Writer) put(ctx context.Context, collection, rkey string, rec any) (*WriteResult, error) {
	if !w.configured() {
		return nil, ErrNotConfigured
	}

	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, &WriteError{Op: "put", Collection: collection, Rkey: rkey, Err: fmt.Errorf("marshal record: %w", err)}
	}

	in := &comatproto.RepoPutRecord_Input{
		Collection: collection,
		Record:     raw,
		Repo:       w.did,
		Rkey:       rkey,
	}

	out, err := w.doPut(ctx, in)
	if err != nil {
		return nil, &WriteError{Op: "put", Collection: collection, Rkey: rkey, Err: err}
	}
	return &WriteResult{URI: out.URI, CID: out.CID}, nil
}

func (w *Writer) doPut(ctx context.Context, in *comatproto.RepoPutRecord_Input) (*comatproto.RepoPutRecord_Output, error) {
	out, err := comatproto.RepoPutRecord(ctx, w.client, in)
	if !isAuthExpired(err) {
		return out, err
	}
	if err := w.refresh(ctx); err != nil {
		return nil, err
	}
	return comatproto.RepoPutRecord(ctx, w.client, in)
}

// refresh obtains a fresh access token via the refresh JWT. Serialized so
// concurrent expired writes refresh exactly once; a dead refresh session
// falls back to a fresh createSession.
func (w *Writer) refresh(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.client.RefreshSession(ctx); err != nil {
		w.logger.Warn("servicerepo: refresh failed; re-creating session", "err", err)
		if _, lerr := w.client.CreateSession(ctx, w.did, w.password); lerr != nil {
			return &WriteError{Op: "refresh", Collection: "com.atproto.server.createSession", Err: lerr}
		}
	}
	w.session = w.client.Auth()
	return nil
}

// isAuthExpired reports whether err is an expired/invalid token response
// from the PDS.
func isAuthExpired(err error) bool {
	var xe *xrpc.Error
	if !errors.As(err, &xe) {
		return false
	}
	if xe.StatusCode != 401 {
		return false
	}
	switch xe.Name {
	case "ExpiredToken", "InvalidToken", "AuthRequired":
		return true
	default:
		return false
	}
}

// WriteGameRecord creates a bot.plays.bot.game record with a fresh TID rkey.
func (w *Writer) WriteGameRecord(ctx context.Context, rec *playsbot.BotGame) (*WriteResult, error) {
	return w.write(ctx, playsbot.NSIDBotGame, rec)
}

// WriteGameRecordAtRkey writes the game record at the caller's rkey (create
// on first put; overwrite afterwards). Game lifecycle writes mint the rkey
// before writing so the AppView-side game URI is known before the record
// round-trips the PDS.
func (w *Writer) WriteGameRecordAtRkey(ctx context.Context, rkey string, rec *playsbot.BotGame) (*WriteResult, error) {
	return w.put(ctx, playsbot.NSIDBotGame, rkey, rec)
}

// UpdateGameRecord writes the game record at rkey (create or overwrite via
// putRecord), used to publish final status/result (spec §2.2 step 7).
func (w *Writer) UpdateGameRecord(ctx context.Context, rkey string, rec *playsbot.BotGame) (*WriteResult, error) {
	return w.put(ctx, playsbot.NSIDBotGame, rkey, rec)
}

// WriteRevealRecord creates a bot.plays.bot.game.reveal record (spec §8.2).
func (w *Writer) WriteRevealRecord(ctx context.Context, rec *playsbot.GameReveal) (*WriteResult, error) {
	return w.write(ctx, playsbot.NSIDGameReveal, rec)
}
