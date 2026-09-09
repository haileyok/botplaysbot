// Package indexer consumes ATProto repo events (the firehose, or Jetstream)
// and applies the plays.bot ingest rules to the AppView's Postgres state
// (spec §2.1, §2.2 step 6, §4.2, §4.5, §10).
//
// Structure:
//
//   - RepoEvent is the transport-agnostic internal event shape; both
//     transports normalize into it (contract tests assert the
//     normalizations agree).
//   - Source wraps atmos's streaming client for one transport; the dial
//     layer is injectable so tests feed hand-authored frames.
//   - Ingestor applies the per-collection rules (ignore rule, move-record
//     verification/linking, challenge and profile upserts).
//   - FlagEmitter pairs a flags row with a bot.plays.bot.flag record in the
//     service repo, exactly once per (subject, kind, game).
//
// The package is deliberately the only place that knows about the wire
// formats: everything downstream works on RepoEvent and decoded lexicon
// records.
package indexer

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/jcalabro/atmos/cbor"
	"github.com/jcalabro/atmos/streaming"

	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/lexbytes"
)

// Kind is the mutation type of a RepoEvent.
type Kind string

const (
	KindCreate Kind = "create"
	KindUpdate Kind = "update"
	KindDelete Kind = "delete"
)

// RepoEvent is one record mutation, normalized from either transport
// (firehose #commit ops or Jetstream #commit events). Create/update carry
// CID plus the decoded record as canonical JSON; deletes carry neither.
//
// Record is json.RawMessage rather than `any`: the generated lexicon types
// marshal deterministically (fixed key order), so decoding each transport's
// wire form into the typed record and re-marshaling yields byte-identical
// canonical JSON. That makes RepoEvent values directly comparable, which is
// what the transport contract test asserts.
type RepoEvent struct {
	Kind       Kind
	DID        string
	Collection string
	Rkey       string
	CID        string          // "" for deletes
	Record     json.RawMessage // nil for deletes
	CommitRev  string
}

// Equal reports whether two events describe the same mutation with the same
// content (used by the transport contract test and by ingest idempotency
// reasoning).
func (e RepoEvent) Equal(o RepoEvent) bool {
	return e.Kind == o.Kind &&
		e.DID == o.DID &&
		e.Collection == o.Collection &&
		e.Rkey == o.Rkey &&
		e.CID == o.CID &&
		e.CommitRev == o.CommitRev &&
		bytes.Equal(e.Record, o.Record)
}

// repoURI is the at:// URI of the mutated record.
func (e RepoEvent) repoURI() string {
	return fmt.Sprintf("at://%s/%s/%s", e.DID, e.Collection, e.Rkey)
}

// NamespacePrefix is the lexicon namespace the indexer cares about.
const NamespacePrefix = "bot.plays.bot."

// normalizeFirehose converts an atmos streaming Event from a
// subscribeRepos stream into RepoEvents. Commit ops decode through the
// event's CAR (atmos streaming/car/cbor); non-commit events yield nothing
// (identity handling is the caller's, via the raw Event).
func normalizeFirehose(evt streaming.Event) ([]RepoEvent, error) {
	if evt.Commit == nil {
		return nil, nil
	}
	var out []RepoEvent
	for op, err := range evt.Operations() {
		if err != nil {
			return out, fmt.Errorf("decode commit op: %w", err)
		}
		switch op.Action {
		case streaming.ActionCreate, streaming.ActionUpdate, streaming.ActionDelete:
			// handled below
		case streaming.ActionResync:
			// A #sync-triggered authoritative repo replacement. v1 does
			// not replay whole repos (documented Phase F follow-up):
			// commit diffs are the only ingest path.
			continue
		default:
			continue
		}

		ev := RepoEvent{
			DID:        op.Repo.String(),
			Collection: op.Collection.String(),
			Rkey:       op.RKey.String(),
			CommitRev:  op.Rev.String(),
		}
		switch op.Action {
		case streaming.ActionCreate:
			ev.Kind = KindCreate
		case streaming.ActionUpdate:
			ev.Kind = KindUpdate
		case streaming.ActionDelete:
			ev.Kind = KindDelete
		}
		if op.CID.Defined() {
			ev.CID = op.CID.String()
		}
		if ev.Kind != KindDelete {
			rec, err := decodeRecordCBOR(ev.Collection, op.BlockData())
			if err != nil {
				return out, fmt.Errorf("decode %s/%s: %w", ev.Collection, ev.Rkey, err)
			}
			ev.Record = rec
		}
		out = append(out, ev)
	}
	return out, nil
}

