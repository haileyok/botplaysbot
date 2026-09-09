// Phase E integration tests: the repo-event indexer against the real
// firehose of the dev PDS harness (isolated DB per test, per the package
// conventions in integration_test.go).
//
// Covered here (E.5):
//  1. accepted moves written as repo records are linked (verified=true)
//  2. mismatched moveToken fields → verified=false + unverifiedMoveRecord
//     flag row + flag record in the service repo, replay-idempotently
//  3. never-arriving record → missingMoveRecord flag row + record
//  4. foreign (non-service-DID) bot.plays.bot.game record → ignored+counted
//  5. repo-backed challenge → listChallenges/accept produce a game record
//     carrying the challenge strongRef
//  6. profile ingest populates actors + activates the capability gate
package appview_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jcalabro/atmos/api/comatproto"
	"github.com/jcalabro/atmos/xrpc"

	"github.com/haileyok/botplaysbot/internal/clock"
	"github.com/haileyok/botplaysbot/internal/config"
	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/match"
	"github.com/haileyok/botplaysbot/internal/testutil"
	"github.com/haileyok/botplaysbot/internal/tid"
)

// bootIndexerEnv is bootGameEnv with a config tweak hook (missing-record
// window etc.).
func bootIndexerEnv(t *testing.T, tweak func(*config.Config)) (*gameEnv, testutil.Account, testutil.Account) {
	t.Helper()
	databaseURL := testutil.IsolatedDBURL(t)
	harness := testutil.StartPDS(t)

	svc := harness.CreateAccount(t)
	cfg := testConfig(t, databaseURL, t.TempDir(), harness.PDS, harness.PLC, svc.DID)
	cfg.ServiceAppPassword = svc.AppPassword
	cfg.Tunables.SweeperInterval = 100 * time.Millisecond
	if tweak != nil {
		tweak(cfg)
	}

	env := bootApp(t, cfg)
	white := harness.CreateAccount(t)
	black := harness.CreateAccount(t)
	return &gameEnv{testEnv: env, svc: svc, harness: harness}, white, black
}

// writeUntilIndexed invokes write, re-invoking periodically, until indexed
// reports true. Re-writing is safe (create falls back to putRecord) and
// covers the boot race where the appview's firehose connection is still
// establishing when the first record lands.
func writeUntilIndexed(t *testing.T, what string, write func() error, indexed func() bool) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for {
		if err := write(); err != nil {
			t.Fatalf("write %s: %v", what, err)
		}
		if indexed() {
			return
		}
		time.Sleep(300 * time.Millisecond)
		if time.Now().After(deadline) {
			t.Fatalf("indexer never indexed %s", what)
		}
	}
}

// acceptance is the part of a submitMove response the record writer needs.
type acceptance struct {
	ReceivedAt string          `json:"receivedAt"`
	MoveToken  string          `json:"moveToken"`
	Payload    json.RawMessage `json:"-"`
}

