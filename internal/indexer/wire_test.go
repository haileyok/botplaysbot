package indexer

import (
	"bytes"
	"testing"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot/comatproto"
)

// TestDecodeRecordEquivalence asserts the two transport normalizations
// produce identical canonical JSON: decodeRecordJSON (Jetstream) and
// decodeRecordCBOR (firehose) of the same logical record.
func TestDecodeRecordEquivalence(t *testing.T) {
	move := &playsbot.GameMove{
		Game: comatproto.RepoStrongRef{
			URI: "at://did:plc:a/bot.plays.bot.game/3k2",
			CID: "bafkreicysgb3pfdsmmyhpyed6frzrqfyt2qusuallynotvalidated",
		},
		Ply:        7,
		ReceivedAt: "2026-09-08T14:03:22.115Z",
		MoveToken:  "eyJhbGciOiJFZERTQSJ9.x.y",
		Payload: playsbot.GameMove_Payload{
			ChessMove: gt.SomeRef(playsbot.ChessMove{From: "e2", To: "e4"}),
		},
		ClockRemainingMs: gt.Some(int64(291_000)),
	}
	move.Payload.ChessMove.Val().LexiconTypeID = "bot.plays.bot.chess.move"
	move.Game.LexiconTypeID = "com.atproto.repo.strongRef"

	profile := &playsbot.ActorProfile{
		Revision:     3,
		Operator:     gt.Some("owner.example.dev"),
		Capabilities: []string{"bot.plays.bot.chess.move"},
		Model: gt.Some(playsbot.ActorProfile_Component{
			Name:     "gpt-x",
			Provider: gt.Some("openai"),
		}),
	}

	challenge := &playsbot.GameChallenge{
		GameType:  "bot.plays.bot.chess.move",
		Opponent:  gt.Some("did:plc:b"),
		CreatedAt: "2026-09-08T14:00:00.000Z",
		TimeControl: playsbot.BotGame_TimeControl{
			Kind:           "perMove",
			PerMoveSeconds: gt.Some(int64(300)),
		},
		Rated: gt.Some(true),
	}

	cases := []struct {
		name       string
		collection string
		rec        interface {
			MarshalJSON() ([]byte, error)
			MarshalCBOR() ([]byte, error)
		}
	}{
		{"move", playsbot.NSIDGameMove, move},
		{"profile", playsbot.NSIDActorProfile, profile},
		{"challenge", playsbot.NSIDGameChallenge, challenge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonRaw, err := tc.rec.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			cborRaw, err := tc.rec.MarshalCBOR()
			if err != nil {
				t.Fatal(err)
			}

			fromJSON, err := decodeRecordJSON(tc.collection, jsonRaw)
			if err != nil {
				t.Fatalf("decodeRecordJSON: %v", err)
			}
			fromCBOR, err := decodeRecordCBOR(tc.collection, cborRaw)
			if err != nil {
				t.Fatalf("decodeRecordCBOR: %v", err)
			}
			if !bytes.Equal(fromJSON, fromCBOR) {
				t.Fatalf("canonical JSON differs:\n json:  %s\n cbor:  %s", fromJSON, fromCBOR)
			}
		})
	}
}

// TestRepoEventEqual covers the comparator used by the transport contract.
func TestRepoEventEqual(t *testing.T) {
	base := RepoEvent{
		Kind: KindCreate, DID: "did:plc:a",
		Collection: playsbot.NSIDGameMove, Rkey: "3zz",
		CID: "bafy", Record: []byte(`{"a":1}`), CommitRev: "rev1",
	}
	same := base
	if !base.Equal(same) {
		t.Fatal("identical events compared unequal")
	}
	if base.Equal(RepoEvent{}) {
		t.Fatal("empty event compared equal to filled one")
	}

	diffs := []func(*RepoEvent){
		func(e *RepoEvent) { e.Kind = KindUpdate },
		func(e *RepoEvent) { e.DID = "did:plc:b" },
		func(e *RepoEvent) { e.Collection = playsbot.NSIDActorProfile },
		func(e *RepoEvent) { e.Rkey = "3zy" },
		func(e *RepoEvent) { e.CID = "other" },
		func(e *RepoEvent) { e.CommitRev = "rev2" },
		func(e *RepoEvent) { e.Record = []byte(`{"a":2}`) },
	}
	for i, mutate := range diffs {
		mutated := base
		mutate(&mutated)
		if base.Equal(mutated) {
			t.Fatalf("mutation %d not detected", i)
		}
	}
}
