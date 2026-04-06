-- OAuth states table for PKCE flow
-- Stores code_verifier temporarily during OAuth authorization
CREATE TABLE oauth_states (
    state VARCHAR(255) PRIMARY KEY,
    store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE,
    provider VARCHAR(50) NOT NULL,
    code_verifier VARCHAR(128) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '10 minutes')
);

-- Index for cleanup of expired states
CREATE INDEX idx_oauth_states_expires_at ON oauth_states(expires_at);

-- Comment
COMMENT ON TABLE oauth_states IS 'Temporary storage for OAuth PKCE code_verifier during authorization flow';
