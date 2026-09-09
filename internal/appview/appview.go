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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jcalabro/atmos/xrpcserver"

	"github.com/haileyok/botplaysbot/internal/auth"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/keys"
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

	// 6. XRPC server: no business endpoints this phase; the mount returns
	// proper XRPC error envelopes for unknown methods.
	av.xrpc = &xrpcserver.Server{}

	// 7. HTTP mux.
	av.mux = http.NewServeMux()
	av.mux.HandleFunc("GET /healthz", av.handleHealthz)
	av.mux.Handle("GET /.well-known/plays-bot/escrow-keys.json", av.handleEscrowKeys())
	av.mux.Handle("GET /.well-known/plays-bot/service.json", av.handleServiceDoc())
	av.mux.Handle("/xrpc/", av.xrpc)
	av.mux.Handle("/", webHandler())

	return av, nil
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

// Close releases resources (auth janitor, pool when owned).
func (a *AppView) Close() {
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
