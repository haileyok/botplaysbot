package indexer

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/servicerepo"
)

// Indexer wires a Source (per PLAYSBOT_EVENT_SOURCE) to the Ingestor and
// runs the missing-record sweep (§10). Construct with New, Start, Close.
// A nil-pointer Indexer is valid to Close (test boots that skip the
// indexer entirely).
type Indexer struct {
	cfg     *config.Config
	log     *slog.Logger
	source  *Source
	ingest  *Ingestor
	emitter *FlagEmitter

	// sweepEvery is the missing-record check cadence: window/2 clamped to
	// [250ms, 1m] so tests that shorten the window see flags promptly and
	// production sweeps no more than a couple of times per window.
	sweepEvery time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	started atomic.Bool
}

// New assembles the indexer from config. The source endpoint resolves as:
// firehose → PLAYSBOT_EVENT_SOURCE_URL, else the configured PDS URL (spec
// §2.1: the indexer consumes the PDS's subscribeRepos in single-PDS
// deployments), else inert; jetstream → PLAYSBOT_EVENT_SOURCE_URL, else
// the documented public Jetstream default. An inert indexer logs a warning
// and no-ops (tests and local spectator-only deployments).
func New(cfg *config.Config, pool *pgxpool.Pool, repos *repo.Pool, pub ed25519.PublicKey,
	writer *servicerepo.Writer, logger *slog.Logger) *Indexer {
	log := logger.With("component", "indexer")
	emitter := NewFlagEmitter(repos, writer, log)
	ingest := NewIngestor(cfg, repos, pub, emitter, log)

	idx := &Indexer{
		cfg:     cfg,
		log:     log,
		ingest:  ingest,
		emitter: emitter,
		done:    make(chan struct{}),
	}

	kind := KindFirehose
	if cfg.EventSource == config.EventSourceJetstream {
		kind = KindJetstream
	}

	rawURL := resolveURL(cfg, kind)
	if rawURL == "" {
		log.Warn("indexer: no event source endpoint (PLAYSBOT_EVENT_SOURCE_URL and PDS URL unset); indexer is inert")
		return idx
	}

	opts := SourceOptions{
		Kind:        kind,
		URL:         rawURL,
		Logger:      log,
		Collections: BotCollections(),
	}
	if kind == KindFirehose {
		// Cursor persistence (spec §2.2 step 6): resume the firehose from
		// the last processed sequence across restarts.
		opts.CursorStore = NewDBCursorStore(pool, string(kind))
	}
	idx.source = NewSource(opts)

	window := cfg.Tunables.MissingRecordWindow
	if window <= 0 {
		window = 10 * time.Minute
	}
	sweep := window / 2
	if sweep < 250*time.Millisecond {
		sweep = 250 * time.Millisecond
	}
	if sweep > time.Minute {
		sweep = time.Minute
	}
	idx.sweepEvery = sweep

	return idx
}

// resolveURL derives the websocket endpoint for kind.
func resolveURL(cfg *config.Config, kind SourceKind) string {
	if cfg.EventSourceURL != "" {
		u := toWebSocketScheme(cfg.EventSourceURL)
		if kind == KindFirehose {
			// Tolerate base-URL configs (e.g. PLAYSBOT_EVENT_SOURCE_URL=http://pds:2583):
			// a bare host has no subscribeRepos path — append the conventional one.
			rest := strings.TrimPrefix(strings.TrimPrefix(u, "wss://"), "ws://")
			if !strings.Contains(rest, "/") {
				u = strings.TrimRight(u, "/") + "/xrpc/com.atproto.sync.subscribeRepos"
			}
		}
		return u
	}
	switch kind {
	case KindFirehose:
		if cfg.PDSURL != "" {
			return toWebSocketScheme(cfg.PDSURL) + "/xrpc/com.atproto.sync.subscribeRepos"
		}
		return ""
	case KindJetstream:
		return toWebSocketScheme(config.DefaultEventSourceURL[config.EventSourceJetstream])
	default:
		return ""
	}
}

// toWebSocketScheme rewrites http(s) endpoints to ws(s) so PLAYSBOT_PDS_URL
// style http://localhost:2583 values work as stream URLs.
func toWebSocketScheme(raw string) string {
	switch {
	case strings.HasPrefix(raw, "https://"):
		return "wss://" + strings.TrimPrefix(raw, "https://")
	case strings.HasPrefix(raw, "http://"):
		return "ws://" + strings.TrimPrefix(raw, "http://")
	default:
		return raw
	}
}

// Start launches the consume loop and the missing-record sweep. Safe to
// call once; subsequent calls are no-ops.
func (ix *Indexer) Start() {
	if !ix.started.CompareAndSwap(false, true) {
		return
	}
	if ix.source == nil {
		close(ix.done)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ix.cancel = cancel

	go func() {
		defer close(ix.done)
		go ix.sweepLoop(ctx)
		if err := ix.source.Run(ctx, ix.ingest); err != nil {
			ix.log.Error("indexer: source terminated", "err", err)
		}
	}()
}

// sweepLoop runs SweepMissingRecords on the sweep cadence until ctx ends.
func (ix *Indexer) sweepLoop(ctx context.Context) {
	interval := ix.sweepEvery
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := ix.ingest.SweepMissingRecords(ctx); err != nil {
				ix.log.Error("indexer: missing-record sweep failed", "err", err)
			}
		}
	}
}

// Close stops the consume loop and waits for it.
func (ix *Indexer) Close() {
	if ix == nil {
		return
	}
	if ix.cancel != nil {
		ix.cancel()
		<-ix.done
		ix.cancel = nil
	}
}

// Ingestor exposes the pipeline (tests drive rules directly and inspect
// counters).
func (ix *Indexer) Ingestor() *Ingestor { return ix.ingest }

// FlagEmitter exposes the shared flag emitter (the matcher's timingAnomaly
// path uses the same exactly-once emission).
func (ix *Indexer) FlagEmitter() *FlagEmitter { return ix.emitter }

// SweepOnce runs one missing-record pass (tests avoid waiting for the
// ticker).
func (ix *Indexer) SweepOnce(ctx context.Context) (int, error) {
	return ix.ingest.SweepMissingRecords(ctx)
}

// StatsSnapshot reads the ingest counters.
func (ix *Indexer) StatsSnapshot() StatsSnapshot {
	return ix.ingest.SnapshotStats()
}
