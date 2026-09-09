// Package appview assembles the plays.bot AppView server: database pool and
// migrations, service identity keys, auth verification, the service repo
// writer, the XRPC server mount, the well-known identity documents, the
// health endpoint, and the embedded web build.
//
// New returns a fully-wired, test-bootable AppView; Handler serves it
// in-process (httptest.NewServer(appview.Handler())), Start runs the real
// HTTP listener until ctx is cancelled.
package appview

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/atmos/xrpcserver"

	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/indexer"
	"github.com/haileyok/botplaysbot/internal/keys"
	"github.com/haileyok/botplaysbot/internal/match"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/servicerepo"
)

// shutdownTimeout bounds graceful request draining.
const shutdownTimeout = 10 * time.Second

// AppView is the assembled server. Construct with New; safe for concurrent
// serving after New returns (register test endpoints before serving).
type AppView struct {
	cfg    *config.Config
	logger *slog.Logger

	pool       *pgxpool.Pool
	ownsPool   bool
	repos      *repo.Pool
	escrow     keys.EscrowKeyDirectory
	signingKey ed25519.PrivateKey

	auth   *auth.Verifier
	writer *servicerepo.Writer

	engines engine.Registry
	games   *games.Manager
	matcher *match.Matcher
	indexer *indexer.Indexer

	xrpc *xrpcserver.Server
	mux  *http.ServeMux
}

// Option customizes construction (primarily for tests).
type Option func(*options)

type options struct {
	logger *slog.Logger
	pool   *pgxpool.Pool
}

// WithLogger overrides the slog logger.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithPool injects an existing pgx pool instead of opening one from
// cfg.DatabaseURL. The injected pool is not closed by Close and migrations
// still run against it.
func WithPool(pool *pgxpool.Pool) Option {
	return func(o *options) { o.pool = pool }
}

// New builds the AppView: open the pool, apply migrations, load or create
// the service identity keys, start the auth verifier, create the service
// repo writer (inert when the service account is unconfigured), and mount
// the XRPC server, well-known documents, healthz, and the embedded web.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*AppView, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}

	av := &AppView{cfg: cfg, logger: logger}

	// 1. Database pool.
	if o.pool != nil {
		av.pool = o.pool
	} else {
		pool, err := db.OpenPool(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("appview: open pool: %w", err)
		}
		av.pool = pool
		av.ownsPool = true
	}
	av.repos = repo.New(av.pool)

	// 2. Migrations.
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		av.closePool()
		return nil, fmt.Errorf("appview: migrate: %w", err)
	}

	// 3. Service identity: Ed25519 signing key + X25519 escrow keys.
	priv, err := keys.LoadOrCreateSigningKey(cfg.ServiceSigningKeyFile)
	if err != nil {
		av.closePool()
		return nil, fmt.Errorf("appview: signing key: %w", err)
	}
	av.signingKey = priv
	av.escrow = keys.NewDBDirectory(av.pool)
	if _, err := keys.EnsureCurrent(ctx, av.escrow); err != nil {
		av.closePool()
		return nil, fmt.Errorf("appview: escrow keys: %w", err)
	}

	// 4. Auth verifier (DID resolution + session verification).
	av.auth = auth.NewVerifier(cfg, logger.With("component", "auth"))

	// 5. Service repo writer. Inert (ErrNotConfigured on write) when the
	// service account is not configured.
	writer, err := servicerepo.New(ctx, cfg, logger.With("component", "servicerepo"))
	if err != nil {
		av.auth.Close()
		av.closePool()
		return nil, fmt.Errorf("appview: service repo writer: %w", err)
	}
	av.writer = writer

	// 6. Game lifecycle: engine registry, moveToken minter, game manager
	// (with the clock sweeper), and the game XRPC endpoints.
	av.engines = engine.NewRegistry(engine.NewChess())
	minter := games.NewTokenMinter(cfg.ServiceDID, av.signingKey)
	av.games = games.NewManager(cfg, av.pool, av.repos, av.engines, minter, av.writer,
		logger.With("component", "games"))
	av.xrpc = &xrpcserver.Server{}
	games.Register(av.xrpc, av.auth, av.games)
	games.RegisterSubscriptions(av.xrpc, av.games)

	// 6b. Matchmaking (Phase D): challenges, seek pool + pairing loop,
	// subscriptions. The matcher observes finished games for no-show
	// detection and registers its own endpoints. Phase E wires the flag
	// emitter so the no-show timingAnomaly also lands as a
	// bot.plays.bot.flag record in the service repo.
	av.indexer = indexer.New(cfg, av.pool, av.repos, av.signingKey.Public().(ed25519.PublicKey), av.writer,
		logger.With("component", "indexer"))
	av.matcher = match.NewMatcher(cfg, av.pool, av.repos, av.games,
		match.ProvisionalRatingSource{}, logger.With("component", "match"),
		match.WithFlagEmitter(av.indexer.FlagEmitter()))
	av.games.SetFinishHook(av.matcher.OnFinish)
	match.Register(av.xrpc, av.auth, av.matcher)
	if err := match.RegisterSubscription(av.xrpc, av.matcher); err != nil {
		av.auth.Close()
		av.games.Close()
		av.closePool()
		return nil, fmt.Errorf("appview: register match.subscribe: %w", err)
	}
	av.games.StartSweeper(cfg.Tunables.SweeperInterval)
	av.matcher.StartPairing(cfg.Tunables.PairingInterval)
	av.indexer.Start()

	// 7. HTTP mux. The xrpc mount is wrapped so the authenticated
	// subscription endpoint (match.subscribe) gets its bearer token
	// verified — and its identity injected into the request context —
	// before atmos upgrades the connection (the subscription handler
	// itself only sees the negotiated stream, not the request).
	av.mux = http.NewServeMux()
	av.mux.HandleFunc("GET /healthz", av.handleHealthz)
	av.mux.Handle("GET /.well-known/plays-bot/escrow-keys.json", av.handleEscrowKeys())
	av.mux.Handle("GET /.well-known/plays-bot/service.json", av.handleServiceDoc())
	av.mux.Handle("/xrpc/", av.authRequiredSubscriptions(av.xrpc))
	av.mux.Handle("/", webHandler())

	return av, nil
}