// normalizeJetstream converts an atmos streaming Event from a Jetstream
// stream into RepoEvents. The commit's record arrives as JSON and is
// normalized through the same typed decode/re-marshal as the firehose's
// CBOR path.
func normalizeJetstream(evt streaming.Event) ([]RepoEvent, error) {
	js := evt.Jetstream
	if js == nil || js.Commit == nil {
		return nil, nil
	}
	jc := js.Commit

	var kind Kind
	switch jc.Operation {
	case streaming.JetstreamOpCreate:
		kind = KindCreate
	case streaming.JetstreamOpUpdate:
		kind = KindUpdate
	case streaming.JetstreamOpDelete:
		kind = KindDelete
	default:
		return nil, fmt.Errorf("jetstream: unknown operation %q", jc.Operation)
	}

	ev := RepoEvent{
		Kind:       kind,
		DID:        js.DID,
		Collection: jc.Collection,
		Rkey:       jc.RKey,
		CID:        jc.CID,
		CommitRev:  jc.Rev,
	}
	if kind != KindDelete && len(jc.Record) > 0 {
		rec, err := decodeRecordJSON(ev.Collection, jc.Record)
		if err != nil {
			return nil, fmt.Errorf("jetstream: decode %s/%s: %w", ev.Collection, ev.Rkey, err)
		}
		ev.Record = rec
	}
	return []RepoEvent{ev}, nil
}

// decodeRecordCBOR normalizes a CBOR record block into canonical JSON:
// known bot.plays.bot collections decode through the generated lexicon
// types (whose MarshalJSON is deterministic); anything else decodes
// generically so unexpected-but-namespaced records still flow.
func decodeRecordCBOR(collection string, data []byte) (json.RawMessage, error) {
	if data == nil {
		return nil, fmt.Errorf("no record data")
	}
	if rec, ok := newKnownRecord(collection); ok {
		if collection == playsbot.NSIDGameCommentary {
			// Records written by TS agents carry bytes fields as CBOR/JSON
			// strings (the dev PDS passes unknown collections through); the
			// generated types want the $bytes dialect. Normalize via a
			// generic decode: CBOR → any → JSON (Go []byte → base64 string)
			// → lexbytes normalization → typed decode.
			var generic any
			generic, err := cbor.Unmarshal(data)
			if err != nil {
				return nil, err
			}
			asJSON, err := json.Marshal(generic)
			if err != nil {
				return nil, err
			}
			normalized := lexbytes.NormalizeCommentaryRecordJSON(asJSON)
			if err := rec.UnmarshalJSON(normalized); err != nil {
				return nil, err
			}
			return json.Marshal(rec)
		}
		if err := rec.UnmarshalCBOR(data); err != nil {
			return nil, err
		}
		return json.Marshal(rec)
	}
	v, err := cbor.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// decodeRecordJSON is decodeRecordCBOR's JSON counterpart (Jetstream).
func decodeRecordJSON(collection string, data []byte) (json.RawMessage, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("no record data")
	}
	if rec, ok := newKnownRecord(collection); ok {
		if collection == playsbot.NSIDGameCommentary {
			data = lexbytes.NormalizeCommentaryRecordJSON(data)
		}
		if err := rec.UnmarshalJSON(data); err != nil {
			return nil, err
		}
		return json.Marshal(rec)
	}
	return json.RawMessage(data), nil
}

// newKnownRecord returns a fresh typed record value for the collections the
// indexer understands, so both transports normalize through the same
// generated types. The bool reports whether collection is known.
func newKnownRecord(collection string) (record, bool) {
	switch collection {
	case playsbot.NSIDBotGame:
		return record(&playsbot.BotGame{}), true
	case playsbot.NSIDGameMove:
		return record(&playsbot.GameMove{}), true
	case playsbot.NSIDGameChallenge:
		return record(&playsbot.GameChallenge{}), true
	case playsbot.NSIDGameCommentary:
		return record(&playsbot.GameCommentary{}), true
	case playsbot.NSIDGameReveal:
		return record(&playsbot.GameReveal{}), true
	case playsbot.NSIDActorProfile:
		return record(&playsbot.ActorProfile{}), true
	case playsbot.NSIDBotRating:
		return record(&playsbot.BotRating{}), true
	case playsbot.NSIDBotFlag:
		return record(&playsbot.BotFlag{}), true
	default:
		return nil, false
	}
}

// record erases the concrete generated type so a switch can hold any of
// them; both atmos wire formats are supported on every generated record.
type record interface {
	UnmarshalCBOR([]byte) error
	UnmarshalJSON([]byte) error
}
