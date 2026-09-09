// BotLoop — the always-on Go agent loop (spec §9a.6), the counterpart of the
// TS SDK's AgentLoop in packages/client/src/agent-loop.ts.
//
//	document. RECONCILIATION RULE (identical to the TS loop): WS events are
//
// advisory only. After any reconnect or state discrepancy, the loop re-pulls
// getState and acts on that state — it never replays or trusts event history.
// Concretely:
//   - #matched is a *hint*; the play session keyed by game URI is idempotent.
//   - #move is ignored (the play loop drives itself via getState polling).
//   - #gameFinished triggers key publication; the poll also terminates the
//     session, so a missed event changes nothing.
//   - The ply cap counts ACCEPTED MOVES, then the loop RESIGNS (§5.6) so the
//     game terminates rather than stalling to the move clock.
//
// The loop is poll-driven with event wake: a slow safety poll (pollMs)
// reconciles state; #move/#gameFinished WS frames wake a sleeping poll early
// through a channel, so latency stays event-driven without trusting events.
package botclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/haileyok/botplaysbot/internal/escrow"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// Visibility is a commentary visibility (spec §4.6).
type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityDelayed Visibility = "delayed"
	VisibilitySealed  Visibility = "sealed"
)

// Note is one commentary note the author wants attached to a move.
type Note struct {
	Text       string
	Visibility Visibility
}

// Author is the agent-authoring surface (spec §13), the Go AgentAuthor.
type Author interface {
	// ChooseMove returns the payload to submit for the current state. The
	// payload MUST set $type (e.g. bot.plays.bot.chess.move) — the AppView
	// rejects payloads without it.
	ChooseMove(state *playsbot.GameGetState_Output) (*playsbot.GameSubmitMove_Input_Payload, error)
	// Explain optionally explains the move. Return nil for no commentary.
	Explain(state *playsbot.GameGetState_Output, payload *playsbot.GameSubmitMove_Input_Payload) *Note
	// ChallengePolicy decides an incoming challenge; nil means accept rated
	// challenges (the TS default).
	ChallengePolicy(ch *MatchEventChallengeReceived) bool
}

// SeekOptions configures the standing seek.
type SeekOptions struct {
	GameType      string
	Variant       string
	Rated         *bool
	RatingWindow  *int64
	MaxConcurrent int64
}

// LoopOptions configures a BotLoop.
type LoopOptions struct {
	Client *BotClient
	Author Author
	Seek   *SeekOptions
	// PollMs is the safety-poll interval while it is the loop's turn.
	PollMs time.Duration
	// MaxPlies caps accepted moves per game; on reaching the cap the loop
	// resigns (never stalls the game to the clock).
	MaxPlies int64
	Logger   Logger
}

// ActiveGame reports the oldest live session's game URI (test convenience:
// assertions pin to the game the loop is actually playing).
func (l *BotLoop) ActiveGame() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for uri := range l.sessions {
		return uri
	}
	return ""
}

// Idle reports whether the loop has no live sessions.
func (l *BotLoop) Idle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions) == 0
}

type playSession struct {
	gameURI    string
	gameRef    StrongRef
	contentKey escrow.ContentKey
	keyID      string
	escrowKey  *EscrowKeyRef // lazily wrapped to the appview's current rotation

	mu        sync.Mutex
	running   bool
	published bool
	moves     int64 // accepted moves (the ply-cap counter)

	wake chan struct{} // closed+replaced to wake a sleeping poll
}

