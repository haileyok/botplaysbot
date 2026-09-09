// Package gen_test pins the lexicon catalog to the NSIDs required by
// docs/spec-v0.1.md §4.1 and verifies the generated code registry covers every
// record collection. It lives outside internal/gen/playsbot because that
// directory must contain only generated files (see scripts/check-codegen.sh).
package gen_test

import (
	"strings"
	"testing"

	"github.com/jcalabro/atmos/lexicon"
	playsbot "github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// specSection41 lists the 24 lexicon NSIDs in the spec §4.1 namespace map.
var specSection41 = []string{
	// Records and objects.
	"bot.plays.bot.actor.profile",
	"bot.plays.bot.game",
	"bot.plays.bot.game.move",
	"bot.plays.bot.game.commentary",
	"bot.plays.bot.game.reveal",
	"bot.plays.bot.game.challenge",
	"bot.plays.bot.rating",
	"bot.plays.bot.flag",
	"bot.plays.bot.chess.move",
	"bot.plays.bot.chess.position",
	"bot.plays.bot.checkers.move",
	"bot.plays.bot.checkers.position",
	// XRPC endpoints.
	"bot.plays.bot.game.submitMove",
	"bot.plays.bot.game.getState",
	"bot.plays.bot.game.listGames",
	"bot.plays.bot.game.subscribe",
	"bot.plays.bot.game.acceptChallenge",
	"bot.plays.bot.match.seek",
	"bot.plays.bot.match.cancelSeek",
	"bot.plays.bot.match.subscribe",
	"bot.plays.bot.game.resign",
	"bot.plays.bot.game.offerDraw",
	"bot.plays.bot.actor.getProfile",
	"bot.plays.bot.actor.getLeaderboard",
}

// apiSurfaceExtras are the 8 additional endpoints the API surface needs
// (spec §5.5–5.7, §5.5a, and §9a.5).
var apiSurfaceExtras = []string{
	"bot.plays.bot.game.createChallenge",
	"bot.plays.bot.game.declineChallenge",
	"bot.plays.bot.game.cancelChallenge",
	"bot.plays.bot.game.listChallenges",
	"bot.plays.bot.match.getSeeks",
	"bot.plays.bot.game.acceptDraw",
	"bot.plays.bot.game.declineDraw",
	"bot.plays.bot.game.postCommentary",
}

// repoRecords are the ATProto record collections the generated DecodeRecord
// registry must decode.
var repoRecords = []string{
	"bot.plays.bot.actor.profile",
	"bot.plays.bot.game",
	"bot.plays.bot.game.move",
	"bot.plays.bot.game.commentary",
	"bot.plays.bot.game.reveal",
	"bot.plays.bot.game.challenge",
	"bot.plays.bot.rating",
	"bot.plays.bot.flag",
}

func TestLexiconCatalogCoversAPISurface(t *testing.T) {
	schemas, err := lexicon.ParseDir("../../lexicons")
	if err != nil {
		t.Fatalf("parse lexicons: %v", err)
	}
	cat := lexicon.NewCatalog()
	if err := cat.AddAll(schemas); err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	if err := cat.Resolve(); err != nil {
		t.Fatalf("resolve refs: %v", err)
	}

	all := append(append([]string{}, specSection41...), apiSurfaceExtras...)
	if len(all) != 32 {
		t.Fatalf("expected 32 surface NSIDs, got %d", len(all))
	}
	for _, nsid := range all {
		if cat.Schema(nsid) == nil {
			t.Errorf("lexicons/ is missing %s", nsid)
		}
	}

	// The vendored com.atproto strongRef must also resolve, since several
	// record lexicons reference it.
	if cat.Schema("com.atproto.repo.strongRef") == nil {
		t.Error("lexicons/ is missing com.atproto.repo.strongRef")
	}
}

func TestDecodeRecordRegistryCoversRecords(t *testing.T) {
	// An empty DAG-CBOR map decodes to the zero record; what matters here is
	// that each collection is recognized rather than rejected as unknown.
	for _, nsid := range repoRecords {
		v, err := playsbot.DecodeRecord(nsid, []byte{0xa0})
		if err != nil {
			t.Errorf("DecodeRecord(%q): %v", nsid, err)
			continue
		}
		if v == nil {
			t.Errorf("DecodeRecord(%q): nil value", nsid)
		}
	}

	// A collection with no lexicon must be rejected.
	if _, err := playsbot.DecodeRecord("com.example.unknown", []byte{0xa0}); err == nil ||
		!strings.Contains(err.Error(), "unknown collection") {
		t.Errorf("expected unknown-collection error, got %v", err)
	}
}
