package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// The operator association script locks the same store row. This prevents a
// simultaneous OAuth callback from creating an independent copy of the token.
func (r *Repository) createInstagram(ctx context.Context, params CreateIntegrationParams) (_ *IntegrationRow, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(cleanupCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	var storeID string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM stores WHERE id=$1::uuid FOR UPDATE", params.StoreID).Scan(&storeID); err != nil {
		return nil, err
	}
	repo := &Repository{queries: r.queries.WithTx(tx)}
	existing, err := repo.GetAnyByType(ctx, storeID, "social")
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, httpx.ErrConflict("Instagram já conectado. Use a conexão existente.")
	}
	row, err := repo.createIntegration(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// Resolve credentials only through the server-managed FK, never through
// caller-supplied metadata. Keep the caller's integration/store for attribution.
func (r *Repository) resolveIntegrationRow(ctx context.Context, raw sqlc.Integration) (*IntegrationRow, error) {
	row := r.toIntegrationRow(raw)
	if !raw.InstagramCredentialsSourceID.Valid {
		return row, nil
	}
	source, err := r.queries.GetIntegrationByIDOnly(ctx, raw.InstagramCredentialsSourceID)
	if err != nil {
		return nil, fmt.Errorf("reading shared Instagram credentials: %w", err)
	}
	if row.Provider != "instagram" || row.Type != "social" || source.Provider != "instagram" ||
		source.Type != "social" || source.InstagramCredentialsSourceID.Valid {
		return nil, httpx.ErrConflict("Vínculo de credenciais Instagram inválido.")
	}
	owner := r.toIntegrationRow(source)
	row.InstagramCredentialsSourceID = owner.ID
	row.InstagramCredentialsSourceStoreID = owner.StoreID
	row.Credentials = owner.Credentials
	row.TokenExpiresAt = owner.TokenExpiresAt
	if row.Status == "active" {
		row.Status = owner.Status
	}
	if row.Metadata == nil {
		row.Metadata = make(map[string]any)
	}
	for _, key := range []string{"instagram_user_id", "instagram_app_scoped_id", "username"} {
		delete(row.Metadata, key)
		if value, ok := owner.Metadata[key]; ok {
			row.Metadata[key] = value
		}
	}
	return row, nil
}

func (s *Service) requireInstagramAuthorizationOwner(ctx context.Context, storeID string) (*IntegrationRow, error) {
	existing, err := s.repo.GetAnyByType(ctx, storeID, "social")
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.InstagramCredentialsSourceID != "" {
		return nil, httpx.ErrConflict("Este Instagram é compartilhado. Reconecte pela loja responsável pela conexão principal.")
	}
	return existing, nil
}

// Workers and requests from either store lock the same credential owner, also
// across separate API instances. Re-read after locking to reuse a completed refresh.
func (s *Service) refreshInstagramToken(ctx context.Context, integration *IntegrationRow, previous *providers.Credentials) (*providers.Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	current, err := s.repo.GetByID(ctx, integration.ID, integration.StoreID)
	if err != nil {
		return nil, err
	}
	if current.Status != "active" {
		return nil, httpx.ErrConflict("A conexão Instagram não está ativa. Verifique a conexão principal.")
	}
	id, storeID := current.ID, current.StoreID
	if current.InstagramCredentialsSourceID != "" {
		id, storeID = current.InstagramCredentialsSourceID, current.InstagramCredentialsSourceStoreID
	}
	var refreshed *providers.Credentials
	var refreshErr error
	err = s.repo.withIntegrationRefreshLock(ctx, id, storeID, func(repo *Repository, owner *IntegrationRow) error {
		if owner.Status != "active" || owner.Provider != "instagram" || owner.InstagramCredentialsSourceID != "" {
			return httpx.ErrConflict("A conexão principal do Instagram não está ativa.")
		}
		latest, err := s.decryptCredentials(owner.Credentials)
		if err != nil {
			return err
		}
		if previous != nil && !latest.IsExpired() &&
			(latest.AccessToken != previous.AccessToken || latest.ExpiresAt.After(previous.ExpiresAt)) {
			refreshed = latest
			return nil
		}
		refreshed, refreshErr = s.refreshTokenWithRepository(ctx, repo, owner, latest)
		// Commit a permanent authorization failure as well as successful rotation.
		return nil
	})
	return refreshed, errors.Join(refreshErr, err)
}

// OAuth and refresh write through the same lock. A shared connection may be
// renewed for the same account, but cannot silently switch every linked store
// to another Instagram account.
func (s *Service) saveInstagramAuthorization(ctx context.Context, existing *IntegrationRow, encrypted []byte, expiresAt time.Time, metadata map[string]any) error {
	return s.repo.withIntegrationRefreshLock(ctx, existing.ID, existing.StoreID, func(repo *Repository, current *IntegrationRow) error {
		if current.InstagramCredentialsSourceID != "" || current.Provider != "instagram" {
			return httpx.ErrConflict("Reconecte o Instagram pela conexão principal.")
		}
		id, err := parseUUID(current.ID)
		if err != nil {
			return err
		}
		shared, err := repo.queries.HasInstagramCredentialAliases(ctx, id)
		if err != nil {
			return fmt.Errorf("checking shared Instagram authorization: %w", err)
		}
		if shared {
			oldID, _ := current.Metadata["instagram_user_id"].(string)
			newID, _ := metadata["instagram_user_id"].(string)
			if oldID == "" || oldID != newID {
				return httpx.ErrConflict("O Instagram autorizado difere da conta compartilhada. Autorize o mesmo perfil.")
			}
		}
		merged := make(map[string]any)
		for k, v := range current.Metadata {
			merged[k] = v
		}
		for k, v := range metadata {
			merged[k] = v
		}
		if _, ok := merged["connected_at"]; !ok {
			merged["connected_at"] = time.Now()
		}
		if err := repo.UpdateCredentials(ctx, current.ID, encrypted, &expiresAt); err != nil {
			return fmt.Errorf("saving Instagram authorization: %w", err)
		}
		return repo.UpdateMetadata(ctx, current.ID, merged)
	})
}
