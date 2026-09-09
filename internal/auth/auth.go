// Package auth authenticates agents calling the AppView XRPC API.
//
// A caller presents an access JWT issued by its own PDS (an app-password
// session token in v1; OAuth in a later phase). Verification never trusts
// anything the token itself claims:
//
//  1. Parse the JWT **unverified** solely to locate the account DID and
//     reject obviously expired tokens. These claims are hints, not identity.
//  2. Resolve that DID to its PDS origin via the identity directory
//     (PLC/did:web, cached).
//  3. Call com.atproto.server.getSession **on that PDS** with the presented
//     token. The 200 response is the authoritative identity
//     {did, handle, pds}.
//
// Verified identities are cached by sha256(token) for a few minutes so
// repeated calls do not round-trip the PDS. Tokens are never logged; only
// sha256 prefixes appear in log lines.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"
	"github.com/jcalabro/gt"
	"golang.org/x/time/rate"

	"github.com/haileyok/botplaysbot/internal/config"
)

// SessionCacheTTL is how long a verified session is remembered by token
// hash before the PDS is consulted again.
const SessionCacheTTL = 5 * time.Minute

// RateLimitPerSec is the per-DID request budget (requests per second).
const RateLimitPerSec = 10

// RateLimitBurst is the bucket size; short bursts up to this are absorbed.
const RateLimitBurst = 10

// RateLimitIdleEvict is how long a DID's limiter survives without use before
// the janitor drops it, so limiter state does not leak DIDs.
const RateLimitIdleEvict = 10 * time.Minute

// janitorInterval is the cleanup cadence for session and limiter maps.
const janitorInterval = 30 * time.Second

// Identity is the verified identity of a caller. It is constructed only from
// a successful getSession call against the caller's own PDS.
type Identity struct {
	DID    string
	Handle string
	PDS    string
}

type contextKeyType struct{}

var contextKey contextKeyType

// WithIdentity returns ctx carrying id.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, contextKey, id)
}

// IdentityFromContext returns the verified identity, or nil for anonymous
// requests.
func IdentityFromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(contextKey).(*Identity)
	return id
}

// Mode declares how a wrapped endpoint treats missing or invalid auth.
type Mode int

const (
	// Optional: attach the identity when a valid token is presented; missing
	// or invalid tokens yield an anonymous request, never a rejection.
	// Public queries (getState, listGames, ...) use this.
	Optional Mode = iota
	// Required: reject with an XRPC 401 envelope when auth is missing or
	// invalid. Procedures use this.
	Required
)

// Verifier turns bearer tokens into verified identities. Safe for
// concurrent use.
type Verifier struct {
	cfg    *config.Config
	dir    *identity.Directory
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	sessions map[sessionKey]sessionEntry
	limiters map[string]*limiterEntry
	stop     chan struct{}
	stopped  sync.Once
}

type sessionKey [sha256.Size]byte

type sessionEntry struct {
	identity *Identity
	expires  time.Time
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewVerifier builds a verifier. PLC resolution goes through
// cfg.PLCDirectoryURL (point it at the dev PDS harness's PLC locally).
// The returned verifier runs a janitor goroutine; call Close to stop it.
func NewVerifier(cfg *config.Config, logger *slog.Logger) *Verifier {
	resolver := &identity.DefaultResolver{
		PLCURL: gt.Some(cfg.PLCDirectoryURL),
	}
	dir := &identity.Directory{
		// Auth only needs the PDS endpoint; the bi-directional handle check
		// would double resolution cost for no auth benefit. The handle in
		// the verified identity comes from getSession, not the DID doc.
		SkipHandleVerification: true,
		Resolver:               resolver,
		Cache:                  identity.NewLRUCache(1024, 15*time.Minute),
	}

	v := &Verifier{
		cfg:      cfg,
		dir:      dir,
		logger:   logger,
		now:      time.Now,
		sessions: make(map[sessionKey]sessionEntry),
		limiters: make(map[string]*limiterEntry),
		stop:     make(chan struct{}),
	}
	go v.janitor()
	return v
}

// Close stops the janitor goroutine.
func (v *Verifier) Close() {
	v.stopped.Do(func() { close(v.stop) })
}

func (v *Verifier) janitor() {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case now := <-ticker.C:
			v.Sweep(now)
		}
	}
}

// Sweep evicts expired session cache entries and idle rate limiters.
// Exposed for tests; called periodically by the janitor goroutine.
func (v *Verifier) Sweep(now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, e := range v.sessions {
		if now.After(e.expires) {
			delete(v.sessions, k)
		}
	}
	for did, e := range v.limiters {
		if now.Sub(e.lastSeen) > RateLimitIdleEvict {
			delete(v.limiters, did)
		}
	}
}

// hashToken derives the cache/limiter-safe form of a token. Tokens never
// enter logs or maps as plaintext.
func hashToken(token string) sessionKey {
	return sha256.Sum256([]byte(token))
}

