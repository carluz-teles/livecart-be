package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
)

// Repository handles database operations for integrations.
type Repository struct {
	queries *sqlc.Queries
	pool    *pgxpool.Pool
}

// NewRepository creates a new integration repository.
func NewRepository(queries *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{
		queries: queries,
		pool:    pool,
	}
}

// =============================================================================
// INTEGRATIONS
// =============================================================================

// Create creates a new integration.
func (r *Repository) Create(ctx context.Context, params CreateIntegrationParams) (*IntegrationRow, error) {
	storeID, err := parseUUID(params.StoreID)
	if err != nil {
		return nil, err
	}

	var tokenExpiresAt pgtype.Timestamptz
	if params.TokenExpiresAt != nil {
		tokenExpiresAt = pgtype.Timestamptz{Time: *params.TokenExpiresAt, Valid: true}
	}

	var metadataStr string
	if params.Metadata != nil {
		metadataJSON, err := json.Marshal(params.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata: %w", err)
		}
		metadataStr = string(metadataJSON)
	} else {
		metadataStr = "{}"
	}

	sqlParams := sqlc.CreateIntegrationParams{
		StoreID:        storeID,
		Type:           params.Type,
		Provider:       params.Provider,
		Status:         params.Status,
		Credentials:    params.Credentials,
		TokenExpiresAt: tokenExpiresAt,
		Column7:        metadataStr,
	}

	row, err := r.queries.CreateIntegration(ctx, sqlParams)
	if err != nil {
		return nil, fmt.Errorf("creating integration: %w", err)
	}

	return r.toIntegrationRow(row), nil
}

// GetByID retrieves an integration by ID and store ID.
func (r *Repository) GetByID(ctx context.Context, id, storeID string) (*IntegrationRow, error) {
	integrationID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetIntegrationByID(ctx, sqlc.GetIntegrationByIDParams{
		ID:      integrationID,
		StoreID: sID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, httpx.ErrNotFound("integration not found")
		}
		return nil, fmt.Errorf("getting integration: %w", err)
	}

	return r.toIntegrationRow(row), nil
}

// GetByIDOnly retrieves an integration by ID only (for webhook handlers).
func (r *Repository) GetByIDOnly(ctx context.Context, id string) (*IntegrationRow, error) {
	integrationID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetIntegrationByIDOnly(ctx, integrationID)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, httpx.ErrNotFound("integration not found")
		}
		return nil, fmt.Errorf("getting integration: %w", err)
	}

	return r.toIntegrationRow(row), nil
}

// ListByStore lists all integrations for a store.
func (r *Repository) ListByStore(ctx context.Context, storeID string) ([]IntegrationRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.queries.ListIntegrationsByStore(ctx, sID)
	if err != nil {
		return nil, fmt.Errorf("listing integrations: %w", err)
	}

	result := make([]IntegrationRow, len(rows))
	for i, row := range rows {
		result[i] = *r.toIntegrationRow(row)
	}

	return result, nil
}

// ListByType lists active integrations by type for a store.
func (r *Repository) ListByType(ctx context.Context, storeID, integrationType string) ([]IntegrationRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.queries.ListIntegrationsByType(ctx, sqlc.ListIntegrationsByTypeParams{
		StoreID: sID,
		Type:    integrationType,
	})
	if err != nil {
		return nil, fmt.Errorf("listing integrations by type: %w", err)
	}

	result := make([]IntegrationRow, len(rows))
	for i, row := range rows {
		result[i] = *r.toIntegrationRow(row)
	}

	return result, nil
}

// GetActiveByProvider gets an active integration by type and provider.
func (r *Repository) GetActiveByProvider(ctx context.Context, storeID, integrationType, provider string) (*IntegrationRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetActiveIntegrationByProvider(ctx, sqlc.GetActiveIntegrationByProviderParams{
		StoreID:  sID,
		Type:     integrationType,
		Provider: provider,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, httpx.ErrNotFound("active integration not found")
		}
		return nil, fmt.Errorf("getting active integration: %w", err)
	}

	return r.toIntegrationRow(row), nil
}

// GetByProvider gets an integration by type and provider (active or pending_auth).
func (r *Repository) GetByProvider(ctx context.Context, storeID, integrationType, provider string) (*IntegrationRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetIntegrationByProvider(ctx, sqlc.GetIntegrationByProviderParams{
		StoreID:  sID,
		Type:     integrationType,
		Provider: provider,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, httpx.ErrNotFound("integration not found")
		}
		return nil, fmt.Errorf("getting integration: %w", err)
	}

	return r.toIntegrationRow(row), nil
}

// UpdateCredentials updates an integration's credentials.
func (r *Repository) UpdateCredentials(ctx context.Context, id string, credentials []byte, tokenExpiresAt *time.Time) error {
	integrationID, err := parseUUID(id)
	if err != nil {
		return err
	}

	var expiresAt pgtype.Timestamptz
	if tokenExpiresAt != nil {
		expiresAt = pgtype.Timestamptz{Time: *tokenExpiresAt, Valid: true}
	}

	return r.queries.UpdateIntegrationCredentials(ctx, sqlc.UpdateIntegrationCredentialsParams{
		ID:             integrationID,
		Credentials:    credentials,
		TokenExpiresAt: expiresAt,
	})
}

