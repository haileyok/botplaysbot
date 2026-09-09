// Package botclient is the Go agent client + loop for plays.bot (spec §13),
// the Go counterpart of the TS SDK in packages/client.
//
// Composition:
//
//   - AppView XRPC calls go through an atmos xrpc.Client pointed at the
//     AppView (Host = appview URL), using the generated client funcs in
//     internal/gen/playsbot. The same client's session handles PDS repo
//     writes? No — repo writes target the PDS, so a SECOND xrpc.Client is
//     pointed at the PDS (createSession + com.atproto.repo.createRecord),
//     mirroring how the TS client splits fetch-to-appview from Agent-to-PDS.
//
//   - Every call to the AppView is paced (min interval 125ms) and backs off
//     on 429 (Retry-After or 750ms→8s exponential, up to 4 retries). The
//     AppView rate-limits to 10 req/s per DID; loops must never self-throttle
//     into a stall.
//
//   - WebSocket subscriptions dial with the xrpc.v1.json subprotocol; the
//     match.subscribe upgrade carries Authorization: Bearer <accessJwt> in
//     the HTTP header (the appview verifies the token on the upgrade itself).
//
// The agent loop (BotLoop) lives in loop.go; the random-mover author in
// random.go.
package botclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	gencomatproto "github.com/haileyok/botplaysbot/internal/gen/playsbot/comatproto"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// Subprotocol is the XRPC WS subprotocol negotiated on every subscription.
const Subprotocol = "xrpc.v1.json"

// Error is an XRPC error from the AppView (kept for parity with the TS
// SDK's XRPCError so agent code can branch on status/error name).
type Error struct {
	StatusCode int
	Name       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("xrpc %s (%d): %s", e.Name, e.StatusCode, e.Message)
}

// Session mirrors the TS SDK's Session (what createSession returns).
type Session struct {
	Handle    string `json:"handle"`
	DID       string `json:"did"`
	AccessJwt string `json:"accessJwt"`
}

// EscrowKeysDoc is the /.well-known/plays-bot/escrow-keys.json document.
type EscrowKeysDoc struct {
	Keys    []EscrowKeyDoc `json:"keys"`
	Current string         `json:"current"`
}

type EscrowKeyDoc struct {
	RotationID string `json:"rotationId"`
	PublicKey  string `json:"publicKey"` // base64url, 32 bytes
	Algorithm  string `json:"algorithm"`
	CreatedAt  string `json:"createdAt"`
}

// StrongRef is the com.atproto.repo.strongRef shape used in repo records.
type StrongRef struct {
	URI string
	CID string
}

// BotClient is an authenticated agent client for one bot account.
// Safe for concurrent use.
type BotClient struct {
	AppviewURL string
	PDSURL     string

	appview *xrpc.Client // session points at the AppView (auth for XRPC calls)
	pds     *xrpc.Client // session points at the PDS (repo writes)
	pdsAuth *xrpc.AuthInfo

	identifier string
	password   string

	// Rate discipline (spec §5.0: 10 req/s per DID at the AppView).
	paceMu    sync.Mutex
	paceTail  chan struct{} // serialized pacing turns
	nextReqAt time.Time
	throttle  time.Time

	logger Logger
}

// Logger is the minimal logging surface the client uses (*slog.Logger
// satisfies it via the Log method; see SlogLogger).
type Logger interface {
	Printf(format string, args ...any)
}

// SlogLogger adapts *slog.Logger (or any slog handler logger) to Logger at
// debug level: bot loop chatter is diagnostic, not operator-facing.
type SlogLogger struct{ L *slog.Logger }

func (s SlogLogger) Printf(format string, args ...any) {
	s.L.Debug(fmt.Sprintf(format, args...))
}

// LoginOptions mirrors the TS SDK's LoginOptions.
type LoginOptions struct {
	PDSURL     string
	Identifier string
	Password   string
}

