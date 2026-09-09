// Integration tests for the AppView skeleton (Phase B).
//
// Dependencies:
//   - Postgres via DATABASE_URL (`docker compose up -d db`)
//   - The dev PDS harness via node packages/dev/pds-harness.mjs for auth
//     tests (requires `pnpm install`).
//
// Each dependency is skipped with a clear message when unavailable; CI wires
// both up in a later phase (spec §14 Phase I).
package appview_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"

	"github.com/haileyok/botplaysbot/internal/appview"
	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/testutil"
)

// ---------------------------------------------------------------------------
// helpers

type xrpcErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type testEnv struct {
	cfg    *config.Config
	app    *appview.AppView
	server *httptest.Server
	client *http.Client
}

var testLogger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// testConfig builds a Config for direct appview.New construction.
// keyDir must be reused across boots of the same logical service so the
// signing key file persists.
func testConfig(t *testing.T, databaseURL string, keyDir string, pdsURL, plcURL, serviceDID string) *config.Config {
	t.Helper()
	return &config.Config{
		DatabaseURL:           databaseURL,
		PDSURL:                pdsURL,
		ServiceDID:            serviceDID,
		ServiceSigningKeyFile: filepath.Join(keyDir, "service-signing.key"),
		PLCDirectoryURL:       plcURL,
		Port:                  0, // unused: tests serve in-process
		Tunables: config.Tunables{
			PerMoveSeconds:  300,
			SweeperInterval: time.Second,
			// Phase D tunables (spec §9a/§10 defaults; matchmaking tests
			// tighten intervals in their boot helpers).
			ChallengeTTL:       10 * time.Minute,
			ChallengeMaxTTL:    24 * time.Hour,
			NoShowSuspend:      time.Hour,
			DistinctOperators:  true,
			MaxConcurrentGames: 20,
		},
	}
}

// bootApp constructs the appview and serves it in-process.
func bootApp(t *testing.T, cfg *config.Config) *testEnv {
	t.Helper()
	app, err := appview.New(context.Background(), cfg, appview.WithLogger(testLogger))
	if err != nil {
		t.Fatalf("appview.New: %v", err)
	}
	t.Cleanup(app.Close)

	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	return &testEnv{
		cfg:    cfg,
		app:    app,
		server: server,
		client: server.Client(),
	}
}

func (e *testEnv) get(t *testing.T, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	return e.do(t, http.MethodGet, path, headers)
}

// post issues a POST with an empty JSON body (procedures).
func (e *testEnv) post(t *testing.T, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	return e.do(t, http.MethodPost, path, headers)
}