// tokenHashPrefix is a short, loggable identifier for a token.
func tokenHashPrefix(token string) string {
	h := hashToken(token)
	return fmt.Sprintf("sha256:%x", h[:4])
}

// Verify authenticates token and returns the verified identity.
func (v *Verifier) Verify(ctx context.Context, token string) (*Identity, error) {
	key := hashToken(token)

	v.mu.Lock()
	if entry, ok := v.sessions[key]; ok && v.now().Before(entry.expires) {
		id := entry.identity
		v.mu.Unlock()
		return id, nil
	}
	v.mu.Unlock()

	// 1. Unverified parse: locate the account DID, short-circuit expiry.
	// These claims are NOT trusted as identity — the DID is only a lookup
	// key for finding the PDS that must confirm the token.
	claims, err := parseUnverified(token)
	if err != nil {
		v.logger.Warn("auth: unparseable token", "token_hash", tokenHashPrefix(token), "err", err)
		return nil, errInvalidToken
	}
	if claims.exp != nil && v.now().After(time.Unix(*claims.exp, 0)) {
		return nil, errInvalidToken
	}
	if claims.sub == "" {
		return nil, errInvalidToken
	}

	did, err := atmos.ParseDID(claims.sub)
	if err != nil {
		v.logger.Warn("auth: token carries an invalid did", "token_hash", tokenHashPrefix(token), "err", err)
		return nil, errInvalidToken
	}

	// 2. Resolve DID → PDS origin.
	ident, err := v.dir.LookupDID(ctx, did)
	if err != nil {
		v.logger.Warn("auth: did resolution failed", "token_hash", tokenHashPrefix(token), "did", did.String(), "err", err)
		return nil, errInvalidToken
	}
	pds := ident.PDSEndpoint()
	if pds == "" {
		v.logger.Warn("auth: did document has no PDS endpoint", "token_hash", tokenHashPrefix(token), "did", did.String())
		return nil, errInvalidToken
	}

	// 3. The authoritative check: getSession on the caller's own PDS.
	client := &xrpc.Client{Host: pds}
	client.SetAuth(&xrpc.AuthInfo{AccessJwt: token})
	out, err := comatproto.ServerGetSession(ctx, client)
	if err != nil {
		v.logger.Info("auth: getSession rejected token", "token_hash", tokenHashPrefix(token), "pds", pds, "err", err)
		return nil, errInvalidToken
	}

	id := &Identity{DID: out.DID, Handle: out.Handle, PDS: pds}

	v.mu.Lock()
	v.sessions[key] = sessionEntry{identity: id, expires: v.now().Add(SessionCacheTTL)}
	v.mu.Unlock()
	return id, nil
}

// unverifiedClaims mirrors the access-JWT payload fields used for locating
// the PDS. Nothing here establishes identity.
type unverifiedClaims struct {
	sub string
	exp *int64
}

func parseUnverified(token string) (*unverifiedClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: not three segments")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("jwt: decode payload: %w", err)
	}
	var payload struct {
		Sub string  `json:"sub"`
		Exp *int64  `json:"exp"`
		Aud string  `json:"aud"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("jwt: parse payload: %w", err)
	}
	return &unverifiedClaims{sub: payload.Sub, exp: payload.Exp}, nil
}

var errInvalidToken = errors.New("invalid token")

// allow reports whether did may make a request right now, updating the
// limiter's last-seen time.
func (v *Verifier) allow(did string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.limiters[did]
	if !ok {
		entry = &limiterEntry{limiter: rate.NewLimiter(RateLimitPerSec, RateLimitBurst)}
		v.limiters[did] = entry
	}
	entry.lastSeen = v.now()
	return entry.limiter.Allow()
}

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *xrpcserver.Request) (string, bool) {
	h := r.HTTPReq.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// Wrap authenticates an XRPC handler per mode.
func (v *Verifier) Wrap(h xrpcserver.Handler, mode Mode) xrpcserver.Handler {
	return xrpcserver.HandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *xrpcserver.Request) error {
		return v.serveWrapped(ctx, w, r, h, mode)
	})
}

func (v *Verifier) serveWrapped(ctx context.Context, w http.ResponseWriter, r *xrpcserver.Request, h xrpcserver.Handler, mode Mode) error {
	token, ok := bearerToken(r)
	if !ok {
		if mode == Required {
			return xrpcserver.AuthRequired("authentication required")
		}
		return h.ServeXRPC(ctx, w, r)
	}

	id, err := v.Verify(ctx, token)
	if err != nil {
		if mode == Required {
			return &xrpc.Error{StatusCode: http.StatusUnauthorized, Name: "InvalidToken", Message: "invalid access token"}
		}
		// Public endpoint with an unusable header: serve anonymously rather
		// than rejecting.
		return h.ServeXRPC(ctx, w, r)
	}

	// Per-DID budget applies wherever an identity is established, public
	// endpoint or not: a verified DID cannot buy unlimited requests.
	if !v.allow(id.DID) {
		return xrpcserver.RateLimited("rate limit exceeded")
	}

	return h.ServeXRPC(WithIdentity(ctx, id), w, r)
}
