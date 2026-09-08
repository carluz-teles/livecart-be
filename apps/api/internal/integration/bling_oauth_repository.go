package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/lib/httpx"
)

// consumeBlingOAuthState binds the one-time state to its provider in the same
// DELETE that consumes it. A state created for another provider remains usable
// only by that provider, and concurrent callbacks cannot exchange the same code.
func (r *Repository) consumeBlingOAuthState(ctx context.Context, state string) (*OAuthStateRow, error) {
	var row OAuthStateRow
	err := r.pool.QueryRow(ctx, `DELETE FROM oauth_states
		WHERE state = $1 AND provider = 'bling' AND expires_at > now()
		RETURNING state, store_id, provider, COALESCE(code_verifier, ''), created_at, expires_at`, state).
		Scan(&row.State, &row.StoreID, &row.Provider, &row.CodeVerifier, &row.CreatedAt, &row.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.DomainError(422, httpx.CodeValidationFailed, "Autorização expirada, incompatível ou já utilizada. Tente conectar novamente.")
	}
	if err != nil {
		return nil, fmt.Errorf("consumindo a autorização do Bling: %w", err)
	}
	return &row, nil
}

// saveBlingOAuthConnection commits credentials, routing identity, metadata and
// status in one statement. The existing partial unique indexes serialize first
// connections and reject accounts owned by another store, including error state.
func (r *Repository) saveBlingOAuthConnection(
	ctx context.Context, storeID, accountID string, credentials []byte,
	expiresAt time.Time, identity map[string]any,
) (string, error) {
	metadata, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("codificando a identidade do Bling: %w", err)
	}
	expires := pgtype.Timestamptz{Time: expiresAt, Valid: !expiresAt.IsZero()}
	var id string
	err = r.pool.QueryRow(ctx, `INSERT INTO integrations
		(store_id, type, provider, status, credentials, token_expires_at, metadata, erp_account_id)
		VALUES ($1::uuid, 'erp', 'bling', 'active', $2, $3, $4::jsonb, $5)
		ON CONFLICT (store_id) WHERE type = 'erp' DO UPDATE SET
			credentials = EXCLUDED.credentials,
			token_expires_at = EXCLUDED.token_expires_at,
			status = 'active',
			erp_account_id = EXCLUDED.erp_account_id,
			metadata = CASE
				WHEN COALESCE(NULLIF(integrations.erp_account_id, ''), integrations.metadata->>'bling_company_id') = EXCLUDED.erp_account_id
				THEN COALESCE(integrations.metadata, '{}'::jsonb) || EXCLUDED.metadata
				ELSE EXCLUDED.metadata
			END,
			last_synced_at = now()
		WHERE integrations.provider = 'bling' AND (
			COALESCE(NULLIF(integrations.erp_account_id, ''), integrations.metadata->>'bling_company_id') = EXCLUDED.erp_account_id
			OR (
				NOT EXISTS (SELECT 1 FROM products p WHERE p.store_id = integrations.store_id
					AND p.external_source = 'bling' AND NULLIF(p.external_id, '') IS NOT NULL)
				AND NOT EXISTS (SELECT 1 FROM carts c WHERE c.store_id = integrations.store_id
					AND NULLIF(c.external_order_id, '') IS NOT NULL)
				AND NOT EXISTS (SELECT 1 FROM erp_contacts ec WHERE ec.integration_id = integrations.id)
			)
		)
		RETURNING id::text`, storeID, credentials, expires, string(metadata), accountID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", httpx.DomainError(409, httpx.CodeValidationFailed, "Esta loja já possui uma integração ou vínculos de ERP incompatíveis com esta conta. Reconecte a conta original; a troca exige desvinculação explícita dos registros existentes.")
	}
	var dbErr *pgconn.PgError
	if errors.As(err, &dbErr) && dbErr.Code == "23505" && dbErr.ConstraintName == "uniq_integrations_erp_account" {
		return "", httpx.DomainError(409, httpx.CodeValidationFailed, "Esta conta Bling já está conectada a outra loja do LiveCart.")
	}
	if err != nil {
		return "", fmt.Errorf("gravando a conexão do Bling: %w", err)
	}
	return id, nil
}