func (e *testEnv) do(t *testing.T, method, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if method == http.MethodPost {
		bodyReader = strings.NewReader("{}")
	}
	req, err := http.NewRequest(method, e.server.URL+path, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func decodeJSON(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decode JSON %s: %v", body, err)
	}
}

// ---------------------------------------------------------------------------
// 1. Boot: migrations apply; /healthz OK

func TestIntegrationBootAndHealthz(t *testing.T) {
	databaseURL := testutil.IsolatedDBURL(t)

	cfg := testConfig(t, databaseURL, t.TempDir(), "", "", "did:plc:service-placeholder")
	env := bootApp(t, cfg)

	status, body := env.get(t, "/healthz", nil)
	if status != http.StatusOK {
		t.Fatalf("/healthz = %d %s", status, body)
	}
	var health struct {
		OK bool   `json:"ok"`
		DB string `json:"db"`
	}
	decodeJSON(t, body, &health)
	if !health.OK || health.DB != "ok" {
		t.Fatalf("healthz = %+v", health)
	}

	// Unknown XRPC methods still get the proper XRPC error envelope. The
	// game lifecycle endpoints are mounted as of Phase C, so use a method
	// no phase has registered yet.
	status, body = env.get(t, "/xrpc/bot.plays.bot.actor.getLeaderboard", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("unmounted xrpc query = %d %s, want 501", status, body)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "MethodNotImplemented" {
		t.Fatalf("xrpc error = %+v, want MethodNotImplemented", xerr)
	}

	// Migrations applied: the schema's core tables exist.
	if !tableExists(t, databaseURL, "games") || !tableExists(t, databaseURL, "escrow_keys") {
		t.Fatal("migrations did not create the expected tables")
	}

	// The embedded web build is served at /.
	status, body = env.get(t, "/", nil)
	if status != http.StatusOK {
		t.Fatalf("GET / = %d", status)
	}
	if !strings.Contains(string(body), "<!doctype html>") && !strings.Contains(string(body), "<html") {
		t.Fatalf("GET / returned non-HTML: %.80s", body)
	}
}

// ---------------------------------------------------------------------------
// 2. Escrow keys: endpoint shape; persistence across re-boot

type escrowKeyDoc struct {
	RotationID string `json:"rotationId"`
	PublicKey  string `json:"publicKey"`
	Algorithm  string `json:"algorithm"`
	CreatedAt  string `json:"createdAt"`
}

type escrowKeysDoc struct {
	Keys    []escrowKeyDoc `json:"keys"`
	Current string         `json:"current"`
}

func TestIntegrationEscrowKeysAndPersistence(t *testing.T) {
	databaseURL := testutil.IsolatedDBURL(t)
	keyDir := t.TempDir() // shared across the re-boot so the Ed25519 file persists

	cfg := testConfig(t, databaseURL, keyDir, "", "", "did:plc:service-placeholder")

	// First boot.
	env1 := bootApp(t, cfg)

	status, body := env1.get(t, "/.well-known/plays-bot/service.json", nil)
	if status != http.StatusOK {
		t.Fatalf("service.json = %d %s", status, body)
	}
	var svcDoc struct {
		DID              string `json:"did"`
		SigningPublicKey string `json:"signingPublicKey"`
	}
	decodeJSON(t, body, &svcDoc)
	if svcDoc.DID != "did:plc:service-placeholder" {
		t.Fatalf("service.json did = %q", svcDoc.DID)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(svcDoc.SigningPublicKey); err != nil || len(raw) != 32 {
		t.Fatalf("service.json signingPublicKey = %q (err=%v len=%d)", svcDoc.SigningPublicKey, err, len(raw))
	}
	signingKeyBoot1 := svcDoc.SigningPublicKey

	status, body = env1.get(t, "/.well-known/plays-bot/escrow-keys.json", nil)
	if status != http.StatusOK {
		t.Fatalf("escrow-keys.json = %d %s", status, body)
	}
	var doc1 escrowKeysDoc
	decodeJSON(t, body, &doc1)
	if len(doc1.Keys) != 1 {
		t.Fatalf("first boot escrow keys = %d, want 1", len(doc1.Keys))
	}
	if doc1.Keys[0].RotationID != doc1.Current {
		t.Fatal("current does not match the first key")
	}
	if doc1.Keys[0].Algorithm != "X25519" {
		t.Fatalf("algorithm = %q, want X25519", doc1.Keys[0].Algorithm)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(doc1.Keys[0].PublicKey); err != nil || len(raw) != 32 {
		t.Fatalf("publicKey %q not base64url 32B (err=%v)", doc1.Keys[0].PublicKey, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, doc1.Keys[0].CreatedAt); err != nil {
		t.Fatalf("createdAt %q unparsable: %v", doc1.Keys[0].CreatedAt, err)
	}

	// Rotate: the endpoint must now publish current + one previous.
	if _, err := keys.GenerateCurrent(context.Background(), env1.app.Escrow().(*keys.DBDirectory)); err != nil {
		t.Fatalf("rotate escrow key: %v", err)
	}
	status, body = env1.get(t, "/.well-known/plays-bot/escrow-keys.json", nil)
	if status != http.StatusOK {
		t.Fatalf("escrow-keys.json after rotate = %d %s", status, body)
	}
	var doc2 escrowKeysDoc
	decodeJSON(t, body, &doc2)
	if len(doc2.Keys) != 2 {
		t.Fatalf("keys after rotate = %d, want 2", len(doc2.Keys))
	}
	if doc2.Keys[0].RotationID == doc1.Keys[0].RotationID {
		t.Fatal("rotation did not produce a new current key")
	}
	if doc2.Keys[1].RotationID != doc1.Keys[0].RotationID {
		t.Fatalf("previous rotation %q not retained (got %q)", doc1.Keys[0].RotationID, doc2.Keys[1].RotationID)
	}
	if doc2.Current != doc2.Keys[0].RotationID {
		t.Fatal("current does not point at the newest rotation")
	}

	// Re-boot against the same database + key dir: the same current rotation
	// and the same signing key must come back.
	env2 := bootApp(t, cfg)
	status, body = env2.get(t, "/.well-known/plays-bot/escrow-keys.json", nil)
	if status != http.StatusOK {
		t.Fatalf("escrow-keys.json after re-boot = %d %s", status, body)
	}
	var doc3 escrowKeysDoc
	decodeJSON(t, body, &doc3)
	if doc3.Current != doc2.Current {
		t.Fatalf("current rotation changed across reboot: %s -> %s", doc2.Current, doc3.Current)
	}
	if len(doc3.Keys) < 2 || doc3.Keys[1].RotationID != doc1.Keys[0].RotationID {
		t.Fatalf("rotation history lost across reboot: %+v", doc3.Keys)
	}

	status, body = env2.get(t, "/.well-known/plays-bot/service.json", nil)
	if status != http.StatusOK {
		t.Fatalf("service.json after re-boot = %d %s", status, body)
	}
	var svcDoc2 struct {
		SigningPublicKey string `json:"signingPublicKey"`
	}
	decodeJSON(t, body, &svcDoc2)
	if svcDoc2.SigningPublicKey != signingKeyBoot1 {
		t.Fatalf("signing key changed across reboot: %s -> %s", signingKeyBoot1, svcDoc2.SigningPublicKey)
	}

	// The private key is still held for the current rotation.
	cur, err := env2.app.Escrow().Current(context.Background())
	if err != nil {
		t.Fatalf("current after reboot: %v", err)
	}
	if cur.PrivateKey == nil {
		t.Fatal("current escrow key lost its private key across reboot")
	}
}

// ---------------------------------------------------------------------------
// 3. Auth roundtrip against the dev PDS

const (
	nsidTestWhoami = "bot.plays.bot.test.whoami"  // auth.Required procedure
	nsidTestPublic = "bot.plays.bot.test.publicq" // auth.Optional query
)

// registerTestEndpoints mounts the test-only endpoints used to exercise the
// auth middleware. Business endpoints arrive in Phase C.
func registerTestEndpoints(t *testing.T, app *appview.AppView) {
	t.Helper()

	type whoamiOut struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}
	app.RegisterProcedure(nsidTestWhoami, auth.Required,
		xrpcserver.HandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *xrpcserver.Request) error {
			id := auth.IdentityFromContext(ctx)
			if id == nil {
				return xrpcserver.InternalError("no identity despite required auth")
			}
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(whoamiOut{DID: id.DID, Handle: id.Handle})
		}))

	type publicOut struct {
		Anonymous bool   `json:"anonymous"`
		DID       string `json:"did,omitempty"`
	}
	app.RegisterQuery(nsidTestPublic, auth.Optional,
		xrpcserver.HandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *xrpcserver.Request) error {
			id := auth.IdentityFromContext(ctx)
			out := publicOut{Anonymous: id == nil}
			if id != nil {
				out.DID = id.DID
			}
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(out)
		}))
}

