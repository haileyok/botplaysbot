package indexer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jcalabro/atmos/api/comatproto"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/testutil"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// TestTransportContractFirehoseVsJetstream (E.4): record a REAL firehose
// fixture — an agent writes a profile and a move record through the harness
// PDS while a FirehoseSource listens — then hand-author the equivalent
// Jetstream #commit JSON frames and feed them through a JetstreamSource's
// injected frame path. Both sources must produce identical RepoEvent
// structs for create, update, and delete.
func TestTransportContractFirehoseVsJetstream(t *testing.T) {
	harness := testutil.StartPDS(t)
	account := harness.CreateAccount(t)
	client := harness.Client(t, account)
	did := account.DID

	ctx := context.Background()

	// --- fixture writes through the harness PDS.
	profile := map[string]any{
		"$type":        playsbot.NSIDActorProfile,
		"revision":     1,
		"createdAt":    "2026-09-08T14:00:00.000Z",
		"operator":     "owner.example.dev",
		"capabilities": []string{"bot.plays.bot.chess.move"},
		"model":        map[string]any{"name": "test-model", "provider": "test"},
	}
	if _, _, err := testutil.WriteRecord(ctx, client, did, playsbot.NSIDActorProfile, "self", profile); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	moveRkey := tid.Next().String()
	move := map[string]any{
		"$type": playsbot.NSIDGameMove,
		"game": map[string]any{
			"$type": "com.atproto.repo.strongRef",
			"uri":   "at://" + did + "/" + playsbot.NSIDBotGame + "/3kbogus",
			"cid":   "bafkreigamebogus",
		},
		"ply":         1,
		"payload":     map[string]any{"$type": "bot.plays.bot.chess.move", "from": "e2", "to": "e4"},
		"receivedAt":  "2026-09-08T14:03:22.115Z",
		"moveToken":   "eyJhbGciOiJFZERTQSJ9.fixturesarenotverified",
		"createdAt_x": nil, // removed below; keeps the map literal honest
	}
	delete(move, "createdAt_x")
	if _, _, err := testutil.WriteRecord(ctx, client, did, playsbot.NSIDGameMove, moveRkey, move); err != nil {
		t.Fatalf("write move: %v", err)
	}

	// --- firehose capture (real subscribeRepos against the harness PDS).
	connected := make(chan struct{}, 1)
	rec := newRecorderHandler()
	firehose := NewSource(SourceOptions{
		Kind:        KindFirehose,
		URL:         toWebSocketScheme(harness.PDS) + "/xrpc/com.atproto.sync.subscribeRepos",
		OnConnected: connected,
		Backoff:     fastBackoff(),
		Logger:      testSlog(),
	})
	fctx, fcancel := context.WithCancel(ctx)
	fdone := make(chan struct{})
	go func() { _ = firehose.Run(fctx, rec); close(fdone) }()
	t.Cleanup(func() {
		fcancel()
		select {
		case <-fdone:
		case <-time.After(10 * time.Second):
		}
	})
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("firehose source never connected to the harness PDS")
	}

	// The connection established before these writes: profile update and
	// move delete complete the create/update/delete matrix.
	profile["revision"] = 2
	profile["operator"] = "owner2.example.dev"
	profileRaw, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comatproto.RepoPutRecord(ctx, client, &comatproto.RepoPutRecord_Input{
		Repo:       did,
		Collection: playsbot.NSIDActorProfile,
		Rkey:       "self",
		Record:     profileRaw,
	}); err != nil {
		t.Fatalf("put profile: %v", err)
	}
	if err := testutil.DeleteRecord(ctx, client, did, playsbot.NSIDGameMove, moveRkey); err != nil {
		t.Fatalf("delete move: %v", err)
	}

	// Expected firehose order: profile create, move create, profile
	// update, move delete (commit order is preserved on the stream).
	firehoseEvents := rec.collect(t, 4, 20*time.Second)
	expect := []struct {
		kind Kind
		col  string
		rkey string
	}{
		{KindCreate, playsbot.NSIDActorProfile, "self"},
		{KindCreate, playsbot.NSIDGameMove, moveRkey},
		{KindUpdate, playsbot.NSIDActorProfile, "self"},
		{KindDelete, playsbot.NSIDGameMove, moveRkey},
	}
	for i, e := range expect {
		got := firehoseEvents[i]
		if got.Kind != e.kind || got.Collection != e.col || got.Rkey != e.rkey || got.DID != did {
			t.Fatalf("firehose event %d = {kind=%s col=%s rkey=%s did=%s}, want {%s %s %s %s}",
				i, got.Kind, got.Collection, got.Rkey, got.DID, e.kind, e.col, e.rkey, did)
		}
		if e.kind == KindDelete {
			if got.Record != nil || got.CID != "" {
				t.Fatalf("delete event %d carries record/cid: %+v", i, got)
			}
		} else if got.Record == nil || got.CID == "" {
			t.Fatalf("event %d missing record/cid: %+v", i, got)
		}
	}

	// --- hand-author the equivalent Jetstream frames from the captured
	// fixture (same record content, real cids/revs) per the Jetstream
	// event schema.
	frames := make([][]byte, 0, len(firehoseEvents))
	for i, ev := range firehoseEvents {
		frames = append(frames, jetstreamFrame(t, ev, int64(7000+i)))
	}

	jsRec := newRecorderHandler()
	conn := newFakeConn(frames...)
	jetstream := NewSource(SourceOptions{
		Kind:        KindJetstream,
		URL:         "wss://js.example/subscribe",
		Collections: BotCollections(),
		Dial:        dialTo(t, nil, conn),
		Backoff:     fastBackoff(),
		Logger:      testSlog(),
	})
	jctx, jcancel := context.WithCancel(ctx)
	jdone := make(chan struct{})
	go func() { _ = jetstream.Run(jctx, jsRec); close(jdone) }()
	t.Cleanup(func() {
		jcancel()
		select {
		case <-jdone:
		case <-time.After(10 * time.Second):
		}
	})
	jsEvents := jsRec.collect(t, len(frames), 15*time.Second)

	// --- the contract: both transports produce identical RepoEvents.
	for i := range firehoseEvents {
		if !firehoseEvents[i].Equal(jsEvents[i]) {
			t.Fatalf("transport divergence at event %d:\n firehose: %+v\n jetstream: %+v",
				i, firehoseEvents[i], jsEvents[i])
		}
	}
	t.Logf("contract verified over %d events (create/update/delete, profile+move)", len(firehoseEvents))
}
