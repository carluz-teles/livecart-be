-- Only new/pending work can enter the grace period. Completed comments are
-- deliberately untouched: their cart effects may predate durable checkpoints.
ALTER TABLE live_comment_work
    ADD COLUMN binding_wait_until timestamptz,
    ADD COLUMN binding_store_ids uuid[],
    ADD COLUMN binding_session_id uuid;