func TestIntegrationAuthRoundtrip(t *testing.T) {
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, "")
	env := bootApp(t, cfg)
	registerTestEndpoints(t, env.app)

	account := harness.CreateAccount(t)

	// Session token from the agent's PDS: createSession with the app password.
	accessJwt := createSession(t, harness.PDS, account.Handle, account.AppPassword)

	authHeader := map[string]string{"Authorization": "Bearer " + accessJwt}

	// Valid token on an auth-required procedure: 200 with the verified identity.
	status, body := env.post(t, "/xrpc/"+nsidTestWhoami, authHeader)
	if status != http.StatusOK {
		t.Fatalf("whoami with valid token = %d %s", status, body)
	}
	var who struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}
	decodeJSON(t, body, &who)
	if who.DID != account.DID || who.Handle != account.Handle {
		t.Fatalf("verified identity = %+v, want did=%s handle=%s", who, account.DID, account.Handle)
	}

	// Garbage token: 401 InvalidToken XRPC envelope.
	status, body = env.post(t, "/xrpc/"+nsidTestWhoami, map[string]string{"Authorization": "Bearer garbage.token.here"})
	if status != http.StatusUnauthorized {
		t.Fatalf("whoami with garbage token = %d %s, want 401", status, body)
	}
	var xerr xrpcErrorBody
	decodeJSON(t, body, &xerr)
	if xerr.Error != "InvalidToken" {
		t.Fatalf("garbage token error = %+v, want InvalidToken", xerr)
	}

	// No header at all: 401 AuthRequired.
	status, body = env.post(t, "/xrpc/"+nsidTestWhoami, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("whoami without header = %d %s, want 401", status, body)
	}
	decodeJSON(t, body, &xerr)
	if xerr.Error != "AuthRequired" {
		t.Fatalf("missing auth error = %+v, want AuthRequired", xerr)
	}

	// A structurally valid but forged token (wrong signature): the PDS must
	// reject it, so 401.
	forged := forgeToken(t, account.DID)
	status, body = env.post(t, "/xrpc/"+nsidTestWhoami, map[string]string{"Authorization": "Bearer " + forged})
	if status != http.StatusUnauthorized {
		t.Fatalf("whoami with forged token = %d %s, want 401", status, body)
	}

	// Public query without auth works.
	status, body = env.get(t, "/xrpc/"+nsidTestPublic, nil)
	if status != http.StatusOK {
		t.Fatalf("public query without auth = %d %s", status, body)
	}
	var pub struct {
		Anonymous bool   `json:"anonymous"`
		DID       string `json:"did"`
	}
	decodeJSON(t, body, &pub)
	if !pub.Anonymous || pub.DID != "" {
		t.Fatalf("anonymous public query = %+v, want anonymous", pub)
	}

	// Public query with a garbage header still works (never reject).
	status, body = env.get(t, "/xrpc/"+nsidTestPublic, map[string]string{"Authorization": "Bearer nope"})
	if status != http.StatusOK {
		t.Fatalf("public query with bad header = %d %s, want 200", status, body)
	}
	decodeJSON(t, body, &pub)
	if !pub.Anonymous {
		t.Fatalf("public query with bad header = %+v, want anonymous", pub)
	}

	// Public query with a valid header attaches the identity.
	status, body = env.get(t, "/xrpc/"+nsidTestPublic, authHeader)
	if status != http.StatusOK {
		t.Fatalf("public query with auth = %d %s", status, body)
	}
	decodeJSON(t, body, &pub)
	if pub.Anonymous || pub.DID != account.DID {
		t.Fatalf("public query with auth = %+v, want did=%s", pub, account.DID)
	}

	// Session cache: a second call within the TTL must not re-hit the PDS.
	// Verify observably: kill the PDS; the cached identity still authenticates.
	harnessStop(t, harness)
	status, body = env.post(t, "/xrpc/"+nsidTestWhoami, authHeader)
	if status != http.StatusOK {
		t.Fatalf("whoami after PDS down (cache) = %d %s", status, body)
	}
	decodeJSON(t, body, &who)
	if who.DID != account.DID {
		t.Fatalf("cached identity = %+v", who)
	}
}

