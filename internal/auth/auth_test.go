package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jcalabro/atmos/xrpcserver"

	"github.com/haileyok/botplaysbot/internal/config"
)

func testVerifier(t *testing.T) *Verifier {
	t.Helper()
	cfg := &config.Config{PLCDirectoryURL: "http://127.0.0.1:1"} // never dialed in these tests
	v := NewVerifier(cfg, slog.New(slog.NewJSONHandler(&strings.Builder{}, nil)))
	t.Cleanup(v.Close)
	return v
}

func TestRateLimiterPerDID(t *testing.T) {
	v := testVerifier(t)

	// 10 req/s with burst 10: the first RateLimitBurst pass, then reject.
	for i := 0; i < RateLimitBurst; i++ {
		if !v.allow("did:plc:a") {
			t.Fatalf("request %d from did:plc:a rejected, want allowed", i+1)
		}
	}
	if v.allow("did:plc:a") {
		t.Fatal("request beyond the burst budget was allowed")
	}

	// A different DID has its own bucket.
	if !v.allow("did:plc:b") {
		t.Fatal("did:plc:b was rate limited by did:plc:a's bucket")
	}

	// After the idle eviction window the state is dropped.
	v.Sweep(time.Now().Add(RateLimitIdleEvict + time.Minute))
	if len(v.limiters) != 0 {
		t.Fatalf("limiters after sweep = %d, want 0 (DIDs leaked)", len(v.limiters))
	}
	if !v.allow("did:plc:a") {
		t.Fatal("did:plc:a still limited after eviction")
	}
}

func TestSessionCacheEviction(t *testing.T) {
	v := testVerifier(t)

	fake := time.Now()
	v.now = func() time.Time { return fake }

	key := hashToken("some-token")
	v.mu.Lock()
	v.sessions[key] = sessionEntry{identity: &Identity{DID: "did:plc:x"}, expires: fake.Add(SessionCacheTTL)}
	v.mu.Unlock()

	if len(v.sessions) != 1 {
		t.Fatal("session entry missing")
	}

	fake = fake.Add(SessionCacheTTL + time.Minute)
	v.Sweep(fake)

	if len(v.sessions) != 0 {
		t.Fatalf("sessions after expiry sweep = %d, want 0", len(v.sessions))
	}
}

func TestParseUnverified(t *testing.T) {
	// A structurally valid JWT whose payload claims we must not trust.
	payload := func(fields map[string]any) string {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	cases := []struct {
		name    string
		token   string
		wantSub string
		wantErr bool
	}{
		{
			name:    "valid",
			token:   fmt.Sprintf("x.%s.y", payload(map[string]any{"sub": "did:plc:abc", "exp": time.Now().Add(time.Hour).Unix()})),
			wantSub: "did:plc:abc",
		},
		{
			name:    "no exp is fine",
			token:   fmt.Sprintf("x.%s.y", payload(map[string]any{"sub": "did:web:example.com"})),
			wantSub: "did:web:example.com",
		},
		{
			name:    "expired",
			token:   fmt.Sprintf("x.%s.y", payload(map[string]any{"sub": "did:plc:abc", "exp": time.Now().Add(-time.Hour).Unix()})),
			wantSub: "did:plc:abc",
		},
		{
			name:    "missing sub",
			token:   fmt.Sprintf("x.%s.y", payload(map[string]any{})),
			wantErr: false, // sub empty, exp nil: parse succeeds, Verify rejects later
		},
		{
			name:    "garbage",
			token:   "not-a-jwt",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := parseUnverified(tc.token)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got claims %+v", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if claims.sub != tc.wantSub {
				t.Fatalf("sub = %q, want %q", claims.sub, tc.wantSub)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	mk := func(authz string) *xrpcserver.Request {
		r := httptest.NewRequest("GET", "/xrpc/x", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		return &xrpcserver.Request{HTTPReq: r}
	}

	if tok, ok := bearerToken(mk("Bearer abc.def.ghi")); !ok || tok != "abc.def.ghi" {
		t.Fatalf("bearerToken = %q, %v", tok, ok)
	}
	// Case-insensitive scheme.
	if tok, ok := bearerToken(mk("bearer abc")); !ok || tok != "abc" {
		t.Fatalf("bearerToken lowercase = %q, %v", tok, ok)
	}
	for _, h := range []string{"", "Bearer ", "Basic abc", "Bearer"} {
		if _, ok := bearerToken(mk(h)); ok {
			t.Fatalf("bearerToken(%q) accepted, want rejected", h)
		}
	}
}

// TestWrapRequired rejects anonymous requests with an XRPC 401 envelope
// without consulting the verifier.
func TestWrapRequiredAnonymous(t *testing.T) {
	v := testVerifier(t)

	h := v.Wrap(xrpcserver.HandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *xrpcserver.Request) error {
		t.Error("handler ran for an anonymous request")
		return nil
	}), Required)

	r := httptest.NewRequest("POST", "/xrpc/test.whoami", nil)
	w := httptest.NewRecorder()
	err := h.ServeXRPC(r.Context(), w, &xrpcserver.Request{NSID: "test.whoami", HTTPReq: r})
	if err == nil {
		t.Fatal("want an error for anonymous request")
	}
	// The server renders the envelope; verify the shape here via the error.
	xe, ok := err.(interface{ Error() string })
	if !ok || !strings.Contains(xe.Error(), "401") {
		t.Fatalf("error = %v, want a 401 XRPC error", err)
	}
}
