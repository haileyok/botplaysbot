package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// sharedHandleSuffixes are v1's known free/shared handle providers
// (spec §5.8: maintain an allowlist/denylist; v1 rule = custom domains
// only). A handle under one of these (or its apex) is not operator-
// verified. Extend as providers appear; the rule is intentionally
// conservative for DIDs we cannot resolve (unresolvable → unverified).
var sharedHandleSuffixes = []string{"bsky.social", "bsky.team"}

// IsCustomDomainHandle reports whether handle looks like an operator's own
// domain per the v1 operatorVerified rule (§5.8): non-empty, dotted, and
// not a subdomain of a shared handle provider. "handle.invalid" (the
// placeholder for invalid handles) is never verified.
func IsCustomDomainHandle(handle string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(handle), "."))
	if h == "" || h == "handle.invalid" || !strings.Contains(h, ".") {
		return false
	}
	for _, suffix := range sharedHandleSuffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return false
		}
	}
	return true
}

// operatorState is the derived operator columns for an actors row.
type operatorState struct {
	OperatorDID      *string
	OperatorVerified bool
}

// operatorFor derives operator_did / operator_verified from a profile's
// operator field (§4.3, §5.8 v1 rule):
//
//   - operator absent → unverified, no DID.
//   - operator is a DID → operator_did set; verified iff the identity
//     cache has a handle for that DID and the handle is a custom domain.
//     Conservative: an unresolvable operator DID stays unverified until an
//     #identity event supplies the handle.
//   - operator is a handle → verified iff the handle is a custom domain.
func operatorFor(operator string, handleForDID func(did string) (string, bool)) operatorState {
	op := strings.TrimSpace(operator)
	if op == "" {
		return operatorState{}
	}
	if strings.HasPrefix(op, "did:") {
		st := operatorState{OperatorDID: &op}
		if handle, ok := handleForDID(op); ok && IsCustomDomainHandle(handle) {
			st.OperatorVerified = true
		}
		return st
	}
	return operatorState{OperatorVerified: IsCustomDomainHandle(op)}
}

// profileHash computes the actors.profile_hash column: sha256 over the
// canonical JSON of the profile's model+harness fields (spec §4.3: "hash
// of canonicalized model+harness fields"; the game record's
// playerSnapshots carries it). Component $type markers are stripped before
// hashing so CBOR-written and JSON-written profiles hash identically.
func profileHash(model, harness gt.Option[playsbot.ActorProfile_Component]) (string, error) {
	doc := map[string]any{}
	if model.HasVal() {
		c := model.Val()
		c.LexiconTypeID = ""
		doc["model"] = c
	}
	if harness.HasVal() {
		c := harness.Val()
		c.LexiconTypeID = ""
		doc["harness"] = c
	}
	raw, err := json.Marshal(doc) // encoding/json sorts map keys: canonical
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
