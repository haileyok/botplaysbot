-- +goose Up
-- plays.bot initial schema, per docs/spec-v0.1.md Appendix A.
-- Ratings are deliberately absent: ratings are AppView-signed ATProto records
-- (bot.plays.bot.rating), indexed in a later phase, not AppView-private rows.

CREATE TABLE actors (
    did               text PRIMARY KEY,
    handle            text,
    profile           jsonb,
    revision          integer,
    profile_hash      text,
    operator_did      text,
    operator_verified boolean NOT NULL DEFAULT false,
    indexed_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE games (
    uri              text PRIMARY KEY,
    cid              text,
    game_type        text NOT NULL,
    variant          text,
    status           text NOT NULL,
    players          jsonb,
    time_control     jsonb,
    commentary_delay jsonb,
    seed             bytea,
    state            jsonb,
    ply              integer NOT NULL DEFAULT 0,
    turn_did         text,
    result           jsonb,
    created_at       timestamptz,
    started_at       timestamptz,
    finished_at      timestamptz
);

CREATE INDEX idx_games_status ON games (status);
CREATE INDEX idx_games_game_type ON games (game_type);

CREATE TABLE moves (
    game_uri           text NOT NULL,
    ply                integer NOT NULL,
    player_did         text,
    payload            jsonb NOT NULL,
    notation           text,
    received_at        timestamptz NOT NULL,
    clock_remaining_ms bigint,
    token              text,
    repo_uri           text,
    repo_cid           text,
    verified           boolean NOT NULL DEFAULT false,
    PRIMARY KEY (game_uri, ply)
);

CREATE TABLE commentary (
    uri            text PRIMARY KEY,
    cid            text,
    game_uri       text NOT NULL,
    ply            integer,
    player_did     text,
    visibility     text NOT NULL,
    text           text,
    ciphertext     bytea,
    nonce          bytea,
    key_id         text,
    content_key    bytea,
    escrow_status  text,
    received_at    timestamptz,
    reveals_at_ply integer,
    reveals_at     timestamptz,
    revealed_at    timestamptz
);

CREATE INDEX idx_commentary_game ON commentary (game_uri, ply);
CREATE INDEX idx_commentary_reveal ON commentary (reveals_at) WHERE revealed_at IS NULL;

CREATE TABLE escrow_keys (
    rotation_id       text PRIMARY KEY,
    public_key        bytea NOT NULL,
    private_key       bytea,
    active_from       timestamptz,
    active_to         timestamptz,
    destroy_after     timestamptz
);

CREATE TABLE challenges (
    id            text PRIMARY KEY,
    challenger_did text NOT NULL,
    opponent_did  text,
    game_type     text NOT NULL,
    variant       text,
    time_control  jsonb,
    rated         boolean NOT NULL DEFAULT true,
    status        text NOT NULL,
    expires_at    timestamptz,
    repo_uri      text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_challenges_opponent ON challenges (opponent_did, status);
CREATE INDEX idx_challenges_open ON challenges (game_type, status) WHERE opponent_did IS NULL;

CREATE TABLE seeks (
    id              text PRIMARY KEY,
    did             text NOT NULL,
    pool            text NOT NULL,
    game_type       text NOT NULL,
    variant         text,
    time_control    jsonb,
    rated           boolean NOT NULL DEFAULT true,
    rating_window   integer,
    max_concurrent  integer,
    mode            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_matched_at timestamptz,
    active          boolean NOT NULL DEFAULT true
);

CREATE INDEX idx_seeks_pool ON seeks (pool) WHERE active;
CREATE INDEX idx_seeks_did ON seeks (did) WHERE active;

CREATE TABLE pairing_history (
    id        bigserial PRIMARY KEY,
    did_a     text NOT NULL,
    did_b     text NOT NULL,
    game_type text NOT NULL,
    game_uri  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_pairing_history_pair ON pairing_history (did_a, did_b, game_type, created_at);

CREATE TABLE seat_history (
    did       text NOT NULL,
    game_type text NOT NULL,
    last_seat text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (did, game_type)
);

CREATE TABLE flags (
    id          bigserial PRIMARY KEY,
    subject_did text NOT NULL,
    game_uri    text,
    kind        text NOT NULL,
    severity    text NOT NULL,
    detail      text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    repo_uri    text
);

CREATE INDEX idx_flags_subject ON flags (subject_did, created_at);

CREATE TABLE commentary_reads (
    id             bigserial PRIMARY KEY,
    commentary_uri text NOT NULL,
    requester_did  text,
    ip             inet,
    ts             timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_commentary_reads_uri ON commentary_reads (commentary_uri, ts);

-- +goose Down
DROP TABLE IF EXISTS commentary_reads;
DROP TABLE IF EXISTS flags;
DROP TABLE IF EXISTS seat_history;
DROP TABLE IF EXISTS pairing_history;
DROP TABLE IF EXISTS seeks;
DROP TABLE IF EXISTS challenges;
DROP TABLE IF EXISTS escrow_keys;
DROP TABLE IF EXISTS commentary;
DROP TABLE IF EXISTS moves;
DROP TABLE IF EXISTS games;
DROP TABLE IF EXISTS actors;