// UpdateStatus updates an integration's status.
func (r *Repository) UpdateStatus(ctx context.Context, id, status string) error {
	integrationID, err := parseUUID(id)
	if err != nil {
		return err
	}

	return r.queries.UpdateIntegrationStatus(ctx, sqlc.UpdateIntegrationStatusParams{
		ID:     integrationID,
		Status: status,
	})
}

// Delete deletes an integration.
func (r *Repository) Delete(ctx context.Context, id, storeID string) error {
	integrationID, err := parseUUID(id)
	if err != nil {
		return err
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	return r.queries.DeleteIntegration(ctx, sqlc.DeleteIntegrationParams{
		ID:      integrationID,
		StoreID: sID,
	})
}

// =============================================================================
// INTEGRATION LOGS
// =============================================================================

// CreateLog creates an integration log entry.
func (r *Repository) CreateLog(ctx context.Context, integrationID, entityType, entityID, direction, status string, requestPayload, responsePayload []byte, errorMessage string) error {
	intID, err := parseUUID(integrationID)
	if err != nil {
		return err
	}

	var entID pgtype.UUID
	if entityID != "" {
		entID, err = parseUUID(entityID)
		if err != nil {
			return err
		}
	}

	// Convert []byte to string for JSONB insertion
	reqPayload := "{}"
	if len(requestPayload) > 0 {
		reqPayload = string(requestPayload)
	}
	respPayload := "{}"
	if len(responsePayload) > 0 {
		respPayload = string(responsePayload)
	}

	_, err = r.queries.CreateIntegrationLog(ctx, sqlc.CreateIntegrationLogParams{
		IntegrationID: intID,
		EntityType:    pgtype.Text{String: entityType, Valid: entityType != ""},
		EntityID:      entID,
		Direction:     pgtype.Text{String: direction, Valid: direction != ""},
		Status:        pgtype.Text{String: status, Valid: status != ""},
		Column6:       []byte(reqPayload),
		Column7:       []byte(respPayload),
		ErrorMessage:  pgtype.Text{String: errorMessage, Valid: errorMessage != ""},
	})
	return err
}

// =============================================================================
// WEBHOOK EVENTS
// =============================================================================

// CreateWebhookEvent creates a webhook event record.
func (r *Repository) CreateWebhookEvent(ctx context.Context, input StoreWebhookInput) (*WebhookEventRow, error) {
	intID, err := parseUUID(input.IntegrationID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.CreateWebhookEvent(ctx, sqlc.CreateWebhookEventParams{
		IntegrationID:  intID,
		Provider:       input.Provider,
		EventType:      input.EventType,
		EventID:        pgtype.Text{String: input.EventID, Valid: input.EventID != ""},
		Payload:        input.Payload,
		SignatureValid: pgtype.Bool{Bool: input.SignatureValid, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("creating webhook event: %w", err)
	}

	return r.toWebhookEventRow(row), nil
}

// GetWebhookEventByEventID checks if a webhook event already exists.
func (r *Repository) GetWebhookEventByEventID(ctx context.Context, integrationID, eventID string) (*WebhookEventRow, error) {
	intID, err := parseUUID(integrationID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetWebhookEventByEventID(ctx, sqlc.GetWebhookEventByEventIDParams{
		IntegrationID: intID,
		EventID:       pgtype.Text{String: eventID, Valid: true},
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting webhook event: %w", err)
	}

	return r.toWebhookEventRow(row), nil
}

// MarkWebhookProcessed marks a webhook event as processed.
func (r *Repository) MarkWebhookProcessed(ctx context.Context, id string) error {
	eventID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.MarkWebhookProcessed(ctx, eventID)
}

// MarkWebhookFailed marks a webhook event as failed.
func (r *Repository) MarkWebhookFailed(ctx context.Context, id, errorMessage string) error {
	eventID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.MarkWebhookFailed(ctx, sqlc.MarkWebhookFailedParams{
		ID:           eventID,
		ErrorMessage: pgtype.Text{String: errorMessage, Valid: errorMessage != ""},
	})
}

// =============================================================================
// IDEMPOTENCY REPOSITORY IMPLEMENTATION
// =============================================================================

// IdempotencyRepository implements the idempotency.Repository interface.
type IdempotencyRepository struct {
	queries *sqlc.Queries
}

// NewIdempotencyRepository creates a new idempotency repository.
func NewIdempotencyRepository(queries *sqlc.Queries) *IdempotencyRepository {
	return &IdempotencyRepository{queries: queries}
}

// GetByKey retrieves an idempotency record by key.
func (r *IdempotencyRepository) GetByKey(ctx context.Context, storeID, key string) (*idempotency.Record, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetIdempotencyByKey(ctx, sqlc.GetIdempotencyByKeyParams{
		StoreID:        sID,
		IdempotencyKey: key,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting idempotency key: %w", err)
	}

	return r.toIdempotencyRecord(row), nil
}

// GetByHash retrieves an idempotency record by payload hash.
func (r *IdempotencyRepository) GetByHash(ctx context.Context, storeID, hash string, windowStart time.Time) (*idempotency.Record, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetIdempotencyByHash(ctx, sqlc.GetIdempotencyByHashParams{
		StoreID:     sID,
		RequestHash: pgtype.Text{String: hash, Valid: true},
		CreatedAt:   pgtype.Timestamptz{Time: windowStart, Valid: true},
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting idempotency by hash: %w", err)
	}

	return r.toIdempotencyRecord(row), nil
}

// Create creates a new idempotency record.
func (r *IdempotencyRepository) Create(ctx context.Context, params idempotency.CreateParams) (*idempotency.Record, error) {
	sID, err := parseUUID(params.StoreID)
	if err != nil {
		return nil, err
	}
	intID, err := parseUUID(params.IntegrationID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.CreateIdempotencyKey(ctx, sqlc.CreateIdempotencyKeyParams{
		IdempotencyKey: params.IdempotencyKey,
		StoreID:        sID,
		IntegrationID:  intID,
		Operation:      params.Operation,
		RequestHash:    pgtype.Text{String: params.RequestHash, Valid: params.RequestHash != ""},
		Status:         "pending",
	})
	if err != nil {
		return nil, fmt.Errorf("creating idempotency key: %w", err)
	}

	return r.toIdempotencyRecord(row), nil
}

// Update updates an idempotency record.
func (r *IdempotencyRepository) Update(ctx context.Context, id string, response []byte, status string) error {
	idemID, err := parseUUID(id)
	if err != nil {
		return err
	}

	return r.queries.UpdateIdempotencyKey(ctx, sqlc.UpdateIdempotencyKeyParams{
		ID:              idemID,
		ResponsePayload: response,
		Status:          status,
	})
}

func (r *IdempotencyRepository) toIdempotencyRecord(row sqlc.IdempotencyKey) *idempotency.Record {
	return &idempotency.Record{
		ID:             uuidToString(row.ID),
		IdempotencyKey: row.IdempotencyKey,
		StoreID:        uuidToString(row.StoreID),
		IntegrationID:  uuidToString(row.IntegrationID),
		Operation:      row.Operation,
		RequestHash:    row.RequestHash.String,
		Response:       row.ResponsePayload,
		Status:         row.Status,
		CreatedAt:      row.CreatedAt.Time,
		ExpiresAt:      row.ExpiresAt.Time,
	}
}

// =============================================================================
// HELPERS
// =============================================================================

func (r *Repository) toIntegrationRow(row sqlc.Integration) *IntegrationRow {
	var metadata map[string]any
	if row.Metadata != nil {
		_ = json.Unmarshal(row.Metadata, &metadata)
	}

	var lastSyncedAt *time.Time
	if row.LastSyncedAt.Valid {
		lastSyncedAt = &row.LastSyncedAt.Time
	}

	var tokenExpiresAt *time.Time
	if row.TokenExpiresAt.Valid {
		tokenExpiresAt = &row.TokenExpiresAt.Time
	}

	return &IntegrationRow{
		ID:             uuidToString(row.ID),
		StoreID:        uuidToString(row.StoreID),
		Type:           row.Type,
		Provider:       row.Provider,
		Status:         row.Status,
		Credentials:    row.Credentials,
		TokenExpiresAt: tokenExpiresAt,
		Metadata:       metadata,
		LastSyncedAt:   lastSyncedAt,
		CreatedAt:      row.CreatedAt.Time,
	}
}

func (r *Repository) toWebhookEventRow(row sqlc.WebhookEvent) *WebhookEventRow {
	var signatureValid *bool
	if row.SignatureValid.Valid {
		signatureValid = &row.SignatureValid.Bool
	}

	var processedAt *time.Time
	if row.ProcessedAt.Valid {
		processedAt = &row.ProcessedAt.Time
	}

	return &WebhookEventRow{
		ID:             uuidToString(row.ID),
		IntegrationID:  uuidToString(row.IntegrationID),
		Provider:       row.Provider,
		EventType:      row.EventType,
		EventID:        row.EventID.String,
		Payload:        row.Payload,
		SignatureValid: signatureValid,
		Processed:      row.Processed,
		ProcessedAt:    processedAt,
		ErrorMessage:   row.ErrorMessage.String,
		CreatedAt:      row.CreatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(s); err != nil {
		return pgtype.UUID{}, httpx.ErrUnprocessable(fmt.Sprintf("invalid UUID: %s", s))
	}
	return uuid, nil
}

func uuidToString(uuid pgtype.UUID) string {
	if !uuid.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		uuid.Bytes[0:4],
		uuid.Bytes[4:6],
		uuid.Bytes[6:8],
		uuid.Bytes[8:10],
		uuid.Bytes[10:16])
}
