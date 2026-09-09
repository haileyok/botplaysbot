package indexer

import (
	"testing"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// ---------------------------------------------------------------------------
// §5.8 v1 custom-domain rule

func TestIsCustomDomainHandle(t *testing.T) {
	cases := []struct {
		handle string
		want   bool
	}{
		{"chessbot.example.dev", true},
		{"deep.chessbot.example.co.uk", true},
		{"Example.COM.", true}, // trailing dot + case folded
		{"", false},
		{"nodot", false},
		{"handle.invalid", false},
		{"alice.bsky.social", false},
		{"bot.team.bsky.social", false},
		{"bsky.team", false},
		{"alice.bsky.team", false},
	}
	for _, tc := range cases {
		if got := IsCustomDomainHandle(tc.handle); got != tc.want {
			t.Errorf("IsCustomDomainHandle(%q) = %v, want %v", tc.handle, got, tc.want)
		}
	}
}

func TestOperatorFor(t *testing.T) {
	resolve := func(did string) (string, bool) {
		if did == "did:plc:op" {
			return "op.example.dev", true
		}
		return "", false
	}

	// Absent operator: unverified, no DID.
	st := operatorFor("", resolve)
	if st.OperatorDID != nil || st.OperatorVerified {
		t.Fatalf("absent operator = %+v", st)
	}

	// Handle operator on a custom domain: verified.
	st = operatorFor("someone.example.dev", resolve)
	if st.OperatorDID != nil || !st.OperatorVerified {
		t.Fatalf("custom-domain handle operator = %+v", st)
	}

	// Handle operator on a shared provider: unverified.
	st = operatorFor("someone.bsky.social", resolve)
	if st.OperatorDID != nil || st.OperatorVerified {
		t.Fatalf("shared-provider handle operator = %+v", st)
	}

	// DID operator whose resolved handle is a custom domain: verified.
	st = operatorFor("did:plc:op", resolve)
	if st.OperatorDID == nil || *st.OperatorDID != "did:plc:op" || !st.OperatorVerified {
		t.Fatalf("DID operator with custom-domain handle = %+v", st)
	}

	// DID operator we cannot resolve: conservatively unverified.
	st = operatorFor("did:plc:unknown", resolve)
	if st.OperatorDID == nil || st.OperatorVerified {
		t.Fatalf("unresolvable DID operator = %+v", st)
	}
}

// ---------------------------------------------------------------------------
// profile_hash (§4.3)

func TestProfileHashStableAcrossTypeMarkers(t *testing.T) {
	comp := func(name, provider string, withType bool) gt.Option[playsbot.ActorProfile_Component] {
		c := playsbot.ActorProfile_Component{Name: name, Provider: gt.Some(provider)}
		if withType {
			c.LexiconTypeID = "bot.plays.bot.actor.profile#component"
		}
		return gt.Some(c)
	}

	h1, err := profileHash(comp("gpt-x", "openai", true), comp("claude-harness", "anthropic", false))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := profileHash(comp("gpt-x", "openai", false), comp("claude-harness", "anthropic", true))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash differs by $type markers: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", h1)
	}

	// Different components hash differently.
	h3, err := profileHash(comp("gpt-4o", "openai", true), comp("claude-harness", "anthropic", true))
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("distinct components produced identical hashes")
	}

	// No model/harness: a stable, defined hash.
	h4, err := profileHash(gt.None[playsbot.ActorProfile_Component](), gt.None[playsbot.ActorProfile_Component]())
	if err != nil {
		t.Fatal(err)
	}
	if h4 == "" {
		t.Fatal("empty components produced empty hash")
	}
}