func (s *playSession) wakeUp() {
	s.mu.Lock()
	ch := s.wake
	s.wake = make(chan struct{})
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (s *playSession) waitWake(d time.Duration) {
	s.mu.Lock()
	ch := s.wake
	s.mu.Unlock()
	if ch == nil {
		time.Sleep(d)
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}

// BotLoop is the always-on agent loop. Create with NewLoop, then Start; Stop
// cancels the standing seek and resigns live games best-effort.
type BotLoop struct {
	client   *BotClient
	author   Author
	seek     SeekOptions
	pollMs   time.Duration
	maxPlies int64
	logger   Logger

	mu       sync.Mutex
	sessions map[string]*playSession
	seekID   string
	stopped  bool
	sub      *Subscription
	cancel   context.CancelFunc
}

// NewLoop constructs a loop (not started).
func NewLoop(opts LoopOptions) *BotLoop {
	if opts.PollMs <= 0 {
		opts.PollMs = 500 * time.Millisecond
	}
	if opts.MaxPlies <= 0 {
		opts.MaxPlies = 1000
	}
	seek := opts.Seek
	if seek == nil {
		seek = &SeekOptions{GameType: "bot.plays.bot.chess.move", MaxConcurrent: 3}
	}
	if seek.GameType == "" {
		seek.GameType = "bot.plays.bot.chess.move"
	}
	if seek.MaxConcurrent <= 0 {
		seek.MaxConcurrent = 3
	}
	logger := opts.Logger
	if logger == nil {
		logger = nopLogger{}
	}
	return &BotLoop{
		client:   opts.Client,
		author:   opts.Author,
		seek:     *seek,
		pollMs:   opts.PollMs,
		maxPlies: opts.MaxPlies,
		logger:   logger,
		sessions: make(map[string]*playSession),
	}
}

// Start posts the standing seek and connects match.subscribe.
func (l *BotLoop) Start(ctx context.Context) error {
	if l.stopped {
		return errors.New("botloop: already stopped")
	}
	ctx, l.cancel = context.WithCancel(ctx)

	// Connect the authenticated match subscription first: presence is how
	// standing seeks get paired (the matcher expires seeks whose DID has no
	// live match.subscribe connection).
	sub := l.client.MatchSubscription(func(payload map[string]any) {
		l.onMatchEvent(ctx, payload)
	})
	l.sub = sub
	sub.Start(ctx)

	out, err := l.client.Seek(ctx, SeekInput{
		GameType:      l.seek.GameType,
		Variant:       l.seek.Variant,
		Rated:         l.seek.Rated,
		RatingWindow:  l.seek.RatingWindow,
		MaxConcurrent: l.seek.MaxConcurrent,
	})
	if err != nil {
		return fmt.Errorf("botloop: seek: %w", err)
	}
	l.mu.Lock()
	l.seekID = out.SeekId
	l.mu.Unlock()
	l.logger.Printf("seek posted %s status=%s", out.SeekId, out.Status)
	if out.Status == "matched" && out.Game.HasVal() {
		l.startSession(ctx, out.Game.Val())
	}
	return nil
}

// Stop cancels the seek, closes subscriptions, and resigns live games
// best-effort so stopped loops don't leave active games pinning the agent's
// maxConcurrent budget until the move clock times out.
// CancelSeek withdraws the loop's standing seek WITHOUT stopping the loop or
// touching live games — used by tests (and operators) to pin the loop to its
// current games so no further pairing can occur.
func (l *BotLoop) CancelSeek(ctx context.Context) {
	l.mu.Lock()
	seekID := l.seekID
	l.seekID = ""
	l.mu.Unlock()
	if seekID != "" {
		if err := l.client.CancelSeek(ctx, seekID); err != nil {
			l.logger.Printf("cancel seek failed: %v", err)
		}
	}
}

func (l *BotLoop) Stop(ctx context.Context) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	seekID := l.seekID
	sessions := make([]*playSession, 0, len(l.sessions))
	for _, s := range l.sessions {
		s.mu.Lock()
		wasRunning := s.running
		s.running = false
		s.mu.Unlock()
		if wasRunning {
			sessions = append(sessions, s)
		}
	}
	l.mu.Unlock()

	for _, s := range sessions {
		if err := l.client.Resign(ctx, s.gameURI); err != nil {
			l.logger.Printf("resign on stop failed for %s: %v", s.gameURI, err)
		}
	}
	if seekID != "" {
		_ = l.client.CancelSeek(ctx, seekID)
	}
	if l.sub != nil {
		l.sub.Close()
	}
	if l.cancel != nil {
		l.cancel()
	}
}

// onMatchEvent dispatches match.subscribe frames. Events are advisory.
func (l *BotLoop) onMatchEvent(ctx context.Context, payload map[string]any) {
	typ, _ := payload["$type"].(string)
	l.logger.Printf("match event: %s", typ)
	switch typ {
	case "bot.plays.bot.match.subscribe#matched":
		game, _ := payload["game"].(string)
		l.logger.Printf("#matched: %s", game)
		if game != "" {
			l.startSession(ctx, game)
		}
	case "bot.plays.bot.match.subscribe#challengeReceived":
		ch := &MatchEventChallengeReceived{}
		ch.ChallengeID, _ = payload["challengeId"].(string)
		ch.Challenger, _ = payload["challenger"].(string)
		ch.GameType, _ = payload["gameType"].(string)
		if rated, ok := payload["rated"].(bool); ok {
			ch.Rated = &rated
		}
		policy := l.author.ChallengePolicy
		if policy == nil {
			policy = func(c *MatchEventChallengeReceived) bool { return c.Rated == nil || *c.Rated }
		}
		if policy(ch) {
			if _, err := l.client.AcceptChallenge(ctx, ch.ChallengeID); err != nil {
				l.logger.Printf("acceptChallenge failed: %v", err)
			}
		}
	default:
		l.logger.Printf("unhandled match event payload: %v", payload)
	}
}

// MatchEventChallengeReceived mirrors the TS ChallengeReceivedPayload.
type MatchEventChallengeReceived struct {
	ChallengeID string
	Challenger  string
	GameType    string
	Rated       *bool
}

// startSession is idempotent: double #matched or a matched seek + event
// produce one session.
func (l *BotLoop) startSession(ctx context.Context, gameURI string) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	if _, ok := l.sessions[gameURI]; ok {
		l.mu.Unlock()
		return
	}
	ck, err := escrow.GenerateContentKey()
	if err != nil {
		l.mu.Unlock()
		l.logger.Printf("content key generation failed: %v", err)
		return
	}
	s := &playSession{
		gameURI:    gameURI,
		running:    true,
		contentKey: ck,
		keyID:      fmt.Sprintf("key-%s", newUUID()),
		wake:       make(chan struct{}),
	}
	l.sessions[gameURI] = s
	l.mu.Unlock()

	l.logger.Printf("starting play session for %s", gameURI)
	go func() {
		if err := l.runSession(ctx, s); err != nil {
			l.logger.Printf("session crashed %s: %v", gameURI, err)
			l.mu.Lock()
			delete(l.sessions, gameURI)
			l.mu.Unlock()
		}
	}()
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// runSession is the play loop: getState is authoritative; events only wake.
func (l *BotLoop) runSession(ctx context.Context, s *playSession) error {
	// Whatever exit path this session takes (finish, cap-resign, crash,
	// context cancel), it no longer counts as live once the goroutine ends.
	defer func() {
		l.mu.Lock()
		delete(l.sessions, s.gameURI)
		l.mu.Unlock()
	}()

	// Resolve the game record's strongRef (service-repo write may lag #matched).
	l.logger.Printf("session %s: resolving game ref", s.gameURI)
	ref, err := l.client.GameRef(ctx, s.gameURI)
	if err != nil {
		return err
	}
	s.gameRef = ref
	l.logger.Printf("session %s: game ref resolved", s.gameURI)

	// Subscribe to per-game events (public). They only wake the poll.
	gameSub := l.client.GameSubscription(s.gameURI, func(map[string]any) {
		s.wakeUp()
	})
	gameSub.Start(ctx)
	defer gameSub.Close()

	// The ply cap counts ACCEPTED MOVES, not iterations; a hard iteration
	// guard (40× cap) still bounds runaway loops.
	maxIter := l.maxPlies * 40
	for iter := int64(0); iter < maxIter; iter++ {
		if iter < 3 {
			l.logger.Printf("session %s: iter %d", s.gameURI, iter)
		}
		if iter < 3 {
			l.logger.Printf("session %s: iter %d (pre-getState)", s.gameURI, iter)
		}
		l.mu.Lock()
		stopped := l.stopped
		l.mu.Unlock()
		s.mu.Lock()
		running, moves := s.running, s.moves
		s.mu.Unlock()
		if stopped || !running || moves >= l.maxPlies {
			break
		}

		state, err := l.client.GetState(ctx, s.gameURI)
		if err != nil {
			// Context canceled (Stop): the resign already happened there.
			if ctx.Err() != nil {
				return nil
			}
			l.logger.Printf("getState failed: %v", err)
			l.sleepCtx(ctx, l.pollMs*4)
			continue
		}
		if iter < 5 {
			l.logger.Printf("session %s: iter %d ply=%d turn=%s me=%s status=%s", s.gameURI, iter, state.Ply, state.Turn, l.client.DID(), state.Game.Status)
		}

		if state.Game.Status != "active" {
			// Game over on first pull (or after our own move ended it).
			l.finishSession(ctx, s)
			return nil
		}

		if state.Turn != l.client.DID() {
			// Not our turn: sleep until a #move wakes us or the safety poll
			// elapses, then re-check state.
			s.waitWake(l.pollMs * 6)
			continue
		}

		payload, err := l.author.ChooseMove(state)
		if err != nil {
			l.logger.Printf("chooseMove failed: %v", err)
			l.sleepCtx(ctx, l.pollMs)
			continue
		}

		res, err := l.client.SubmitMove(ctx, SubmitMoveInput{
			Game:    s.gameURI,
			Ply:     state.Ply + 1,
			Payload: payload,
		})
		if err != nil {
			// Rejected (NotYourTurn after opponent moved, PlyMismatch after a
			// retry, IllegalMove): re-pull state and re-derive — never replay.
			if ctx.Err() != nil {
				return nil
			}
			l.logger.Printf("submitMove rejected: %v", err)
			l.sleepCtx(ctx, l.pollMs)
			continue
		}
		if !res.Accepted {
			l.sleepCtx(ctx, l.pollMs)
			continue
		}

		l.writeMoveRecord(ctx, s, res, payload)
		l.maybeExplain(ctx, s, res, payload, state)

		s.mu.Lock()
		s.moves++
		s.mu.Unlock()

		if res.GameOver.HasVal() {
			// Our move ended the game: publish keys and return.
			l.finishSession(ctx, s)
			return nil
		}
	}

	// Ply cap reached with the game still running: resign so the game (and
	// the reveal/key-publication flow) actually terminates instead of
	// stalling until the move clock times out (§5.6).
	s.mu.Lock()
	running, moves := s.running, s.moves
	s.mu.Unlock()
	if running && moves >= l.maxPlies {
		l.logger.Printf("ply cap reached; resigning %s", s.gameURI)
		if err := l.client.Resign(ctx, s.gameURI); err != nil {
			l.logger.Printf("resign failed: %v", err)
		}
		l.finishSession(ctx, s)
	}
	return nil
}

func (l *BotLoop) sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// writeMoveRecord writes the move record (spec §4.5) to the agent's own repo.
// The AppView state is authoritative; a failed repo write must not stop the
// game (the indexer flags the missing record per §9).
func (l *BotLoop) writeMoveRecord(ctx context.Context, s *playSession, res *playsbot.GameSubmitMove_Output, payload *playsbot.GameSubmitMove_Input_Payload) {
	rec := &playsbot.GameMove{
		Game:             genStrongRef(s.gameRef),
		Ply:              res.Ply,
		Payload:          genMovePayload(payload),
		ReceivedAt:       res.ReceivedAt,
		ClockRemainingMs: gtOptionInt64(res.ClockRemainingMs),
		MoveToken:        res.MoveToken,
	}
	if pos := chessPosition(&res.State); pos != nil {
		rec.Position = gtOptionMovePosition(*pos)
	}
	if _, _, err := l.client.CreateMoveRecord(ctx, rec); err != nil {
		l.logger.Printf("move record write failed: %v", err)
	}
}

// maybeExplain writes the author's commentary (spec §4.6):
//   - public → repo plaintext;
//   - delayed → encrypt under the session key + escrow-wrap to the AppView's
//     current rotation so the reveal scheduler can decrypt on schedule (§8.1/§8.2);
//   - sealed → encrypt under the session key; revealed at game end via key
//     publication.
//
// Commentary is optional; never fail the game over it.
func (l *BotLoop) maybeExplain(ctx context.Context, s *playSession, res *playsbot.GameSubmitMove_Output, payload *playsbot.GameSubmitMove_Input_Payload, state *playsbot.GameGetState_Output) {
	note := l.author.Explain(state, payload)
	if note == nil || note.Text == "" {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			l.logger.Printf("explain panicked: %v", err)
		}
	}()

	createdAt := time.Now().UTC().Format(RFC3339MilliLike)
	ply := res.Ply

	if note.Visibility == VisibilityPublic {
		rec := &playsbot.GameCommentary{
			Game:       genStrongRef(s.gameRef),
			Ply:        gtOptionInt64(ply),
			Visibility: string(note.Visibility),
			Text:       gtOptionString(note.Text),
			CreatedAt:  createdAt,
		}
		l.writeAndPostCommentary(ctx, s, rec, note)
		return
	}

	// delayed/sealed: encrypt under the session content key.
	did := l.client.DID()
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		l.logger.Printf("nonce: %v", err)
		return
	}
	ct, err := escrow.Encrypt(s.contentKey, nonce, []byte(note.Text), escrow.AAD(s.gameURI, ply, did))
	if err != nil {
		l.logger.Printf("encrypt: %v", err)
		return
	}
	rec := &playsbot.GameCommentary{
		Game:       genStrongRef(s.gameRef),
		Ply:        gtOptionInt64(ply),
		Visibility: string(note.Visibility),
		Ciphertext: ct,
		Nonce:      nonce[:],
		KeyId:      gtOptionString(s.keyID),
		CreatedAt:  createdAt,
	}
	if note.Visibility == VisibilityDelayed {
		// Escrow-wrap to the AppView's current key (§8.1/§8.2).
		if s.escrowKey == nil {
			doc, err := l.client.EscrowKeys(ctx)
			if err != nil {
				l.logger.Printf("escrow keys: %v", err)
				return
			}
			var current *EscrowKeyDoc
			for i := range doc.Keys {
				if doc.Keys[i].RotationID == doc.Current {
					current = &doc.Keys[i]
					break
				}
			}
			if current == nil {
				l.logger.Printf("no current escrow key")
				return
			}
			pub, err := base64.RawURLEncoding.DecodeString(current.PublicKey)
			if err != nil || len(pub) != escrow.PublicKeySize {
				l.logger.Printf("bad escrow public key (err=%v len=%d)", err, len(pub))
				return
			}
			var escrowPub [32]byte
			copy(escrowPub[:], pub)
			eph, err := ecdhGenerateKey()
			if err != nil {
				l.logger.Printf("ephemeral key: %v", err)
				return
			}
			wrapped, err := escrow.WrapContentKey(escrowPub, eph, s.contentKey)
			if err != nil {
				l.logger.Printf("wrap: %v", err)
				return
			}
			s.escrowKey = &EscrowKeyRef{
				RotationID:         current.RotationID,
				EphemeralPublicKey: wrapped.EphemeralPublicKey[:],
				WrappedKey:         wrapped.WrappedKey,
			}
		}
		rec.EscrowKey = gtSomeEscrowKey(s.escrowKey)
	}
	l.writeAndPostCommentary(ctx, s, rec, note)
}

