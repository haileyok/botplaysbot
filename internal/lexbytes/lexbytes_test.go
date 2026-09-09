package lexbytes

import (
	"encoding/json"
	"testing"
)

func TestNormalizeBothDialects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"padded std string", `{"ciphertext":"AAEC","nonce":"AQID","escrowKey":{"rotationId":"r1","ephemeralPublicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","wrappedKey":"AAECAAEC"}}`},
		{"unpadded std string", `{"ciphertext":"AAEC","nonce":"AQID","escrowKey":{"rotationId":"r1","ephemeralPublicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","wrappedKey":"AAECAAEC"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := NormalizeCommentaryRecordJSON([]byte(tc.in))
			var tree map[string]json.RawMessage
			if err := json.Unmarshal(out, &tree); err != nil {
				t.Fatalf("normalized JSON invalid: %v", err)
			}
			var ct struct {
				Bytes string `json:"$bytes"`
			}
			if err := json.Unmarshal(tree["ciphertext"], &ct); err != nil {
				t.Fatalf("ciphertext not $bytes dialect: %s", tree["ciphertext"])
			}
			if ct.Bytes != "AAEC" {
				t.Fatalf("ciphertext = %q, want AAEC (unpadded)", ct.Bytes)
			}
		})
	}
}

func TestNormalizeLeavesOtherDialect(t *testing.T) {
	in := `{"ciphertext":{"$bytes":"AAEC"},"escrowKey":{"rotationId":"r1","ephemeralPublicKey":{"$bytes":"AAEC"},"wrappedKey":{"$bytes":"AAEC"}},"text":"hi"}`
	out := string(NormalizeCommentaryRecordJSON([]byte(in)))
	if out != in {
		t.Fatalf("input in $bytes dialect was modified:\n in: %s\nout: %s", in, out)
	}
}

func TestNormalizeLeavesInvalidBase64(t *testing.T) {
	in := `{"ciphertext":"!!!not-base64!!!"}`
	out := string(NormalizeCommentaryRecordJSON([]byte(in)))
	if out != in {
		t.Fatalf("invalid base64 should pass through untouched, got %s", out)
	}
}

func TestNormalizeGarbage(t *testing.T) {
	in := `not json at all`
	if out := string(NormalizeCommentaryRecordJSON([]byte(in))); out != in {
		t.Fatalf("garbage should pass through, got %s", out)
	}
}
