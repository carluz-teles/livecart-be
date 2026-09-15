-- Never silently leave an alias active without its credential source.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM integrations WHERE instagram_credentials_source_id IS NOT NULL) THEN
    RAISE EXCEPTION 'Disconnect shared Instagram aliases before rolling back migration 156';
  END IF;
END $$;
DROP INDEX integrations_instagram_credentials_source_idx;
ALTER TABLE integrations
  DROP CONSTRAINT integrations_instagram_credentials_alias_check,
  DROP CONSTRAINT integrations_instagram_credentials_source_fk,
  DROP COLUMN instagram_credentials_source_id;
