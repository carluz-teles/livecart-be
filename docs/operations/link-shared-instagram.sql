-- Run only after migration 156 AND the matching backend are deployed.
-- psql -X -v ON_ERROR_STOP=1 -v source_integration_id=UUID
--   -v target_store_id=UUID -v expected_account_id=INSTAGRAM_ID -f this-file.sql
-- No tokens are copied or printed. This creates only a social integration.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '15s';
SELECT set_config('livecart.link_source_id', :'source_integration_id', true);
SELECT set_config('livecart.link_target_store_id', :'target_store_id', true);
SELECT set_config('livecart.link_expected_account_id', :'expected_account_id', true);

DO $$
DECLARE
  source_id uuid := current_setting('livecart.link_source_id')::uuid;
  target_id uuid := current_setting('livecart.link_target_store_id')::uuid;
  expected_id text := current_setting('livecart.link_expected_account_id');
  owner integrations%ROWTYPE;
  existing integrations%ROWTYPE;
  target_count integer;
BEGIN
  SELECT * INTO STRICT owner FROM integrations WHERE id = source_id FOR UPDATE;
  IF owner.type <> 'social' OR owner.provider <> 'instagram' OR owner.status <> 'active'
     OR owner.instagram_credentials_source_id IS NOT NULL
     OR owner.store_id = target_id
     OR COALESCE(octet_length(owner.credentials), 0) = 0
     OR owner.token_expires_at IS NULL OR owner.token_expires_at <= now() + interval '30 minutes'
     OR expected_id = '' OR COALESCE(owner.metadata->>'instagram_user_id', '') <> expected_id THEN
    RAISE EXCEPTION 'Source must be the expected active Instagram with valid credentials in another store';
  END IF;
  PERFORM 1 FROM stores WHERE id = target_id AND active = true FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'Target store is missing or inactive'; END IF;

  SELECT count(*) INTO target_count FROM integrations WHERE store_id = target_id AND type = 'social';
  IF target_count > 0 THEN
    SELECT * INTO existing FROM integrations WHERE store_id = target_id AND type = 'social';
    IF target_count = 1 AND existing.instagram_credentials_source_id = source_id AND existing.status = 'active' THEN
      RETURN; -- Idempotent: an approved link already exists.
    END IF;
    RAISE EXCEPTION 'Target already has a social integration; review it before linking';
  END IF;

  INSERT INTO integrations (store_id, type, provider, status, instagram_credentials_source_id, metadata)
  VALUES (target_id, 'social', 'instagram', 'active', source_id,
    jsonb_build_object('connected_at', now()));
END $$;

SELECT id, store_id, provider, status, instagram_credentials_source_id
FROM integrations WHERE store_id = current_setting('livecart.link_target_store_id')::uuid AND type = 'social';
COMMIT;
