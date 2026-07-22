-- Emit-time idempotency for the transactional outbox: dedup_key uniquely
-- identifies a LOGICAL event, so a duplicate emit (webhook retry, at-least-once
-- producer, self-reschedule) collapses to a single row via ON CONFLICT DO
-- NOTHING on the insert. Partial index: dedup_key = '' opts out (events that
-- deliberately don't set a key are not deduped). Published rows are retained, so
-- the dedup window is the table lifetime.

-- Existing duplicates would block the unique index — collapse them first,
-- keeping the earliest row per key (only dedup_key <> '').
DELETE FROM event_outbox a
USING event_outbox b
WHERE a.dedup_key <> ''
  AND a.dedup_key = b.dedup_key
  AND a.ctid > b.ctid;

CREATE UNIQUE INDEX IF NOT EXISTS event_outbox_dedup_key_uniq
    ON event_outbox (dedup_key)
    WHERE dedup_key <> '';
