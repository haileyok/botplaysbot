// WebSocket subscriptions for the bot loop (coder/websocket), mirroring the
// TS SDK's BaseSubscription/MatchSubscription/GameSubscription.
//
// Frames parse as {"$type":"message","payload":{...}}; the payload is handed
// to the callback. Reconnects use capped exponential backoff with jitter.
// Events are advisory only — the BotLoop never trusts them for state.
package botclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// SubscriptionFrame is the WS envelope the appview emits.
type SubscriptionFrame struct {
	Type    string         `json:"$type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Subscription is a reconnecting WebSocket subscription manager.
type Subscription struct {
	url     string
	tokenFn func() string // re-read on every (re)connect; empty = no auth
	onEvent func(payload map[string]any)
	logger  Logger

	mu      sync.Mutex
	closed  bool
	conn    *websocket.Conn
	attempt int
	cancel  context.CancelFunc
}

// MatchSubscription dials the authenticated match.subscribe stream: the
// appview verifies Authorization: Bearer <accessJwt> on the upgrade request
// itself, so the token rides the HTTP headers.
func (c *BotClient) MatchSubscription(onEvent func(payload map[string]any)) *Subscription {
	tokenFn := func() string {
		// Re-read the live PDS session token on every (re)connect so a
		// refreshed token is picked up automatically.
		if c.pdsAuth == nil {
			return ""
		}
		return c.pdsAuth.AccessJwt
	}
	return &Subscription{
		url:     c.wsURL("/xrpc/bot.plays.bot.match.subscribe", nil),
		tokenFn: tokenFn,
		onEvent: onEvent,
		logger:  c.logger,
	}
}

// GameSubscription dials the public per-game game.subscribe stream (no auth
// on the upgrade).
func (c *BotClient) GameSubscription(game string, onEvent func(payload map[string]any)) *Subscription {
	return &Subscription{
		url:     c.wsURL("/xrpc/bot.plays.bot.game.subscribe", map[string]string{"game": game}),
		onEvent: onEvent,
		logger:  c.logger,
	}
}

func (c *BotClient) wsURL(path string, params map[string]string) string {
	u := strings.Replace(c.AppviewURL, "http", "ws", 1) + path
	if len(params) > 0 {
		parts := make([]string, 0, len(params))
		for k, v := range params {
			parts = append(parts, k+"="+url.QueryEscape(v))
		}
		u += "?" + strings.Join(parts, "&")
	}
	return u
}

// Start launches the connect/reconnect loop in a goroutine. The subscription
// hangs off a DEDICATED context (not the loop's) so closing the subscription
// cancels only its socket reads — the loop context would tie socket reads to
// the caller's context, which is what we want; but deriving from the parent
// here means Close() must cancel the child, which it does via s.cancel.
func (s *Subscription) Start(ctx context.Context) {
	child, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.loop(child)
}

// Close stops reconnecting for good and force-drops the socket.
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conn := s.conn
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// KillSocket force-drops the underlying socket without marking closed (the
// reconnect loop recovers) — the TS loop's killSocket test hook.
func (s *Subscription) KillSocket() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (s *Subscription) loop(ctx context.Context) {
	for {
		s.mu.Lock()
		closed := s.closed
		attempt := s.attempt
		s.mu.Unlock()
		if closed {
			return
		}

		dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
		conn, err := s.dial(dialCtx)
		cancelDial()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			s.attempt++
			s.mu.Unlock()
			delay := backoffDelay(attempt, 250*time.Millisecond, 8*time.Second)
			s.logger.Printf("ws dial %s failed (%v); retry in %s", s.url, err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}

		s.mu.Lock()
		s.conn = conn
		s.attempt = 0
		s.mu.Unlock()

		s.readAll(ctx, conn)

		s.mu.Lock()
		wasClosed := s.closed
		s.conn = nil
		if !wasClosed {
			s.attempt++
		}
		s.mu.Unlock()
		if wasClosed || ctx.Err() != nil {
			return
		}
		// Small settle delay before reconnecting.
		delay := backoffDelay(s.attemptValue(), 250*time.Millisecond, 8*time.Second)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (s *Subscription) attemptValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempt
}

func (s *Subscription) dial(ctx context.Context) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{
		Subprotocols: []string{Subprotocol},
	}
	if s.tokenFn != nil {
		if token := s.tokenFn(); token != "" {
			opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
		}
	}
	conn, resp, err := websocket.Dial(ctx, s.url, opts)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("dial %s: %v (status %d)", s.url, err, status)
	}
	return conn, nil
}

// readAll pumps frames until the socket dies. Unparseable frames are ignored
// (WS events are advisory, never trusted).
func (s *Subscription) readAll(ctx context.Context, conn *websocket.Conn) {
	defer func() { _ = conn.CloseNow() }()
	for {
		ctxRead, cancel := context.WithTimeout(ctx, 24*time.Hour)
		_, data, err := conn.Read(ctxRead)
		cancel()
		if err != nil {
			return
		}
		var frame SubscriptionFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		if frame.Type != "message" || frame.Payload == nil {
			continue
		}
		// Panics in a callback must not kill the pump.
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Printf("ws event handler panicked: %v", r)
				}
			}()
			s.onEvent(frame.Payload)
		}()
	}
}
