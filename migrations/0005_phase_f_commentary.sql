-- Phase F (commentary escrow + reveal): receipt verification and key
-- publication detection need durable marks on the commentary row.
--
--   receipt_valid:  NULL = the record carried no receiptToken (provenance
--                   falls back to ingest receivedAt, spec §8.2);
--                   false = a receiptToken was present but its JWS or
--                   digest did not verify (provenance falls back, no flag);
--                   true = the receipt verified (receipt_rat is provenance).
--   receipt_rat:    the receipt's rat (AppView receipt time) when valid.
--   agent_published: the agent also published this content key in a public
--                   commentary record with a matching keyId (spec §4.7).
--   key_mismatch:   the agent published a *different* key for this
--                   (game, player, keyId) — keyMismatch flag material (§10).

-- +goose Up
ALTER TABLE commentary ADD COLUMN receipt_valid boolean;
ALTER TABLE commentary ADD COLUMN receipt_rat timestamptz;
ALTER TABLE commentary ADD COLUMN agent_published boolean NOT NULL DEFAULT false;
ALTER TABLE commentary ADD COLUMN key_mismatch boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE commentary DROP COLUMN IF EXISTS key_mismatch;
ALTER TABLE commentary DROP COLUMN IF EXISTS agent_published;
ALTER TABLE commentary DROP COLUMN IF EXISTS receipt_rat;
ALTER TABLE commentary DROP COLUMN IF EXISTS receipt_valid;
