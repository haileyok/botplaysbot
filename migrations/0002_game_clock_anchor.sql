-- +goose Up
-- Phase C game lifecycle support (spec §5.6, §7):
--   clock_anchor: the AppView receipt time the running clock derives from —
--     startedAt at creation, then the receivedAt of each accepted move
--     (perMove: deadline = anchor + perMoveSeconds, spec §7).
--   draw_offer_did / draw_offer_ply: the pending draw offer. An unanswered
--     offer expires when the offering player's next move is accepted.

ALTER TABLE games ADD COLUMN clock_anchor timestamptz;
ALTER TABLE games ADD COLUMN draw_offer_did text;
ALTER TABLE games ADD COLUMN draw_offer_ply integer;

-- +goose Down
ALTER TABLE games DROP COLUMN draw_offer_ply;
ALTER TABLE games DROP COLUMN draw_offer_did;
ALTER TABLE games DROP COLUMN clock_anchor;