// New creates a client; call Login before using it.
func New(appviewURL string, logger Logger) *BotClient {
	if logger == nil {
		logger = nopLogger{}
	}
	return &BotClient{
		AppviewURL: strings.TrimRight(appviewURL, "/"),
		logger:     logger,
	}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Login establishes the PDS app-password session (createSession) and the
// AppView session, mirroring PlaysClient.login.
func (c *BotClient) Login(ctx context.Context, opts LoginOptions) (*Session, error) {
	c.identifier = opts.Identifier
	c.password = opts.Password
	c.PDSURL = strings.TrimRight(opts.PDSURL, "/")

	// The PDS client owns the repo-write session.
	c.pds = &xrpc.Client{Host: c.PDSURL}
	authInfo, err := c.pds.CreateSession(ctx, opts.Identifier, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("botclient: createSession at %s: %w", c.PDSURL, err)
	}
	c.pdsAuth = authInfo

	// The AppView client authenticates with the PDS-issued access JWT (the
	// appview verifies it against the PDS). Copy the initial token; the auth
	// getter re-reads the live session so a refresh is picked up.
	c.appview = &xrpc.Client{Host: c.AppviewURL}
	c.appview.SetAuth(&xrpc.AuthInfo{
		AccessJwt:  authInfo.AccessJwt,
		RefreshJwt: authInfo.RefreshJwt,
		DID:        authInfo.DID,
		Handle:     authInfo.Handle,
	})

	return &Session{Handle: authInfo.Handle, DID: authInfo.DID, AccessJwt: authInfo.AccessJwt}, nil
}

// DID returns the logged-in account's DID.
func (c *BotClient) DID() string {
	if c.pdsAuth == nil {
		return ""
	}
	return c.pdsAuth.DID
}

// Handle returns the logged-in account's handle.
func (c *BotClient) Handle() string {
	if c.pdsAuth == nil {
		return ""
	}
	return c.pdsAuth.Handle
}

// auth returns the live access JWT. The PDS client refreshes lazily (each
// repo-write retries once after a refresh); for AppView calls we re-read the
// PDS session's token so a refresh lands on the next call.
func (c *BotClient) auth() string {
	if c.pdsAuth == nil {
		return ""
	}
	return c.pdsAuth.AccessJwt
}

// -- pacing -----------------------------------------------------------------

const (
	minIntervalMs = 125
	maxRetries429 = 4
)

// pace serializes request spacing: each turn waits until minInterval has
// passed since the previous request, plus any shared 429 throttle deadline.
func (c *BotClient) pace(ctx context.Context) error {
	c.paceMu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if c.nextReqAt.After(now) {
		wait = c.nextReqAt.Sub(now)
	}
	if c.throttle.After(now) && c.throttle.Sub(now) > wait {
		wait = c.throttle.Sub(now)
	}
	c.nextReqAt = now.Add(wait + minIntervalMs)
	turn := make(chan struct{})
	c.paceTail = turn
	c.paceMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *BotClient) throttleUntil(d time.Time) {
	c.paceMu.Lock()
	if d.After(c.throttle) {
		c.throttle = d
	}
	c.paceMu.Unlock()
}

// -- AppView XRPC -----------------------------------------------------------

// appviewCall performs a paced, 429-backoff XRPC call against the AppView
// using the generated client funcs (which take an *xrpc.Client). The AppView
// rate-limits to 10 req/s per DID.
func (c *BotClient) doAppview(ctx context.Context, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries429; attempt++ {
		if err := c.pace(ctx); err != nil {
			return err
		}
		c.logger.Printf("appview call attempt %d nsid", attempt)
		err := fn()
		c.logger.Printf("appview call attempt %d done: %v", attempt, err == nil)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err() // canceled (Stop): do not retry or wrap
		}
		var xe *xrpc.Error
		if !errors.As(err, &xe) || xe.StatusCode != http.StatusTooManyRequests {
			return err
		}
		lastErr = err
		wait := backoff429(attempt)
		c.throttleUntil(time.Now().Add(wait))
		c.logger.Printf("appview 429; backing off %s (attempt %d)", wait, attempt+1)
	}
	return lastErr
}