// ---------------------------------------------------------------------------
// 4. Rate limiting

func TestIntegrationRateLimit(t *testing.T) {
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, "")
	env := bootApp(t, cfg)
	registerTestEndpoints(t, env.app)

	account := harness.CreateAccount(t)
	accessJwt := createSession(t, harness.PDS, account.Handle, account.AppPassword)
	authHeader := map[string]string{"Authorization": "Bearer " + accessJwt}

	// RateLimitBurst requests pass; the next one is 429 RateLimited.
	var sawLimited bool
	for i := 0; i < auth.RateLimitBurst+2; i++ {
		status, body := env.get(t, "/xrpc/"+nsidTestPublic, authHeader)
		if status == http.StatusTooManyRequests {
			var xerr xrpcErrorBody
			decodeJSON(t, body, &xerr)
			if xerr.Error != "RateLimited" {
				t.Fatalf("429 error name = %+v, want RateLimited", xerr)
			}
			sawLimited = true
			break
		}
		if status != http.StatusOK {
			t.Fatalf("request %d = %d %s", i+1, status, body)
		}
	}
	if !sawLimited {
		t.Fatalf("no 429 within %d requests, want the per-DID budget enforced", auth.RateLimitBurst+2)
	}
}

// ---------------------------------------------------------------------------
// plumbing

func createSession(t *testing.T, pdsURL, identifier, password string) string {
	t.Helper()
	client := &xrpc.Client{Host: pdsURL}
	authInfo, err := client.CreateSession(context.Background(), identifier, password)
	if err != nil {
		t.Fatalf("createSession at %s: %v", pdsURL, err)
	}
	if authInfo.AccessJwt == "" {
		t.Fatal("createSession returned an empty access JWT")
	}
	return authInfo.AccessJwt
}

// forgeToken builds a structurally-valid JWT with an unverified sub claim
// pointing at a real DID, but no valid signature: the PDS must reject it.
func forgeToken(t *testing.T, sub string) string {
	t.Helper()
	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := b64(map[string]string{"alg": "ES256K", "typ": "JWT"})
	payload := b64(map[string]any{
		"scope": "com.atproto.access",
		"sub":   sub,
		"aud":   "did:web:bot.plays.bot",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	return header + "." + payload + ".Zm9yZ2VkLXNpZ25hdHVyZQ"
}

func harnessStop(t *testing.T, h *testutil.PDSHarness) {
	t.Helper()
	h.Shutdown()
}

func openPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return db.OpenPool(ctx, databaseURL)
}

func tableExists(t *testing.T, databaseURL, table string) bool {
	t.Helper()
	ctx := context.Background()
	pool, err := openPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).
		Scan(&exists); err != nil {
		t.Fatalf("table exists query: %v", err)
	}
	return exists
}
