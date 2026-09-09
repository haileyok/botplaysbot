-- +goose Up
-- Phase D matchmaking support (spec §9a, §5.5):
--   games.matchmaking: the {pool, waitMs, ratingGap} object carried on games
--     created by the seek pool (§9a.5). NULL for challenge/direct games.
--   games.challenge_uri / challenge_cid: the challenge strongRef carried on
--     games created from a repo-backed challenge (§9a.5). NULL when the game
--     came from the pool or an XRPC-only challenge (no indexed record).
--   challenges.seat_preference: challenger's seat wish (first/second/random).
--   challenges.commentary_delay: per-challenge broadcast delay.
--   seek_suspensions: DIDs currently barred from the seek pool after three
--     consecutive ply-1/ply-2 no-shows (§9a.3, flag timingAnomaly). The
--     consecutive-count streak itself is in-memory (single-process AppView);
--     the suspension and the flag row are durable.

ALTER TABLE games ADD COLUMN matchmaking jsonb;
ALTER TABLE games ADD COLUMN challenge_uri text;
ALTER TABLE games ADD COLUMN challenge_cid text;

ALTER TABLE challenges ADD COLUMN seat_preference text NOT NULL DEFAULT 'random';
ALTER TABLE challenges ADD COLUMN commentary_delay jsonb;

CREATE TABLE seek_suspensions (
    did        text PRIMARY KEY,
    until      timestamptz NOT NULL,
    reason     text,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS seek_suspensions;
ALTER TABLE challenges DROP COLUMN commentary_delay;
ALTER TABLE challenges DROP COLUMN seat_preference;
ALTER TABLE games DROP COLUMN challenge_cid;
ALTER TABLE games DROP COLUMN challenge_uri;
ALTER TABLE games DROP COLUMN matchmaking;
