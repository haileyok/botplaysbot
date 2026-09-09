package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jcalabro/atmos/streaming"
	"github.com/jcalabro/atmos/sync"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// Handler receives the normalized stream from a Source. Both methods are
// called sequentially from the Source's run goroutine, so implementations
// need no locking (but must not block for long).
type Handler interface {
	// RepoEvent applies one record mutation.
	RepoEvent(ev RepoEvent)
	// Identity records a DID→handle binding from an #identity event.
	Identity(did, handle string)
}

// SourceKind names a transport; it doubles as the cursor row name.
type SourceKind string

const (
	KindFirehose  SourceKind = "firehose"
	KindJetstream SourceKind = "jetstream"
)

// SourceOptions configures one Source.
type SourceOptions struct {
	// Kind selects the transport (and the cursor row name).
	Kind SourceKind
	// URL is the websocket endpoint (already normalized to ws/wss).
	URL string
	// Collections restricts a Jetstream subscription (firehose ignores it;
	// the firehose is filtered in the ingest pipeline, spec §4.2).
	Collections []string
	// CursorStore persists the resume position. Production wiring enables
	// it for the firehose; tests may enable it for either transport.
	CursorStore streaming.CursorStore
	// Dial overrides the websocket dial (tests inject in-memory
	// connections; nil uses this package's websocket dialer).
	Dial *streaming.DialFunc
	// Backoff overrides reconnect timing (nil = atmos defaults).
	Backoff *streaming.BackoffPolicy
	// OnConnected, when non-nil, receives one send after the first
	// successful dial. Tests use it to synchronize with a live stream
	// before writing records. It applies to the built-in dialer; an
	// injected Dial signals readiness itself.
	OnConnected chan struct{}
	// Logger receives lifecycle/reconnect logs.
	Logger *slog.Logger
}

// Source consumes one repo-event transport and hands normalized RepoEvents
// to a Handler. Run blocks until ctx is cancelled or a non-retryable dial
// failure occurs; the atmos client performs reconnection with exponential
// backoff internally (atmos streaming.BackoffPolicy), surfaced here to the
// logger.
type Source struct {
	opts SourceOptions
	log  *slog.Logger
}

// NewSource builds a Source; it does not connect until Run.
func NewSource(opts SourceOptions) *Source {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Source{opts: opts, log: log}
}

// Run consumes events until ctx is cancelled. It returns nil on shutdown
// and the terminal error otherwise.
func (s *Source) Run(ctx context.Context, h Handler) error {
	opts := streaming.Options{
		URL: s.opts.URL,
		// Strict global ordering: the ingest pipeline is sequential by
		// design (single-process AppView), and per-game events must be
		// applied in stream order.
		Parallelism: gt.Some(1),
		// Disable the sync-1.1 commit verifier atmos auto-attaches by
		// default: its in-memory identity directory cannot resolve any
		// DID, so every commit would be dropped. v1 ingests commit diffs
		// directly; move-record trust is anchored by the moveToken
		// signature (§4.5) instead. Phase F follow-up: wire a real
		// identity directory + durable state store and re-enable.
		Verifier: gt.Some[*sync.Verifier](nil),
		OnReconnect: gt.Some(func(attempt int, delay time.Duration) {
			s.log.Warn("indexer: reconnecting", "source", s.opts.Kind,
				"attempt", attempt, "delay", delay)
		}),
	}
	if s.opts.Kind == KindJetstream && len(s.opts.Collections) > 0 {
		opts.Collections = gt.Some(s.opts.Collections)
	}
	if s.opts.CursorStore != nil {
		opts.CursorStore = gt.Some(s.opts.CursorStore)
	}
	if s.opts.Backoff != nil {
		opts.Backoff = gt.Some(*s.opts.Backoff)
	}
	if s.opts.Dial != nil {
		opts.Dial = gt.Some(*s.opts.Dial)
	} else if s.opts.OnConnected != nil {
		connected := s.opts.OnConnected
		opts.Dial = gt.Some(streaming.DialFunc(func(ctx context.Context, rawURL string, cfg streaming.DialConfig) (streaming.Conn, *http.Response, error) {
			conn, resp, err := dialWebsocket(ctx, rawURL, cfg)
			if err == nil {
				select {
				case connected <- struct{}{}:
				default:
				}
			}
			return conn, resp, err
		}))
	}

	client, err := streaming.NewClient(opts)
	if err != nil {
		return fmt.Errorf("indexer: %s client: %w", s.opts.Kind, err)
	}
	defer client.Close()

	s.log.Info("indexer: source starting", "source", s.opts.Kind, "url", redactedURL(s.opts.URL))

	normalize := normalizeFirehose
	if s.opts.Kind == KindJetstream {
		normalize = normalizeJetstream
	}

	for batch, err := range client.Events(ctx) {
		if err != nil {
			var de *streaming.DialError
			if errors.As(err, &de) {
				s.log.Error("indexer: stream dial failed terminally", "source", s.opts.Kind, "err", err)
				return err
			}
			// Transient stream errors (gap detection, slow consumer) are
			// recovered inside the client; log and keep going.
			s.log.Warn("indexer: stream error", "source", s.opts.Kind, "err", err)
			continue
		}
		for i := range batch {
			s.dispatch(h, normalize, &batch[i])
		}
	}

	s.log.Info("indexer: source stopped", "source", s.opts.Kind)
	return nil
}

func (s *Source) dispatch(h Handler, normalize func(streaming.Event) ([]RepoEvent, error), evt *streaming.Event) {
	// Identity events update the DID→handle cache on both transports.
	if evt.Identity != nil && evt.Identity.DID != "" {
		if evt.Identity.Handle.HasVal() {
			h.Identity(evt.Identity.DID, evt.Identity.Handle.Val())
		}
		return
	}
	// #account and #info frames carry nothing the v1 ingest needs; drop
	// them (debug-logged so operators can see the stream is alive).
	if evt.Commit == nil && evt.Jetstream == nil {
		s.log.Debug("indexer: dropping non-commit frame", "source", s.opts.Kind)
		return
	}

	events, err := normalize(*evt)
	if err != nil {
		s.log.Warn("indexer: dropping undecodable event", "source", s.opts.Kind, "err", err)
		return
	}
	for _, ev := range events {
		h.RepoEvent(ev)
	}
}

// BotCollections enumerates every bot.plays.bot record collection, used as
// the Jetstream wantedCollections filter.
func BotCollections() []string {
	return []string{
		playsbot.NSIDActorProfile,
		playsbot.NSIDBotGame,
		playsbot.NSIDGameMove,
		playsbot.NSIDGameCommentary,
		playsbot.NSIDGameReveal,
		playsbot.NSIDGameChallenge,
		playsbot.NSIDBotRating,
		playsbot.NSIDBotFlag,
	}
}

// redactedURL strips query parameters. A cursor is not secret, but stream
// URLs must never become a place where credentials could land if a future
// endpoint needs query auth.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable)"
	}
	u.RawQuery = ""
	return u.String()
}
