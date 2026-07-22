-- Payload versioning: every event carries a schema_version so consumers can
-- evolve payload shapes without breaking. Reconstructed onto the Envelope by the
-- relay (envelopeFromRow) and serialized into the asynq task, so it reaches the
-- consumer. Defaults to 1 for all existing/emitted events.
ALTER TABLE event_outbox
    ADD COLUMN IF NOT EXISTS schema_version INT NOT NULL DEFAULT 1;
