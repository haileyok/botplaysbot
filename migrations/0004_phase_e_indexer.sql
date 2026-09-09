-- +goose Up
-- Phase E repo-event indexer support (spec §2.1, §2.2):
--   cursors: durable event-stream cursor positions. This is a minimal,
--     documented extension to the Appendix A sketch (which has no cursor
--     table): one row per source ("firehose", "jetstream"), storing the
--     last processed stream sequence so an indexer restart resumes where
--     it stopped instead of skipping (cursor too old) or replaying
--     (no cursor) events.
--   challenges.repo_cid: the record CID of a repo-backed challenge.
--     Phase D stored repo_uri only; a valid com.atproto.repo.strongRef
--     needs {uri, cid}, so games accepted from a repo-backed challenge
--     could not carry the challenge strongRef until the indexer (Phase E)
--     fills both columns.

CREATE TABLE cursors (
    name       text PRIMARY KEY,
    seq        bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE challenges ADD COLUMN repo_cid text;

-- A flag is asserted at most once per (subject, kind, game). The indexer
-- relies on this for flag-record emission idempotency (EmitFlag): replaying
-- a repo event or re-running the missing-record sweep must not produce a
-- second flag row/record for the same assertion (spec §10).
CREATE UNIQUE INDEX idx_flags_unique_assertion
    ON flags (subject_did, kind, COALESCE(game_uri, ''));

-- +goose Down
DROP INDEX IF EXISTS idx_flags_unique_assertion;
ALTER TABLE challenges DROP COLUMN repo_cid;
DROP TABLE IF EXISTS cursors;