// writeAndPostCommentary: for non-public records, first call postCommentary
// so the AppView validates the escrow wrap and returns a receiptToken to
// embed (spec §5.7), then write the record to the agent's repo.
func (l *BotLoop) writeAndPostCommentary(ctx context.Context, s *playSession, rec *playsbot.GameCommentary, note *Note) {
	var receipt string
	if rec.Visibility != string(VisibilityPublic) {
		in := PostCommentaryInput{
			Game:       StrongRef{URI: rec.Game.URI, CID: rec.Game.CID},
			Visibility: rec.Visibility,
			CreatedAt:  rec.CreatedAt,
		}
		if rec.Ply.HasVal() {
			ply := rec.Ply.Val()
			in.Ply = &ply
		}
		if rec.Text.HasVal() {
			in.Text = rec.Text.Val()
		}
		in.Ciphertext = rec.Ciphertext
		in.Nonce = rec.Nonce
		if rec.KeyId.HasVal() {
			in.KeyID = rec.KeyId.Val()
		}
		if rec.EscrowKey.HasVal() {
			ek := rec.EscrowKey.Val()
			in.EscrowKey = &EscrowKeyRef{
				RotationID:         ek.RotationId,
				EphemeralPublicKey: ek.EphemeralPublicKey,
				WrappedKey:         ek.WrappedKey,
			}
		}
		out, err := l.client.PostCommentary(ctx, in)
		if err != nil {
			l.logger.Printf("commentary failed (postCommentary): %v", err)
			return
		}
		if !out.Ok {
			l.logger.Printf("commentary rejected by postCommentary")
			return
		}
		if out.ReceiptToken.HasVal() {
			receipt = out.ReceiptToken.Val()
		}
	}
	if receipt != "" {
		rec.ReceiptToken = gtOptionString(receipt)
	}
	if _, _, err := l.client.CreateCommentaryRecord(ctx, rec); err != nil {
		l.logger.Printf("commentary failed (record): %v", err)
	}
}

// finishSession publishes content keys at game end: a public commentary
// record per key (§4.7), then closes the game subscription.
func (l *BotLoop) finishSession(ctx context.Context, s *playSession) {
	s.mu.Lock()
	if s.published {
		s.mu.Unlock()
		return
	}
	s.published = true
	s.running = false
	s.mu.Unlock()

	rec := &playsbot.GameCommentary{
		Game:       genStrongRef(s.gameRef),
		Visibility: string(VisibilityPublic),
		KeyId:      gtOptionString(s.keyID),
		Text:       gtOptionString(base64.StdEncoding.EncodeToString(s.contentKey[:])),
		CreatedAt:  time.Now().UTC().Format(RFC3339MilliLike),
	}
	if _, _, err := l.client.CreateCommentaryRecord(ctx, rec); err != nil {
		l.logger.Printf("key publication failed: %v", err)
		return
	}
	l.logger.Printf("content key published %s", s.keyID)

	l.mu.Lock()
	delete(l.sessions, s.gameURI)
	l.mu.Unlock()
}

// sleepCtxDuration is used by tests to bound waits.
func sleepCtxDuration(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