// backoff429 returns the exponential backoff window: 750ms→8s, capped.
func backoff429(attempt int) time.Duration {
	d := 750 * time.Millisecond << min(attempt, 4) // 750ms, 1.5s, 3s, 6s, 8s-cap
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// GetState calls bot.plays.bot.game.getState.
func (c *BotClient) GetState(ctx context.Context, game string) (*playsbot.GameGetState_Output, error) {
	var out *playsbot.GameGetState_Output
	err := c.doAppview(ctx, func() error {
		var e error
		out, e = playsbot.GameGetState(ctx, c.appviewClient(), game)
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SubmitMoveInput mirrors the TS submitMove signature.
type SubmitMoveInput struct {
	Game    string
	Ply     int64
	Payload *playsbot.GameSubmitMove_Input_Payload
}

// SubmitMove calls bot.plays.bot.game.submitMove.
func (c *BotClient) SubmitMove(ctx context.Context, in SubmitMoveInput) (*playsbot.GameSubmitMove_Output, error) {
	var out *playsbot.GameSubmitMove_Output
	err := c.doAppview(ctx, func() error {
		var e error
		out, e = playsbot.GameSubmitMove(ctx, c.appviewClient(), &playsbot.GameSubmitMove_Input{
			Game:    in.Game,
			Ply:     in.Ply,
			Payload: *in.Payload,
		})
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SeekInput mirrors the TS SeekInput.
type SeekInput struct {
	GameType      string
	Variant       string
	Rated         *bool
	RatingWindow  *int64
	MaxConcurrent int64
	TimeControl   *playsbot.BotGame_TimeControl
}

// Seek posts a matchmaking seek (mode standing by default).
func (c *BotClient) Seek(ctx context.Context, in SeekInput) (*playsbot.MatchSeek_Output, error) {
	req := &playsbot.MatchSeek_Input{
		GameType:      in.GameType,
		Variant:       gt.Some(in.Variant),
		Mode:          "standing",
		MaxConcurrent: gt.Some(in.MaxConcurrent),
	}
	if in.Rated != nil {
		req.Rated = gt.Some(*in.Rated)
	}
	if in.RatingWindow != nil {
		req.RatingWindow = gt.Some(*in.RatingWindow)
	}
	if in.TimeControl != nil {
		req.TimeControl = gt.Some(*in.TimeControl)
	}
	var out *playsbot.MatchSeek_Output
	err := c.doAppview(ctx, func() error {
		var e error
		out, e = playsbot.MatchSeek(ctx, c.appviewClient(), req)
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CancelSeek leaves the matchmaking pool.
func (c *BotClient) CancelSeek(ctx context.Context, seekID string) error {
	return c.doAppview(ctx, func() error {
		return playsbot.MatchCancelSeek(ctx, c.appviewClient(), &playsbot.MatchCancelSeek_Input{SeekId: seekID})
	})
}

// Resign resigns the game.
func (c *BotClient) Resign(ctx context.Context, game string) error {
	return c.doAppview(ctx, func() error {
		return playsbot.GameResign(ctx, c.appviewClient(), &playsbot.GameResign_Input{Game: game})
	})
}

// AcceptChallenge accepts an incoming challenge.
func (c *BotClient) AcceptChallenge(ctx context.Context, challengeID string) (*playsbot.GameAcceptChallenge_Output, error) {
	var out *playsbot.GameAcceptChallenge_Output
	err := c.doAppview(ctx, func() error {
		var e error
		out, e = playsbot.GameAcceptChallenge(ctx, c.appviewClient(), &playsbot.GameAcceptChallenge_Input{ChallengeId: challengeID})
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PostCommentaryInput mirrors the TS PostCommentaryInput (bytes fields raw).
type PostCommentaryInput struct {
	Game       StrongRef
	Ply        *int64
	Visibility string
	Text       string
	Ciphertext []byte
	Nonce      []byte
	KeyID      string
	EscrowKey  *EscrowKeyRef
	CreatedAt  string
}

// EscrowKeyRef is the escrowKey object as sent to postCommentary.
type EscrowKeyRef struct {
	RotationID         string
	EphemeralPublicKey []byte // 32 bytes
	WrappedKey         []byte // 48 bytes
}

// PostCommentary validates-and-escrows a commentary record before the repo
// write (spec §5.7). Input uses the atmos $bytes JSON dialect (the server's
// generated-code dialect); the AppView validates the wrap and returns a
// receiptToken.
func (c *BotClient) PostCommentary(ctx context.Context, in PostCommentaryInput) (*playsbot.GamePostCommentary_Output, error) {
	req := &playsbot.GamePostCommentary_Input{
		Game:       gencomatproto.RepoStrongRef{URI: in.Game.URI, CID: in.Game.CID},
		Visibility: in.Visibility,
	}
	if in.Ply != nil && *in.Ply > 0 {
		req.Ply = gt.Some(*in.Ply)
	}
	if in.Text != "" {
		req.Text = gt.Some(in.Text)
	}
	if len(in.Ciphertext) > 0 {
		req.Ciphertext = in.Ciphertext
	}
	if len(in.Nonce) > 0 {
		req.Nonce = in.Nonce
	}
	if in.KeyID != "" {
		req.KeyId = gt.Some(in.KeyID)
	}
	if in.EscrowKey != nil {
		req.EscrowKey = gt.Some(playsbot.GamePostCommentary_EscrowKey{
			RotationId:         in.EscrowKey.RotationID,
			EphemeralPublicKey: in.EscrowKey.EphemeralPublicKey,
			WrappedKey:         in.EscrowKey.WrappedKey,
		})
	}
	if in.CreatedAt != "" {
		req.CreatedAt = gt.Some(in.CreatedAt)
	}

	var out *playsbot.GamePostCommentary_Output
	err := c.doAppview(ctx, func() error {
		var e error
		out, e = playsbot.GamePostCommentary(ctx, c.appviewClient(), req)
		return e
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EscrowKeys fetches the AppView's escrow public keys document.
func (c *BotClient) EscrowKeys(ctx context.Context) (*EscrowKeysDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AppviewURL+"/.well-known/plays-bot/escrow-keys.json", nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("botclient: escrow-keys.json: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("botclient: escrow-keys.json: %d", res.StatusCode)
	}
	var doc EscrowKeysDoc
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("botclient: escrow-keys.json: %w", err)
	}
	return &doc, nil
}

// APIGame fetches the AppView site API (GET /api/game?uri=...).
func (c *BotClient) APIGame(ctx context.Context, uri string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AppviewURL+"/api/game?uri="+url.QueryEscape(uri), nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("botclient: /api/game: %d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// -- PDS repo writes ---------------------------------------------------------

// pdsClient returns the PDS xrpc client for the generated com.atproto funcs.
func (c *BotClient) pdsClient() *xrpc.Client { return c.pds }

// appviewClient returns the AppView xrpc client. Method indirection exists so
// tests can hook session refreshes later without changing call sites.
func (c *BotClient) appviewClient() *xrpc.Client {
	// Keep the AppView's bearer in sync with the PDS session (refreshes).
	if c.pdsAuth != nil && c.appview != nil {
		if cur := c.appview.Auth(); cur != nil && cur.AccessJwt != c.pdsAuth.AccessJwt {
			c.appview.SetAuth(&xrpc.AuthInfo{
				AccessJwt:  c.pdsAuth.AccessJwt,
				RefreshJwt: c.pdsAuth.RefreshJwt,
				DID:        c.pdsAuth.DID,
				Handle:     c.pdsAuth.Handle,
			})
		}
	}
	return c.appview
}

// refreshPDS refreshes (or re-creates) the PDS session. Serialized.
func (c *BotClient) refreshPDS(ctx context.Context) error {
	if _, err := c.pds.RefreshSession(ctx); err == nil {
		c.pdsAuth = c.pds.Auth()
		return nil
	}
	authInfo, err := c.pds.CreateSession(ctx, c.identifier, c.password)
	if err != nil {
		return fmt.Errorf("botclient: re-createSession at %s: %w", c.PDSURL, err)
	}
	c.pdsAuth = authInfo
	return nil
}

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

// GameRef resolves the game record's strongRef (uri + cid) via the PDS
// (com.atproto.repo.getRecord), retrying briefly for the first-pull race —
// the game record is written by the service repo and may not be indexed
// resolvable the instant #matched fires.
func (c *BotClient) GameRef(ctx context.Context, gameURI string) (StrongRef, error) {
	var out StrongRef
	uri, err := atURIParse(gameURI)
	if err != nil {
		return out, err
	}
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		if err := c.pace(ctx); err != nil {
			return out, err
		}
		rec, recErr := comatproto.RepoGetRecord(ctx, c.pdsClient(), "", uri.Collection, uri.Repo, uri.Rkey)
		if recErr != nil {
			c.logger.Printf("gameRef attempt %d: getRecord: %v", attempt, recErr)
		}
		if recErr == nil && rec != nil && rec.CID.HasVal() && rec.CID.Val() != "" {
			out = StrongRef{URI: gameURI, CID: rec.CID.Val()}
			return out, nil
		}
		if isAuthExpired(recErr) {
			if rerr := c.refreshPDS(ctx); rerr != nil {
				return out, rerr
			}
		}
	}
	return out, fmt.Errorf("botclient: resolve game ref %s after 10 attempts", gameURI)
}

// CreateMoveRecord writes a bot.plays.bot.game.move record to the agent's
// own repo using the generated record type (real CBOR bytes on the wire).
func (c *BotClient) CreateMoveRecord(ctx context.Context, rec *playsbot.GameMove) (string, string, error) {
	return c.createRecord(ctx, playsbot.NSIDGameMove, rec)
}

// CreateCommentaryRecord writes a bot.plays.bot.game.commentary record.
func (c *BotClient) CreateCommentaryRecord(ctx context.Context, rec *playsbot.GameCommentary) (string, string, error) {
	return c.createRecord(ctx, playsbot.NSIDGameCommentary, rec)
}

func (c *BotClient) createRecord(ctx context.Context, collection string, rec any) (string, string, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", "", fmt.Errorf("botclient: marshal %s record: %w", collection, err)
	}
	in := &comatproto.RepoCreateRecord_Input{
		Repo:       c.DID(),
		Collection: collection,
		Rkey:       gt.Some(tid.Next().String()),
		Record:     raw,
	}
	out, err := comatproto.RepoCreateRecord(ctx, c.pdsClient(), in)
	if isAuthExpired(err) {
		if rerr := c.refreshPDS(ctx); rerr == nil {
			out, err = comatproto.RepoCreateRecord(ctx, c.pdsClient(), in)
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("botclient: createRecord %s: %w", collection, err)
	}
	return out.URI, out.CID, nil
}

// atURIParse splits at://did/collection/rkey.
type atURI struct {
	Repo       string
	Collection string
	Rkey       string
}

func atURIParse(uri string) (atURI, error) {
	var out atURI
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return out, fmt.Errorf("botclient: bad game uri: %s", uri)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return out, fmt.Errorf("botclient: bad game uri: %s", uri)
	}
	out.Repo, out.Collection, out.Rkey = parts[0], parts[1], parts[2]
	return out, nil
}

// backoffDelay is the WS reconnect delay: capped exponential with jitter,
// mirroring the TS SDK's backoffDelay.
func backoffDelay(attempt int, base, cap time.Duration) time.Duration {
	exp := base << min(attempt, 10)
	if exp > cap {
		exp = cap
	}
	half := exp / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not security
}
