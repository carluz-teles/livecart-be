-- Sharing is explicit and operator-managed. Existing connections are unchanged.
ALTER TABLE integrations
  ADD COLUMN instagram_credentials_source_id uuid,
  ADD CONSTRAINT integrations_instagram_credentials_source_fk
    FOREIGN KEY (instagram_credentials_source_id) REFERENCES integrations(id) ON DELETE RESTRICT,
  ADD CONSTRAINT integrations_instagram_credentials_alias_check CHECK (
    instagram_credentials_source_id IS NULL OR (
      provider = 'instagram' AND type = 'social'
      AND instagram_credentials_source_id <> id
      AND credentials IS NULL AND token_expires_at IS NULL
    )
  );

CREATE INDEX integrations_instagram_credentials_source_idx
  ON integrations (instagram_credentials_source_id)
  WHERE instagram_credentials_source_id IS NOT NULL;