// chessGameWithMoves creates a chess game and submits `plies` alternating
// moves (e2e4, e7e5, ...), returning the game URI and each acceptance.
func chessGameWithMoves(t *testing.T, env *gameEnv, white, black testutil.Account, perMoveSeconds int64, want int) (string, map[int64]acceptance) {
	t.Helper()
	gameURI := newChessGame(t, env, white, black, perMoveSeconds)
	moves := map[int64][2]string{
		1: {"e2", "e4"}, 2: {"e7", "e5"}, 3: {"g1", "f3"}, 4: {"b8", "c6"},
	}
	acc := map[int64]acceptance{}
	seat := map[string]testutil.Account{"white": white, "black": black}
	for ply := 1; ply <= want; ply++ {
		player := seat[map[bool]string{true: "white", false: "black"}[ply%2 == 1]]
		mv := moves[int64(ply)]
		status, body := env.submitMove(t, accessJWT(t, env, player), gameURI, int64(ply), mv[0], mv[1], "")
		if status != 200 {
			t.Fatalf("submitMove ply %d = %d %s", ply, status, body)
		}
		var out struct {
			ReceivedAt string `json:"receivedAt"`
			MoveToken  string `json:"moveToken"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode submitMove ply %d: %v", ply, err)
		}
		acc[int64(ply)] = acceptance{ReceivedAt: out.ReceivedAt, MoveToken: out.MoveToken}
	}
	return gameURI, acc
}

func moveRecord(env *gameEnv, gameURI, gameCID string, ply int64, acc acceptance, tamper bool) map[string]any {
	payload := map[string]any{"$type": engine.NSIDChessMove, "from": "e2", "to": "e4"}
	if ply == 2 {
		payload = map[string]any{"$type": engine.NSIDChessMove, "from": "e7", "to": "e5"}
	}
	if tamper {
		// Keep the token intact, mutate the payload: the record no longer
		// matches what the AppView accepted (spec §4.5).
		payload["to"] = "e6"
	}
	rec := map[string]any{
		"$type":       playsbot.NSIDGameMove,
		"game":        map[string]any{"$type": "com.atproto.repo.strongRef", "uri": gameURI, "cid": gameCID},
		"ply":         ply,
		"payload":     payload,
		"receivedAt":  acc.ReceivedAt,
		"moveToken":   acc.MoveToken,
		"createdAt_x": nil,
	}
	delete(rec, "createdAt_x")
	return rec
}

func gameCID(t *testing.T, env *gameEnv, gameURI string) string {
	t.Helper()
	// The game record CID becomes available when the indexer observes the
	// service repo record on the firehose (games.cid backfill) — the same
	// wait a real agent performs by resolving the record.
	deadline := time.Now().Add(20 * time.Second)
	for {
		g, err := env.app.Repos().Games.Get(context.Background(), gameURI)
		if err == nil && g.CID != nil {
			return *g.CID
		}
		if time.Now().After(deadline) {
			t.Fatalf("game record CID never appeared for %s", gameURI)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------

// 1. Happy path: accepted moves appear as repo records → linked rows.
func TestIndexerLinksAcceptedMoveRecords(t *testing.T) {
	env, white, black := bootIndexerEnv(t, nil)
	ctx := context.Background()

	gameURI, acc := chessGameWithMoves(t, env, white, black, 300, 2)
	cid := gameCID(t, env, gameURI)

	movesByPly := map[int64]testutil.Account{1: white, 2: black}
	for ply := int64(1); ply <= 2; ply++ {
		player := movesByPly[ply]
		rkey := tid.Next().String()
		writeUntilIndexed(t,
			fmt.Sprintf("ply %d record", ply),
			func() error {
				_, _, err := testutil.WriteRecord(ctx, env.harness.Client(t, player), player.DID,
					playsbot.NSIDGameMove, rkey, moveRecord(env, gameURI, cid, ply, acc[ply], false))
				return err
			},
			func() bool {
				mv, err := env.app.Repos().Moves.Get(ctx, gameURI, int(ply))
				return err == nil && mv.RepoURI != nil && mv.RepoCID != nil && mv.Verified
			})
		mv, err := env.app.Repos().Moves.Get(ctx, gameURI, int(ply))
		if err != nil {
			t.Fatal(err)
		}
		wantURI := fmt.Sprintf("at://%s/%s/%s", player.DID, playsbot.NSIDGameMove, rkey)
		if *mv.RepoURI != wantURI {
			t.Fatalf("ply %d repo_uri = %s, want %s", ply, *mv.RepoURI, wantURI)
		}
		if !mv.Verified {
			t.Fatalf("ply %d not verified", ply)
		}
	}

	stats := env.app.Indexer().StatsSnapshot()
	if stats.MovesLinked < 2 {
		t.Fatalf("movesLinked = %d, want >= 2", stats.MovesLinked)
	}
}

// 2. Tampered record: verified=false + unverifiedMoveRecord flag row +
// flag record in the service repo; replaying the event stays idempotent.
func TestIndexerFlagsUnverifiedMoveRecord(t *testing.T) {
	env, white, black := bootIndexerEnv(t, nil)
	ctx := context.Background()

	gameURI, acc := chessGameWithMoves(t, env, white, black, 300, 1)
	cid := gameCID(t, env, gameURI)
	rkey := tid.Next().String()
	client := env.harness.Client(t, white)

	writeUntilIndexed(t, "tampered move record",
		func() error {
			_, _, err := testutil.WriteRecord(ctx, client, white.DID,
				playsbot.NSIDGameMove, rkey, moveRecord(env, gameURI, cid, 1, acc[1], true))
			return err
		},
		func() bool {
			mv, err := env.app.Repos().Moves.Get(ctx, gameURI, 1)
			return err == nil && !mv.Verified && mv.RepoURI != nil
		})

	// Replay the same record (update event) — no second flag.
	if _, _, err := testutil.WriteRecord(ctx, client, white.DID,
		playsbot.NSIDGameMove, rkey, moveRecord(env, gameURI, cid, 1, acc[1], true)); err != nil {
		t.Fatalf("replay write: %v", err)
	}
	time.Sleep(2 * time.Second)

	flags, err := env.app.Repos().Flags.ListBySubject(ctx, white.DID, 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range flags {
		if f.Kind == "unverifiedMoveRecord" {
			n++
			if f.Severity != "warning" {
				t.Fatalf("unverifiedMoveRecord severity = %s, want warning", f.Severity)
			}
			if f.RepoURI == nil {
				t.Fatal("unverifiedMoveRecord row has no repo_uri (record not backfilled)")
			}
		}
	}
	if n != 1 {
		t.Fatalf("unverifiedMoveRecord rows for %s = %d, want exactly 1 (idempotent replay)", white.DID, n)
	}

	// The flag record is visible in the service repo.
	recorded := serviceFlagKinds(t, env)
	if recorded["unverifiedMoveRecord"] == 0 {
		t.Fatalf("service repo flag records = %v, want unverifiedMoveRecord present", recorded)
	}
}

// 3. Never-arriving record: shortened window → missingMoveRecord.
func TestIndexerFlagsMissingMoveRecord(t *testing.T) {
	env, white, black := bootIndexerEnv(t, func(cfg *config.Config) {
		cfg.Tunables.MissingRecordWindow = 1200 * time.Millisecond
	})
	ctx := context.Background()

	gameURI, _ := chessGameWithMoves(t, env, white, black, 300, 1)

	// Wait out the window, then sweep (the background ticker may have
	// swept already; InsertOnce makes both paths converge).
	time.Sleep(1600 * time.Millisecond)
	if _, err := env.app.Indexer().SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	writeUntilIndexed(t, "missingMoveRecord flag",
		func() error { return nil },
		func() bool {
			flags, err := env.app.Repos().Flags.ListBySubject(ctx, white.DID, 50)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range flags {
				if f.Kind == "missingMoveRecord" && f.GameURI != nil && *f.GameURI == gameURI {
					if f.Severity != "info" {
						t.Fatalf("missingMoveRecord severity = %s, want info", f.Severity)
					}
					return true
				}
			}
			return false
		})

	if recorded := serviceFlagKinds(t, env); recorded["missingMoveRecord"] == 0 {
		t.Fatalf("service repo flag records = %v, want missingMoveRecord present", recorded)
	}

	// A second sweep must not duplicate the assertion.
	if _, err := env.app.Indexer().SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	flags, _ := env.app.Repos().Flags.ListBySubject(ctx, white.DID, 50)
	n := 0
	for _, f := range flags {
		if f.Kind == "missingMoveRecord" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("missingMoveRecord rows = %d, want exactly 1", n)
	}
}

// 4. A bot.plays.bot.game record from an agent repo is ignored (§4.2).
func TestIndexerIgnoresForeignGameRecord(t *testing.T) {
	env, white, _ := bootIndexerEnv(t, nil)
	ctx := context.Background()

	before := countGames(t, env)

	writeUntilIndexed(t, "foreign game record",
		func() error {
			_, _, err := testutil.WriteRecord(ctx, env.harness.Client(t, white), white.DID,
				playsbot.NSIDBotGame, tid.Next().String(), map[string]any{
					"$type":        playsbot.NSIDBotGame,
					"gameType":     engine.NSIDChessMove,
					"players":      []map[string]any{{"did": white.DID, "seat": "white"}},
					"timeControl":  map[string]any{"kind": "perMove", "perMoveSeconds": 300},
					"status":       "active",
					"createdAt":    "2026-09-08T14:00:00.000Z",
				})
			return err
		},
		func() bool {
			return env.app.Indexer().StatsSnapshot().IgnoredForeign >= 1
		})

	time.Sleep(time.Second) // any (wrong) ingest would have landed by now
	if after := countGames(t, env); after != before {
		t.Fatalf("games rows changed after foreign game record: %d → %d", before, after)
	}
}

// 5. Repo-backed challenge: indexed record → accept → game record carries
// the challenge strongRef.
func TestIndexerRepoBackedChallengeCarriesStrongRef(t *testing.T) {
	env, white, black := bootIndexerEnv(t, nil)
	ctx := context.Background()

	rkey := tid.Next().String()
	challenge := map[string]any{
		"$type":         playsbot.NSIDGameChallenge,
		"opponent":      black.DID,
		"gameType":      engine.NSIDChessMove,
		"variant":       "standard",
		"timeControl":   map[string]any{"kind": "perMove", "perMoveSeconds": 300},
		"seatPreference": "first",
		"rated":         true,
		"createdAt":     "2026-09-08T14:00:00.000Z",
	}
	var recordCID string
	writeUntilIndexed(t, "challenge record",
		func() error {
			var err error
			_, recordCID, err = testutil.WriteRecord(ctx, env.harness.Client(t, white), white.DID,
				playsbot.NSIDGameChallenge, rkey, challenge)
			return err
		},
		func() bool {
			c, err := env.app.Repos().Challenges.Get(ctx, rkey)
			return err == nil && c.RepoURI != nil && c.RepoCID != nil
		})

	c, err := env.app.Repos().Challenges.Get(ctx, rkey)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "pending" || c.ChallengerDID != white.DID {
		t.Fatalf("indexed challenge = {status %s challenger %s}, want pending/%s", c.Status, c.ChallengerDID, white.DID)
	}

	out, err := env.app.Matcher().AcceptChallenge(ctx, black.DID, rkey)
	if err != nil {
		t.Fatalf("accept repo-backed challenge: %v", err)
	}

	g, err := env.app.Repos().Games.Get(ctx, out.Created.URI)
	if err != nil {
		t.Fatal(err)
	}
	if g.ChallengeURI == nil || *g.ChallengeURI != fmt.Sprintf("at://%s/%s/%s", white.DID, playsbot.NSIDGameChallenge, rkey) {
		t.Fatalf("game challenge_uri = %v", g.ChallengeURI)
	}
	if g.ChallengeCID == nil || *g.ChallengeCID != recordCID {
		t.Fatalf("game challenge_cid = %v, want %s", g.ChallengeCID, recordCID)
	}

	// The published game record carries the strongRef.
	found := false
	for _, rec := range env.gameRecords(t) {
		if rec.URI != out.Created.URI {
			continue
		}
		var body struct {
			Challenge *struct {
				URI string `json:"uri"`
				CID string `json:"cid"`
			} `json:"challenge"`
		}
		if err := json.Unmarshal(rec.Value, &body); err != nil {
			t.Fatalf("decode game record: %v", err)
		}
		if body.Challenge == nil || body.Challenge.URI != *g.ChallengeURI || body.Challenge.CID != recordCID {
			t.Fatalf("game record challenge strongRef = %+v, want {%s %s}", body.Challenge, *g.ChallengeURI, recordCID)
		}
		found = true
	}
	if !found {
		t.Fatal("game record not found in the service repo")
	}
}

// 6. Profile ingest populates actors (hash, operator verification) and
// activates Phase D's capability-eligibility gate.
func TestIndexerProfileGatesCapability(t *testing.T) {
	env, white, black := bootIndexerEnv(t, nil)
	ctx := context.Background()

	writeUntilIndexed(t, "profile record",
		func() error {
			_, _, err := testutil.WriteRecord(ctx, env.harness.Client(t, white), white.DID,
				playsbot.NSIDActorProfile, "self", map[string]any{
					"$type":        playsbot.NSIDActorProfile,
					"revision":     2,
					"createdAt":    "2026-09-08T14:00:00.000Z",
					"operator":     "owner.example.dev",
					"capabilities": []string{"bot.plays.bot.checkers.move"},
					"model":        map[string]any{"name": "chess-net", "provider": "example"},
					"harness":      map[string]any{"name": "plays-sdk", "version": "1.0"},
				})
			return err
		},
		func() bool {
			_, err := env.app.Repos().Actors.Get(ctx, white.DID)
			return err == nil
		})

	actor, err := env.app.Repos().Actors.Get(ctx, white.DID)
	if err != nil {
		t.Fatal(err)
	}
	if actor.Revision == nil || *actor.Revision != 2 {
		t.Fatalf("revision = %+v, want 2", actor.Revision)
	}
	if actor.ProfileHash == nil || len(*actor.ProfileHash) != 64 {
		t.Fatalf("profile_hash = %v", actor.ProfileHash)
	}
	if !actor.OperatorVerified {
		t.Fatal("custom-domain operator handle not marked verified")
	}
	var prof struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(actor.Profile, &prof); err != nil {
		t.Fatalf("stored profile jsonb: %v", err)
	}
	if len(prof.Capabilities) != 1 || prof.Capabilities[0] != "bot.plays.bot.checkers.move" {
		t.Fatalf("stored capabilities = %v", prof.Capabilities)
	}

	// The gate: white (checkers only) cannot accept a chess challenge.
	ch, err := env.app.Matcher().CreateChallenge(ctx, black.DID, match.ChallengeParams{
		Opponent:    white.DID,
		GameType:    engine.NSIDChessMove,
		TimeControl: perMove(300),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
		Rated:       true,
	})
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}
	_, err = env.app.Matcher().AcceptChallenge(ctx, white.DID, ch.ID)
	if err == nil {
		t.Fatal("accept succeeded despite capabilities excluding chess")
	}
	me, ok := asMatchError(err)
	if !ok || me.Code != "Forbidden" {
		t.Fatalf("accept error = %v, want Forbidden", err)
	}
}

// ---------------------------------------------------------------------------
// helpers

func serviceFlagKinds(t *testing.T, env *gameEnv) map[string]int {
	t.Helper()
	client := &xrpc.Client{Host: env.cfg.PDSURL}
	out, err := comatproto.RepoListRecords(context.Background(), client, playsbot.NSIDBotFlag, "", 50, env.svc.DID, false)
	if err != nil {
		t.Fatalf("listRecords(bot.plays.bot.flag): %v", err)
	}
	kinds := map[string]int{}
	for _, rec := range out.Records {
		var f struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(rec.Value, &f); err == nil {
			kinds[f.Kind]++
		}
	}
	return kinds
}

func countGames(t *testing.T, env *gameEnv) int {
	t.Helper()
	var n int
	if err := env.app.Repos().QueryRow(context.Background(), `SELECT count(*) FROM games`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func perMove(seconds int64) clock.TimeControl {
	return clock.TimeControl{Kind: clock.KindPerMove, PerMoveSeconds: seconds}
}

func asMatchError(err error) (*match.Error, bool) {
	me, ok := err.(*match.Error)
	return me, ok
}
