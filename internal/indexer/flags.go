package indexer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jcalabro/gt"

	"github.com/haileyok/botplaysbot/internal/games"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot/comatproto"
	"github.com/haileyok/botplaysbot/internal/repo"
	"github.com/haileyok/botplaysbot/internal/servicerepo"
)

// Flag kinds (spec §4.10 knownValues) the indexer asserts.
const (
	FlagMissingMoveRecord   = "missingMoveRecord"
	FlagUnverifiedMoveRecord = "unverifiedMoveRecord"
)

// Flag severities (spec §4.10).
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
)

// FlagEmitter asserts an anomaly flag end to end: the flags table row plus
// the bot.plays.bot.flag record in the service repo (spec §4.10, §10 —
// "All detections produce bot.plays.bot.flag records").
//
// Emission is exactly-once per (subject, kind, game): the unique
// idx_flags_unique_assertion index arbitrates, InsertOnce reports whether
// this call created the row, and the record is written only then. A
// re-ingested repo event or a repeated missing-record sweep therefore
// never duplicates the repo record. A nil writer (service account
// unconfigured) degrades to rows-only.
type FlagEmitter struct {
	repos  *repo.Pool
	writer *servicerepo.Writer
	log    *slog.Logger
	now    func() time.Time
}

// NewFlagEmitter assembles the emitter. writer may be nil in tests.
func NewFlagEmitter(repos *repo.Pool, writer *servicerepo.Writer, logger *slog.Logger) *FlagEmitter {
	return &FlagEmitter{repos: repos, writer: writer, log: logger, now: time.Now}
}

// EmitFlag asserts the flag (row + service-repo record). It reports whether
// this call was the one that created the assertion. Errors from the record
// write do not roll back the row: the row is the AppView's internal state,
// the record is the public log, and the next sweep/ingest backfills the
// record URI on the row.
func (e *FlagEmitter) EmitFlag(ctx context.Context, subjectDID string, gameURI *string, kind, severity, detail string) (bool, error) {
	f := &repo.Flag{
		SubjectDID: subjectDID,
		GameURI:    gameURI,
		Kind:       kind,
		Severity:   severity,
		Detail:     &detail,
	}
	inserted, err := e.repos.Flags.InsertOnce(ctx, f)
	if err != nil {
		return false, err
	}
	if !inserted {
		e.log.Debug("indexer: flag already asserted", "subject", subjectDID, "kind", kind)
		return false, nil
	}

	if e.writer == nil {
		return true, nil
	}

	rec := &playsbot.BotFlag{
		Subject:   subjectDID,
		Kind:      kind,
		Severity:  severity,
		Detail:    gt.Some(detail),
		CreatedAt: games.RFC3339Millis(e.now()),
	}
	if gameURI != nil {
		if ref, err := e.gameStrongRef(ctx, *gameURI); err != nil {
			e.log.Warn("indexer: flag record game strongRef lookup failed", "game", *gameURI, "err", err)
		} else if ref != nil {
			rec.Game = gt.Some(*ref)
		}
	}

	res, err := e.writer.WriteFlagRecord(ctx, rec)
	if err != nil {
		if !isNotConfigured(err) {
			e.log.Error("indexer: flag record write failed", "subject", subjectDID, "kind", kind, "err", err)
			return true, err
		}
		return true, nil // inert service account: row stands alone
	}
	if err := e.repos.Flags.SetRepoURI(ctx, f.ID, res.URI); err != nil {
		e.log.Warn("indexer: flag row repo_uri backfill failed", "flag", f.ID, "err", err)
	}
	return true, nil
}

// gameStrongRef builds the com.atproto.repo.strongRef for a game record,
// or nil when the game row has no CID yet (service repo unconfigured).
func (e *FlagEmitter) gameStrongRef(ctx context.Context, gameURI string) (*comatproto.RepoStrongRef, error) {
	g, err := e.repos.Games.Get(ctx, gameURI)
	if err != nil {
		return nil, err
	}
	if g.CID == nil {
		return nil, nil
	}
	return &comatproto.RepoStrongRef{URI: gameURI, CID: *g.CID}, nil
}

func isNotConfigured(err error) bool {
	return errors.Is(err, servicerepo.ErrNotConfigured)
}
