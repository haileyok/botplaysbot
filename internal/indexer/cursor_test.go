package indexer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jcalabro/atmos/streaming"
	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/db"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/testutil"
)

// fastBackoff keeps reconnect loops quick in tests.
func fastBackoff() *streaming.BackoffPolicy {
	return &streaming.BackoffPolicy{
		InitialDelay: gt.Some(50 * time.Millisecond),
		MaxDelay:     gt.Some(500 * time.Millisecond),
		Jitter:       gt.Some(false),
	}
}

// fakeEvents builds fixture RepoEvents (content shape mirrors what the
// real normalizations produce; the sources' normalizations are covered by
// wire_test.go and the contract test). Records pass through
// decodeRecordJSON so their bytes match the canonical typed form the
// sources emit.
func fakeEvents() []RepoEvent {
	events := []RepoEvent{
		{
			Kind: KindCreate, DID: "did:plc:player1",
			Collection: playsbot.NSIDActorProfile, Rkey: "self",
			CID: "bafyprofilecreate", Record: []byte(`{"$type":"bot.plays.bot.actor.profile","revision":1,"createdAt":"2026-09-08T14:00:00.000Z"}`),
			CommitRev: "rev1001",
		},
		{
			Kind: KindUpdate, DID: "did:plc:player1",
			Collection: playsbot.NSIDActorProfile, Rkey: "self",
			CID: "bafyprofileupdate", Record: []byte(`{"$type":"bot.plays.bot.actor.profile","revision":2,"createdAt":"2026-09-08T14:00:00.000Z"}`),
			CommitRev: "rev1002",
		},
	}
	for i := range events {
		canonical, err := decodeRecordJSON(events[i].Collection, events[i].Record)
		if err != nil {
			panic(err)
		}
		events[i].Record = canonical
	}
	return events
}

// TestJetstreamCursorResumeNoReprocessing (E.5 #7, simulated restart):
// a source with cursor persistence processes a batch, is cancelled, and a
// fresh source resumes from the persisted cursor — the second run's dial
// URL carries the resume position and no event is reprocessed.
func TestJetstreamCursorResumeNoReprocessing(t *testing.T) {
	poolURL := testutil.IsolatedDBURL(t)
	pool, err := db.OpenPool(context.Background(), poolURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	store := NewDBCursorStore(pool, string(KindJetstream))

	events := fakeEvents()

	// --- first run: two events, then cancel mid-stream.
	var dialed []string
	rec1 := newRecorderHandler()
	conn1 := newFakeConn(
		jetstreamFrame(t, events[0], 1001),
		jetstreamFrame(t, events[1], 1002),
	)
	src1 := NewSource(SourceOptions{
		Kind:        KindJetstream,
		URL:         "wss://js.example/subscribe",
		Collections: BotCollections(),
		CursorStore: store,
		Dial:        dialTo(t, &dialed, conn1),
		Backoff:     fastBackoff(),
	})

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { _ = src1.Run(ctx1, rec1); close(done1) }()

	got1 := rec1.collect(t, 2, 15*time.Second)
	cancel1()
	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Fatal("first run did not stop")
	}
	for i, ev := range got1 {
		if !ev.Equal(events[i]) {
			t.Fatalf("first run event %d = %+v, want %+v", i, ev, events[i])
		}
	}

	// The cursor must have advanced to the last yielded event's position.
	cur, err := store.LoadCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur != 1002 {
		t.Fatalf("persisted cursor = %d, want 1002", cur)
	}

	// --- second run: resumes from the cursor; only newer events flow.
	rec2 := newRecorderHandler()
	after := RepoEvent{
		Kind: KindDelete, DID: events[0].DID,
		Collection: playsbot.NSIDActorProfile, Rkey: "self",
		CommitRev: "rev1003",
	}
	conn2 := newFakeConn(jetstreamFrame(t, after, 1003))
	src2 := NewSource(SourceOptions{
		Kind:        KindJetstream,
		URL:         "wss://js.example/subscribe",
		Collections: BotCollections(),
		CursorStore: store,
		Dial:        dialTo(t, &dialed, conn2),
		Backoff:     fastBackoff(),
	})

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = src2.Run(ctx2, rec2); close(done2) }()

	got2 := rec2.collect(t, 1, 15*time.Second)
	cancel2()
	select {
	case <-done2:
	case <-time.After(10 * time.Second):
		t.Fatal("second run did not stop")
	}

	// Resume position was requested upstream: cursor=1002 in the URL.
	if len(dialed) != 2 {
		t.Fatalf("dialed %d URLs, want 2: %v", len(dialed), dialed)
	}
	if !strings.Contains(dialed[1], "cursor=1002") {
		t.Fatalf("second dial URL missing resume cursor: %s", dialed[1])
	}
	// Exactly the newer event arrived — no replay of the first two.
	if !got2[0].Equal(after) {
		t.Fatalf("second run event = %+v, want %+v", got2[0], after)
	}
	// Drain any stragglers: nothing else may arrive.
	select {
	case ev := <-rec2.events:
		t.Fatalf("second run reprocessed an event: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestIngestReplayIdempotent drives the same event through the Ingestor
// twice (replay within one process) and asserts counters/rows do not
// double-count. Uses the isolated DB directly.
func TestIngestReplayIdempotent(t *testing.T) {
	poolURL := testutil.IsolatedDBURL(t)
	pool, err := db.OpenPool(context.Background(), poolURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	repos := repoNew(pool)

	cfg := testCfg()
	emitter := NewFlagEmitter(repos, nil, testSlog())
	ingest := NewIngestor(cfg, repos, testPubKey(), emitter, testSlog())

	// A profile event replayed twice: one actors row, counters stable.
	ev := RepoEvent{
		Kind: KindCreate, DID: "did:plc:playerX",
		Collection: playsbot.NSIDActorProfile, Rkey: "self",
		CID: "bafyself",
		Record: []byte(`{"$type":"bot.plays.bot.actor.profile","revision":1,` +
			`"createdAt":"2026-09-08T14:00:00.000Z","operator":"owner.example.dev",` +
			`"capabilities":["bot.plays.bot.chess.move"]}`),
		CommitRev: "revX",
	}
	ingest.Handle(ev)
	ingest.Handle(ev)

	actor, err := repos.Actors.Get(context.Background(), ev.DID)
	if err != nil {
		t.Fatalf("actors row missing after ingest: %v", err)
	}
	if actor.Revision == nil || *actor.Revision != 1 {
		t.Fatalf("actor revision = %+v", actor.Revision)
	}
	if actor.OperatorDID != nil {
		t.Fatalf("operator_did = %v, want nil for handle operator", *actor.OperatorDID)
	}
	if !actor.OperatorVerified {
		t.Fatal("custom-domain operator handle not verified")
	}

	// A foreign verdict record is ignored and counted.
	foreign := RepoEvent{
		Kind: KindCreate, DID: "did:plc:imposter",
		Collection: playsbot.NSIDBotGame, Rkey: "3zz",
		Record: []byte(`{"$type":"bot.plays.bot.game"}`), CommitRev: "revY",
	}
	ingest.Handle(foreign)
	ingest.Handle(foreign)
	stats := ingest.SnapshotStats()
	if stats.IgnoredForeign != 2 {
		t.Fatalf("ignoredForeign = %d, want 2", stats.IgnoredForeign)
	}
}
