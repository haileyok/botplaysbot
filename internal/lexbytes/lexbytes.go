// Package lexbytes normalizes lexicon bytes fields between wire dialects.
//
// The Go server's generated lexicon types decode JSON bytes fields in the
// atmos dialect: {"$bytes": "<unpadded standard base64>"}. The wider ATProto
// TS ecosystem (spec §4.6/§8.1) writes bytes fields as plain base64 strings,
// and the dev PDS passes unknown-collection records through verbatim — so
// records written by @atproto/api agents arrive with ciphertext/nonce as
// JSON strings. NormalizeCommentaryRecordJSON rewrites those string fields
// into the atmos dialect so the typed decoders accept both.
package lexbytes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// NormalizeCommentaryRecordJSON returns raw with commentary bytes fields
// (ciphertext, nonce at the root; ephemeralPublicKey, wrappedKey inside
// escrowKey) rewritten from plain base64 strings into {"$bytes": …}
// objects. Values already in the $bytes dialect, non-strings, missing, or
// not valid base64 are left untouched (invalid input fails typed decoding
// and is indexed as invalid, per §8.4 step 1).
func NormalizeCommentaryRecordJSON(raw []byte) []byte {
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tree); err != nil {
		return raw
	}
	changed := false
	for _, field := range []string{"ciphertext", "nonce"} {
		if normalizeStringField(tree, field) {
			changed = true
		}
	}
	if escRaw, ok := tree["escrowKey"]; ok {
		var esc map[string]json.RawMessage
		if err := json.Unmarshal(escRaw, &esc); err == nil {
			escChanged := false
			for _, field := range []string{"ephemeralPublicKey", "wrappedKey"} {
				if normalizeStringField(esc, field) {
					escChanged = true
				}
			}
			if escChanged {
				out, err := json.Marshal(esc)
				if err == nil {
					tree["escrowKey"] = out
					changed = true
				}
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return raw
	}
	return out
}

// normalizeStringField rewrites m[field] in place when it is a JSON string
// whose value is valid base64 in any common alphabet/padding combination.
// Reports whether it changed the field.
func normalizeStringField(m map[string]json.RawMessage, field string) bool {
	v, ok := m[field]
	if !ok {
		return false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return false // already an object ($bytes dialect) or null: leave it
	}
	b, ok := decodeLenientB64(s)
	if !ok {
		return false
	}
	m[field] = json.RawMessage(fmt.Sprintf(`{"$bytes":%q}`, base64.RawStdEncoding.EncodeToString(b)))
	return true
}

// decodeLenientB64 accepts standard and URL alphabets, padded or not.
func decodeLenientB64(s string) ([]byte, bool) {
	if s == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}