// authRequiredSubscriptions verifies the bearer token for subscription
// endpoints that require auth before delegating to the xrpc server, so an
// unauthenticated upgrade is rejected with a proper XRPC 401 envelope
// rather than a post-upgrade stream close.
func (a *AppView) authRequiredSubscriptions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/xrpc/"+match.NSIDMatchSubscribe {
			const prefix = "Bearer "
			h := r.Header.Get("Authorization")
			if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
				writeXRPCError(w, http.StatusUnauthorized, "AuthRequired", "authentication required")
				return
			}
			id, err := a.auth.Verify(r.Context(), strings.TrimSpace(h[len(prefix):]))
			if err != nil {
				writeXRPCError(w, http.StatusUnauthorized, "InvalidToken", "invalid access token")
				return
			}
			r = r.WithContext(auth.WithIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// writeXRPCError writes the standard XRPC error envelope (mirrors the
// atmos xrpcserver's writeError shape).
func writeXRPCError(w http.ResponseWriter, status int, name, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(xrpc.Error{Name: name, Message: message})
	_, _ = w.Write(body)
}

// RegisterQuery registers a query (GET) endpoint, wrapped at the given auth
// mode. Phase C moves endpoint registration into business packages; tests
// use this to exercise auth before then.
func (a *AppView) RegisterQuery(nsid string, mode auth.Mode, h xrpcserver.Handler) {
	a.xrpc.HandleQuery(nsid, a.auth.Wrap(h, mode))
}

// RegisterProcedure registers a procedure (POST) endpoint, wrapped at the
// given auth mode.
func (a *AppView) RegisterProcedure(nsid string, mode auth.Mode, h xrpcserver.Handler) {
	a.xrpc.HandleProcedure(nsid, a.auth.Wrap(h, mode))
}

// Games exposes the game manager (Phase D challenge acceptance and the WS
// subscription endpoint attach through it).
func (a *AppView) Games() *games.Manager { return a.games }

// Matcher exposes the matchmaking manager (tests drive pairing passes and
// inspect the notification hub through it).
func (a *AppView) Matcher() *match.Matcher { return a.matcher }

// Indexer exposes the repo event indexer (tests inspect counters and the
// ingest pipeline; the consume loop itself started in New).
func (a *AppView) Indexer() *indexer.Indexer { return a.indexer }

// Auth exposes the verifier (tests use it to seed cache state).
func (a *AppView) Auth() *auth.Verifier { return a.auth }

// Writer exposes the service repo writer.
func (a *AppView) Writer() *servicerepo.Writer { return a.writer }

// Repos exposes the DB repositories.
func (a *AppView) Repos() *repo.Pool { return a.repos }

// Escrow exposes the escrow key directory.
func (a *AppView) Escrow() keys.EscrowKeyDirectory { return a.escrow }

// Config exposes the running configuration.
func (a *AppView) Config() *config.Config { return a.cfg }

// Handler returns the root http.Handler.
func (a *AppView) Handler() http.Handler { return a.mux }

// ServeHTTP serves a single request (implements http.Handler).
func (a *AppView) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// Start runs the HTTP server on cfg.Port until ctx is cancelled, then shuts
// down gracefully. It returns the ListenAndServe error for server failures.
func (a *AppView) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", a.cfg.Port),
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("appview listening", "port", a.cfg.Port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		a.logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("appview: graceful shutdown: %w", err)
	}
	a.logger.Info("appview stopped")
	return nil
}

// Close releases resources (auth janitor, game sweeper, pairing loop,
// indexer, pool when owned).
func (a *AppView) Close() {
	if a.indexer != nil {
		a.indexer.Close()
	}
	if a.matcher != nil {
		a.matcher.Close()
	}
	if a.games != nil {
		a.games.Close()
	}
	if a.auth != nil {
		a.auth.Close()
	}
	a.closePool()
}

func (a *AppView) closePool() {
	if a.ownsPool && a.pool != nil {
		a.pool.Close()
		a.pool = nil
	}
}
