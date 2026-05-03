package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/query"
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

// GetAnyByType returns the first integration of the given type for a store
// regardless of provider or status. Used to enforce single-instance rules
// (e.g. only one active ERP per store) before insert. Returns nil/nil when
// no row exists. Caller checks the row to decide which provider is already
// connected and surface a friendly error.
func (r *Repository) GetAnyByType(ctx context.Context, storeID, integrationType string) (*IntegrationRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	const q = `
		SELECT id, store_id, type, provider, status, credentials,
		       token_expires_at, metadata, last_synced_at, created_at
		FROM integrations
		WHERE store_id = $1 AND type = $2
		ORDER BY created_at ASC
		LIMIT 1
	`
	var row sqlc.Integration
	scanErr := r.pool.QueryRow(ctx, q, sID, integrationType).Scan(
		&row.ID, &row.StoreID, &row.Type, &row.Provider, &row.Status,
		&row.Credentials, &row.TokenExpiresAt, &row.Metadata,
		&row.LastSyncedAt, &row.CreatedAt,
	)
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return nil, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("checking existing integration: %w", scanErr)
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

// ListByStore lists all integrations for a store with pagination.
func (r *Repository) ListByStore(ctx context.Context, storeID string, pagination query.Pagination) ([]IntegrationRow, int, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, 0, err
	}

	// Get total count
	var total int
	err = r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM integrations WHERE store_id = $1`, sID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting integrations: %w", err)
	}

	rows, err := r.queries.ListIntegrationsByStore(ctx, sID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing integrations: %w", err)
	}

	// Apply pagination in memory (integrations are few per store)
	start := pagination.Offset()
	end := start + pagination.Limit
	if start > len(rows) {
		start = len(rows)
	}
	if end > len(rows) {
		end = len(rows)
	}

	paginatedRows := rows[start:end]
	result := make([]IntegrationRow, len(paginatedRows))
	for i, row := range paginatedRows {
		result[i] = *r.toIntegrationRow(row)
	}

	return result, total, nil
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

// GetByInstagramUserID returns an active Instagram integration by the Instagram user ID stored in metadata.
func (r *Repository) GetByInstagramUserID(ctx context.Context, instagramUserID string) (*IntegrationRow, error) {
	query := `
		SELECT id, store_id, type, provider, status, credentials, token_expires_at, metadata, last_synced_at, created_at
		FROM integrations
		WHERE provider = 'instagram'
		  AND status = 'active'
		  AND metadata->>'instagram_user_id' = $1
		LIMIT 1
	`

	row := r.pool.QueryRow(ctx, query, instagramUserID)

	var id, storeID pgtype.UUID
	var intType, provider, status string
	var credentials []byte
	var tokenExpiresAt pgtype.Timestamptz
	var metadata []byte
	var lastSyncedAt pgtype.Timestamptz
	var createdAt time.Time

	err := row.Scan(&id, &storeID, &intType, &provider, &status, &credentials, &tokenExpiresAt, &metadata, &lastSyncedAt, &createdAt)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil // Not found, return nil without error
		}
		return nil, fmt.Errorf("getting integration by instagram user id: %w", err)
	}

	result := &IntegrationRow{
		ID:          uuidToString(id),
		StoreID:     uuidToString(storeID),
		Type:        intType,
		Provider:    provider,
		Status:      status,
		Credentials: credentials,
		CreatedAt:   createdAt,
	}

	if tokenExpiresAt.Valid {
		result.TokenExpiresAt = &tokenExpiresAt.Time
	}
	if lastSyncedAt.Valid {
		result.LastSyncedAt = &lastSyncedAt.Time
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &result.Metadata)
	}

	return result, nil
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

// UpdateMetadata replaces the metadata JSONB of an integration. Used by the
// admin flow when an integration is (re)configured and the metadata contents
// (e.g. `environment`) may change.
//
// We go through the sqlc-generated query (which types $2 as JSON via
// json.RawMessage) instead of raw pool.Exec — pgx otherwise infers []byte as
// bytea and Postgres rejects it with "invalid input syntax for type json".
func (r *Repository) UpdateMetadata(ctx context.Context, id string, metadata map[string]any) error {
	integrationID, err := parseUUID(id)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshaling metadata: %w", err)
	}
	return r.queries.UpdateIntegrationMetadata(ctx, sqlc.UpdateIntegrationMetadataParams{
		ID:       integrationID,
		Metadata: raw,
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

// ListWithExpiringTokens lists active integrations with tokens expiring before the given time.
// Used by background token refresh worker.
func (r *Repository) ListWithExpiringTokens(ctx context.Context, expiresBefore time.Time) ([]IntegrationRow, error) {
	rows, err := r.queries.ListIntegrationsWithExpiringTokens(ctx, pgtype.Timestamptz{
		Time:  expiresBefore,
		Valid: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing integrations with expiring tokens: %w", err)
	}

	result := make([]IntegrationRow, len(rows))
	for i, row := range rows {
		result[i] = *r.toIntegrationRow(row)
	}
	return result, nil
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

	// Convert []byte to valid JSON for JSONB insertion.
	// If payload is not valid JSON, wrap it as a JSON string.
	reqPayload := json.RawMessage(ensureValidJSON(requestPayload))
	respPayload := json.RawMessage(ensureValidJSON(responsePayload))

	_, err = r.queries.CreateIntegrationLog(ctx, sqlc.CreateIntegrationLogParams{
		IntegrationID:   intID,
		EntityType:      pgtype.Text{String: entityType, Valid: entityType != ""},
		EntityID:        entID,
		Direction:       pgtype.Text{String: direction, Valid: direction != ""},
		Status:          pgtype.Text{String: status, Valid: status != ""},
		RequestPayload:  reqPayload,
		ResponsePayload: respPayload,
		ErrorMessage:    pgtype.Text{String: errorMessage, Valid: errorMessage != ""},
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

	// Use raw SQL to insert with explicit ::jsonb cast.
	// SQLC generates Payload as []byte which pgx sends as bytea, incompatible with jsonb columns.
	query := `
		INSERT INTO webhook_events (integration_id, provider, event_type, event_id, payload, signature_valid)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		RETURNING id, integration_id, provider, event_type, event_id, payload, signature_valid, processed, processed_at, error_message, created_at
	`

	eventID := pgtype.Text{String: input.EventID, Valid: input.EventID != ""}
	sigValid := pgtype.Bool{Bool: input.SignatureValid, Valid: true}

	var row sqlc.WebhookEvent
	err = r.pool.QueryRow(ctx, query, intID, input.Provider, input.EventType, eventID, string(input.Payload), sigValid).Scan(
		&row.ID,
		&row.IntegrationID,
		&row.Provider,
		&row.EventType,
		&row.EventID,
		&row.Payload,
		&row.SignatureValid,
		&row.Processed,
		&row.ProcessedAt,
		&row.ErrorMessage,
		&row.CreatedAt,
	)
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
// INSTAGRAM LIVE SESSION OPERATIONS
// =============================================================================

// IncrementLiveSessionComments increments the total_comments counter for a live session.
func (r *Repository) IncrementLiveSessionComments(ctx context.Context, sessionID string) error {
	id, err := parseUUID(sessionID)
	if err != nil {
		return err
	}
	return r.queries.IncrementLiveSessionComments(ctx, id)
}

// IncrementLiveEventOrders increments the total_orders counter for a live event.
func (r *Repository) IncrementLiveEventOrders(ctx context.Context, eventID string) error {
	id, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	return r.queries.IncrementLiveEventOrders(ctx, id)
}

// ProductRow represents a product for keyword matching and stock operations.
type ProductRow struct {
	ID         string
	Keyword    string
	Price      int64
	Stock      int
	ExternalID string
	Name       string
}

// GetProductByKeyword finds an active product by keyword in a store.
func (r *Repository) GetProductByKeyword(ctx context.Context, storeID, keyword string) (*ProductRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetProductByKeyword(ctx, sqlc.GetProductByKeywordParams{
		StoreID: sID,
		Keyword: keyword,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting product by keyword: %w", err)
	}

	var price int64
	if row.Price.Valid {
		price = row.Price.Int64
	}

	var stock int
	if row.Stock.Valid {
		stock = int(row.Stock.Int32)
	}

	var externalID string
	if row.ExternalID.Valid {
		externalID = row.ExternalID.String
	}

	return &ProductRow{
		ID:         uuidToString(row.ID),
		Keyword:    row.Keyword,
		Price:      price,
		Stock:      stock,
		ExternalID: externalID,
		Name:       row.Name,
	}, nil
}

// GetProductByID retrieves a product by its UUID.
func (r *Repository) GetProductByID(ctx context.Context, storeID, productID string) (*ProductRow, error) {
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetProductByID(ctx, sqlc.GetProductByIDParams{
		ID:      pID,
		StoreID: sID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting product by ID: %w", err)
	}

	var price int64
	if row.Price.Valid {
		price = row.Price.Int64
	}

	var stock int
	if row.Stock.Valid {
		stock = int(row.Stock.Int32)
	}

	var externalID string
	if row.ExternalID.Valid {
		externalID = row.ExternalID.String
	}

	return &ProductRow{
		ID:         uuidToString(row.ID),
		Keyword:    row.Keyword,
		Price:      price,
		Stock:      stock,
		ExternalID: externalID,
		Name:       row.Name,
	}, nil
}

// =============================================================================
// STOCK OPERATIONS
// =============================================================================

// DecrementProductStock atomically decrements stock. Returns nil if insufficient stock.
func (r *Repository) DecrementProductStock(ctx context.Context, productID string, quantity int) error {
	id, err := parseUUID(productID)
	if err != nil {
		return err
	}
	_, err = r.queries.DecrementProductStock(ctx, sqlc.DecrementProductStockParams{
		ID:    id,
		Stock: pgtype.Int4{Int32: int32(quantity), Valid: true},
	})
	return err
}

// IncrementProductStock releases reserved stock back to product.
func (r *Repository) IncrementProductStock(ctx context.Context, productID string, quantity int) error {
	id, err := parseUUID(productID)
	if err != nil {
		return err
	}
	_, err = r.queries.IncrementProductStock(ctx, sqlc.IncrementProductStockParams{
		ID:    id,
		Stock: pgtype.Int4{Int32: int32(quantity), Valid: true},
	})
	return err
}

// =============================================================================
// LIVE COMMENTS
// =============================================================================

// CreateLiveComment saves a live comment to the database.
func (r *Repository) CreateLiveComment(ctx context.Context, params CreateLiveCommentParams) (string, error) {
	sessionID, err := parseUUID(params.SessionID)
	if err != nil {
		return "", err
	}
	eventID, err := parseUUID(params.EventID)
	if err != nil {
		return "", err
	}

	var matchedProductID pgtype.UUID
	if params.MatchedProductID != "" {
		matchedProductID, err = parseUUID(params.MatchedProductID)
		if err != nil {
			return "", err
		}
	}

	row, err := r.queries.CreateLiveComment(ctx, sqlc.CreateLiveCommentParams{
		SessionID:         sessionID,
		EventID:           eventID,
		Platform:          params.Platform,
		PlatformCommentID: params.PlatformCommentID,
		PlatformUserID:    params.PlatformUserID,
		PlatformHandle:    params.PlatformHandle,
		Text:              params.Text,
		HasPurchaseIntent: pgtype.Bool{Bool: params.HasPurchaseIntent, Valid: true},
		MatchedProductID:  matchedProductID,
		MatchedQuantity:   pgtype.Int4{Int32: int32(params.MatchedQuantity), Valid: params.MatchedQuantity > 0},
		Result:            pgtype.Text{String: params.Result, Valid: params.Result != ""},
	})
	if err != nil {
		return "", fmt.Errorf("creating live comment: %w", err)
	}
	return uuidToString(row.ID), nil
}

// UpdateLiveCommentResult updates the result of processing a live comment.
func (r *Repository) UpdateLiveCommentResult(ctx context.Context, commentID string, hasPurchaseIntent bool, matchedProductID string, matchedQuantity int, result string) error {
	id, err := parseUUID(commentID)
	if err != nil {
		return err
	}

	var productID pgtype.UUID
	if matchedProductID != "" {
		productID, err = parseUUID(matchedProductID)
		if err != nil {
			return err
		}
	}

	return r.queries.UpdateLiveCommentResult(ctx, sqlc.UpdateLiveCommentResultParams{
		ID:                id,
		HasPurchaseIntent: pgtype.Bool{Bool: hasPurchaseIntent, Valid: true},
		MatchedProductID:  productID,
		MatchedQuantity:   pgtype.Int4{Int32: int32(matchedQuantity), Valid: matchedQuantity > 0},
		Result:            pgtype.Text{String: result, Valid: result != ""},
	})
}

// CreateLiveCommentParams holds parameters for creating a live comment.
type CreateLiveCommentParams struct {
	SessionID         string
	EventID           string
	Platform          string
	PlatformCommentID string
	PlatformUserID    string
	PlatformHandle    string
	Text              string
	HasPurchaseIntent bool
	MatchedProductID  string
	MatchedQuantity   int
	Result            string
}

// =============================================================================
// WAITLIST OPERATIONS
// =============================================================================

// GetNextWaitlistPosition returns the next position for a product waitlist.
func (r *Repository) GetNextWaitlistPosition(ctx context.Context, eventID, productID string) (int, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	pos, err := r.queries.GetNextWaitlistPosition(ctx, sqlc.GetNextWaitlistPositionParams{
		EventID:   eID,
		ProductID: pID,
	})
	return int(pos), err
}

// CreateWaitlistItem creates a waitlist entry.
func (r *Repository) CreateWaitlistItem(ctx context.Context, params CreateWaitlistItemParams) (string, error) {
	eID, err := parseUUID(params.EventID)
	if err != nil {
		return "", err
	}
	pID, err := parseUUID(params.ProductID)
	if err != nil {
		return "", err
	}
	row, err := r.queries.CreateWaitlistItem(ctx, sqlc.CreateWaitlistItemParams{
		EventID:        eID,
		ProductID:      pID,
		PlatformUserID: params.PlatformUserID,
		PlatformHandle: params.PlatformHandle,
		Quantity:       int32(params.Quantity),
		Position:       int32(params.Position),
	})
	if err != nil {
		return "", fmt.Errorf("creating waitlist item: %w", err)
	}
	return uuidToString(row.ID), nil
}

// GetWaitlistItemByEventUserProduct checks if a user already has a waitlist entry for this product.
func (r *Repository) GetWaitlistItemByEventUserProduct(ctx context.Context, eventID, platformUserID, productID string) (bool, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return false, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return false, err
	}
	_, err = r.queries.GetWaitlistItemByEventUserProduct(ctx, sqlc.GetWaitlistItemByEventUserProductParams{
		EventID:        eID,
		PlatformUserID: platformUserID,
		ProductID:      pID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetFirstWaitingByProduct gets the first waiting person in the queue.
func (r *Repository) GetFirstWaitingByProduct(ctx context.Context, eventID, productID string) (*WaitlistItemRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetFirstWaitingByProduct(ctx, sqlc.GetFirstWaitingByProductParams{
		EventID:   eID,
		ProductID: pID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return &WaitlistItemRow{
		ID:             uuidToString(row.ID),
		EventID:        uuidToString(row.EventID),
		ProductID:      uuidToString(row.ProductID),
		PlatformUserID: row.PlatformUserID,
		PlatformHandle: row.PlatformHandle,
		Quantity:       int(row.Quantity),
		Position:       int(row.Position),
		Status:         row.Status,
	}, nil
}

// UpdateWaitlistItemStatus updates waitlist item status and timestamps.
func (r *Repository) UpdateWaitlistItemStatus(ctx context.Context, id, status string, notifiedAt, fulfilledAt, expiresAt *time.Time) error {
	itemID, err := parseUUID(id)
	if err != nil {
		return err
	}
	var na, fa, ea pgtype.Timestamptz
	if notifiedAt != nil {
		na = pgtype.Timestamptz{Time: *notifiedAt, Valid: true}
	}
	if fulfilledAt != nil {
		fa = pgtype.Timestamptz{Time: *fulfilledAt, Valid: true}
	}
	if expiresAt != nil {
		ea = pgtype.Timestamptz{Time: *expiresAt, Valid: true}
	}
	return r.queries.UpdateWaitlistItemStatus(ctx, sqlc.UpdateWaitlistItemStatusParams{
		ID:          itemID,
		Status:      status,
		NotifiedAt:  na,
		FulfilledAt: fa,
		ExpiresAt:   ea,
	})
}

// CreateWaitlistItemParams holds parameters for creating a waitlist item.
type CreateWaitlistItemParams struct {
	EventID        string
	ProductID      string
	PlatformUserID string
	PlatformHandle string
	Quantity       int
	Position       int
}

// WaitlistItemRow represents a waitlist item.
type WaitlistItemRow struct {
	ID             string
	EventID        string
	ProductID      string
	PlatformUserID string
	PlatformHandle string
	Quantity       int
	Position       int
	Status         string
}

// =============================================================================
// ERP CONTACTS
// =============================================================================

// GetERPContact gets a cached ERP contact by store, integration, and platform user.
func (r *Repository) GetERPContact(ctx context.Context, storeID, integrationID, platformUserID string) (string, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}
	iID, err := parseUUID(integrationID)
	if err != nil {
		return "", err
	}
	row, err := r.queries.GetERPContact(ctx, sqlc.GetERPContactParams{
		StoreID:        sID,
		IntegrationID:  iID,
		PlatformUserID: platformUserID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", nil
		}
		return "", err
	}
	return row.ExternalContactID, nil
}

// UpsertERPContact creates or updates an ERP contact cache entry.
func (r *Repository) UpsertERPContact(ctx context.Context, storeID, integrationID, platformUserID, platformHandle, externalContactID string) error {
	sID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	iID, err := parseUUID(integrationID)
	if err != nil {
		return err
	}
	_, err = r.queries.UpsertERPContact(ctx, sqlc.UpsertERPContactParams{
		StoreID:           sID,
		IntegrationID:     iID,
		PlatformUserID:    platformUserID,
		PlatformHandle:    platformHandle,
		ExternalContactID: externalContactID,
	})
	return err
}

// =============================================================================
// CART ERP OPERATIONS
// =============================================================================

// UpdateCartExternalOrderID sets the external ERP order ID on a cart.
func (r *Repository) UpdateCartExternalOrderID(ctx context.Context, cartID, externalOrderID string) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.UpdateCartExternalOrderID(ctx, sqlc.UpdateCartExternalOrderIDParams{
		ID:              id,
		ExternalOrderID: pgtype.Text{String: externalOrderID, Valid: externalOrderID != ""},
	})
}

// NonWaitlistedCartItem represents a cart item that is not waitlisted, with product info.
type NonWaitlistedCartItem struct {
	ID                string
	CartID            string
	ProductID         string
	Quantity          int
	UnitPrice         int64
	ProductName       string
	ProductExternalID string
	ProductKeyword    string
}

// ListNonWaitlistedCartItems returns non-waitlisted cart items with product external_id for ERP sync.
func (r *Repository) ListNonWaitlistedCartItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListNonWaitlistedCartItems(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("listing non-waitlisted cart items: %w", err)
	}
	items := make([]NonWaitlistedCartItem, len(rows))
	for i, row := range rows {
		var extID string
		if row.ProductExternalID.Valid {
			extID = row.ProductExternalID.String
		}
		items[i] = NonWaitlistedCartItem{
			ID:                uuidToString(row.ID),
			CartID:            uuidToString(row.CartID),
			ProductID:         uuidToString(row.ProductID),
			Quantity:          int(row.Quantity),
			UnitPrice:         row.UnitPrice.Int64,
			ProductName:       row.ProductName,
			ProductExternalID: extID,
			ProductKeyword:    row.ProductKeyword,
		}
	}
	return items, nil
}

// ExpiredCartRow represents an expired cart with store_id for ERP operations.
type ExpiredCartRow struct {
	ID              string
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	ExternalOrderID string
	StoreID         string
}

// ListExpiredCartsByEventAndProduct returns expired carts for a specific event/product.
func (r *Repository) ListExpiredCartsByEventAndProduct(ctx context.Context, eventID, productID string) ([]ExpiredCartRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListExpiredCartsByEventAndProduct(ctx, sqlc.ListExpiredCartsByEventAndProductParams{
		EventID:   eID,
		ProductID: pID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing expired carts: %w", err)
	}
	carts := make([]ExpiredCartRow, len(rows))
	for i, row := range rows {
		var extOrderID string
		if row.ExternalOrderID.Valid {
			extOrderID = row.ExternalOrderID.String
		}
		carts[i] = ExpiredCartRow{
			ID:              uuidToString(row.ID),
			EventID:         uuidToString(row.EventID),
			PlatformUserID:  row.PlatformUserID,
			PlatformHandle:  row.PlatformHandle,
			ExternalOrderID: extOrderID,
			StoreID:         uuidToString(row.StoreID),
		}
	}
	return carts, nil
}

// UpdateCartStatus updates a cart's status (e.g., "expired").
func (r *Repository) UpdateCartStatus(ctx context.Context, cartID, status string) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	if _, err := r.queries.UpdateCartStatus(ctx, sqlc.UpdateCartStatusParams{
		ID:     id,
		Status: status,
	}); err != nil {
		return fmt.Errorf("updating cart status: %w", err)
	}
	return nil
}

// GetCartByEventAndUser gets a cart for a specific event and user.
func (r *Repository) GetCartByEventAndUser(ctx context.Context, eventID, platformUserID string) (*CartRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetCartByEventAndUser(ctx, sqlc.GetCartByEventAndUserParams{
		EventID:        eID,
		PlatformUserID: platformUserID,
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	var extOrderID string
	if row.ExternalOrderID.Valid {
		extOrderID = row.ExternalOrderID.String
	}
	return &CartRow{
		ID:              uuidToString(row.ID),
		EventID:         uuidToString(row.EventID),
		PlatformUserID:  row.PlatformUserID,
		PlatformHandle:  row.PlatformHandle,
		ExternalOrderID: extOrderID,
		CreatedAt:       row.CreatedAt.Time,
	}, nil
}

// CartRow represents a cart for ERP operations.
type CartRow struct {
	ID              string
	EventID         string
	StoreID         string
	PlatformUserID  string
	PlatformHandle  string
	ExternalOrderID string
	CreatedAt       time.Time

	// Populated by GetCartForPaidOrder — needed when creating a paid ERP order.
	CustomerEmail    string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string
	ShippingAddress  json.RawMessage

	// Shipping selection persisted at checkout time.
	ShippingServiceName string
	ShippingCarrier     string
	ShippingRealCost    int64
	ShippingDeadline    int
}

// GetCartForPaidOrder loads a cart by ID with customer/shipping data plus the
// store ID resolved from the event, so the paid-order ERP flow has everything
// it needs without an extra join.
func (r *Repository) GetCartForPaidOrder(ctx context.Context, cartID string) (*CartRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	cart, err := r.queries.GetCartByID(ctx, cID)
	if err != nil {
		return nil, fmt.Errorf("getting cart: %w", err)
	}

	event, err := r.queries.GetLiveEventByID(ctx, cart.EventID)
	if err != nil {
		return nil, fmt.Errorf("getting live event for cart: %w", err)
	}

	row := &CartRow{
		ID:              uuidToString(cart.ID),
		EventID:         uuidToString(cart.EventID),
		StoreID:         uuidToString(event.StoreID),
		PlatformUserID:  cart.PlatformUserID,
		PlatformHandle:  cart.PlatformHandle,
		ExternalOrderID: cart.ExternalOrderID.String,
		CreatedAt:       cart.CreatedAt.Time,
		ShippingAddress: cart.ShippingAddress,
	}
	if cart.CustomerEmail.Valid {
		row.CustomerEmail = cart.CustomerEmail.String
	}
	if cart.CustomerName.Valid {
		row.CustomerName = cart.CustomerName.String
	}
	if cart.CustomerDocument.Valid {
		row.CustomerDocument = cart.CustomerDocument.String
	}
	if cart.CustomerPhone.Valid {
		row.CustomerPhone = cart.CustomerPhone.String
	}

	// Load shipping selection separately (not part of the sqlc cart model yet).
	var (
		shipName     pgtype.Text
		shipCarrier  pgtype.Text
		shipRealCost pgtype.Int8
		shipDeadline pgtype.Int4
	)
	err = r.pool.QueryRow(ctx, `
		SELECT shipping_service_name, shipping_carrier, shipping_cost_real_cents, shipping_deadline_days
		FROM carts WHERE id = $1
	`, cID).Scan(&shipName, &shipCarrier, &shipRealCost, &shipDeadline)
	if err == nil {
		row.ShippingServiceName = shipName.String
		row.ShippingCarrier = shipCarrier.String
		row.ShippingRealCost = shipRealCost.Int64
		row.ShippingDeadline = int(shipDeadline.Int32)
	}

	return row, nil
}

// UpdateCartItemWaitlistedQuantity updates the waitlisted quantity of a cart item.
func (r *Repository) UpdateCartItemWaitlistedQuantity(ctx context.Context, cartID, productID string, waitlistedQuantity int) error {
	cID, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return err
	}
	return r.queries.UpdateCartItemWaitlistedQuantity(ctx, sqlc.UpdateCartItemWaitlistedQuantityParams{
		CartID:             cID,
		ProductID:          pID,
		WaitlistedQuantity: int32(waitlistedQuantity),
	})
}

// =============================================================================
// CART PAYMENT OPERATIONS
// =============================================================================

// UpdateCartPaymentStatus updates the payment status of a cart. The
// payment-provider ID (MP/Pagar.me) is stored in checkout_id; external_order_id
// is reserved for the ERP order ID written by finalizeCartERPOrder. Mixing the
// two breaks paid-order idempotency — every paid cart was being skipped because
// finalize saw a populated external_order_id and assumed the ERP order had
// already been created.
func (r *Repository) UpdateCartPaymentStatus(ctx context.Context, cartID string, paymentStatus string, paymentID string, paidAt *time.Time, paymentMethod string) error {
	cID, err := parseUUID(cartID)
	if err != nil {
		return err
	}

	var paidAtPg pgtype.Timestamptz
	if paidAt != nil {
		paidAtPg = pgtype.Timestamptz{Time: *paidAt, Valid: true}
	}

	_, err = r.queries.UpdateCartPayment(ctx, sqlc.UpdateCartPaymentParams{
		ID:            cID,
		PaymentStatus: pgtype.Text{String: paymentStatus, Valid: true},
		CheckoutID:    pgtype.Text{String: paymentID, Valid: paymentID != ""},
		PaidAt:        paidAtPg,
		PaymentMethod: pgtype.Text{String: paymentMethod, Valid: paymentMethod != ""},
	})
	if err != nil {
		return fmt.Errorf("updating cart payment status: %w", err)
	}
	return nil
}

// =============================================================================
// OAUTH STATES (PKCE)
// =============================================================================

// OAuthStateRow represents an OAuth state record.
type OAuthStateRow struct {
	State        string
	StoreID      pgtype.UUID
	Provider     string
	CodeVerifier string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// CreateOAuthState stores an OAuth state with PKCE code_verifier.
func (r *Repository) CreateOAuthState(ctx context.Context, state, storeID, provider, codeVerifier string) error {
	sID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	return r.queries.CreateOAuthState(ctx, sqlc.CreateOAuthStateParams{
		State:        state,
		StoreID:      sID,
		Provider:     provider,
		CodeVerifier: codeVerifier,
	})
}

// GetOAuthState retrieves an OAuth state if not expired.
func (r *Repository) GetOAuthState(ctx context.Context, state string) (*OAuthStateRow, error) {
	row, err := r.queries.GetOAuthState(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("getting OAuth state: %w", err)
	}
	return &OAuthStateRow{
		State:        row.State,
		StoreID:      row.StoreID,
		Provider:     row.Provider,
		CodeVerifier: row.CodeVerifier,
		CreatedAt:    row.CreatedAt.Time,
		ExpiresAt:    row.ExpiresAt.Time,
	}, nil
}

// DeleteOAuthState removes an OAuth state after use.
func (r *Repository) DeleteOAuthState(ctx context.Context, state string) error {
	return r.queries.DeleteOAuthState(ctx, state)
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

// ensureValidJSON returns the payload as-is if it's valid JSON,
// otherwise wraps it as a JSON string. Returns "{}" for nil/empty input.
func ensureValidJSON(data []byte) string {
	if len(data) == 0 {
		return "{}"
	}
	if json.Valid(data) {
		return string(data)
	}
	// Wrap non-JSON content as a JSON string value
	wrapped, _ := json.Marshal(string(data))
	return string(wrapped)
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

// ListCartsByEventForERP returns carts for an event that are in checkout status (finalized).
func (r *Repository) ListCartsByEventForERP(ctx context.Context, eventID string) ([]CartRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListCartsByEvent(ctx, eID)
	if err != nil {
		return nil, err
	}
	var result []CartRow
	for _, row := range rows {
		if row.Status != "checkout" {
			continue
		}
		var extOrderID string
		if row.ExternalOrderID.Valid {
			extOrderID = row.ExternalOrderID.String
		}
		result = append(result, CartRow{
			ID:              uuidToString(row.ID),
			EventID:         uuidToString(row.EventID),
			PlatformUserID:  row.PlatformUserID,
			PlatformHandle:  row.PlatformHandle,
			ExternalOrderID: extOrderID,
			CreatedAt:       row.CreatedAt.Time,
		})
	}
	return result, nil
}

// =============================================================================
// STOCK RESERVATIONS
// =============================================================================

// StockReservationRow represents a stock reservation for ERP operations.
type StockReservationRow struct {
	ID                string
	EventID           string
	CartID            string
	ProductID         string
	ExternalProductID string
	Quantity          int
	ERPMovementID     string
	Status            string
	CreatedAt         time.Time
}

// CreateStockReservationParams holds params for creating a stock reservation.
type CreateStockReservationParams struct {
	EventID           string
	CartID            string
	ProductID         string
	ExternalProductID string
	Quantity          int
	ERPMovementID     string
}

// CreateStockReservation creates a stock reservation record.
func (r *Repository) CreateStockReservation(ctx context.Context, params CreateStockReservationParams) (*StockReservationRow, error) {
	eventID, err := parseUUID(params.EventID)
	if err != nil {
		return nil, err
	}
	cartID, err := parseUUID(params.CartID)
	if err != nil {
		return nil, err
	}
	productID, err := parseUUID(params.ProductID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.CreateStockReservation(ctx, sqlc.CreateStockReservationParams{
		EventID:           eventID,
		CartID:            cartID,
		ProductID:         productID,
		ExternalProductID: params.ExternalProductID,
		Quantity:          int32(params.Quantity),
		ErpMovementID:     pgtype.Text{String: params.ERPMovementID, Valid: params.ERPMovementID != ""},
	})
	if err != nil {
		return nil, fmt.Errorf("creating stock reservation: %w", err)
	}

	return &StockReservationRow{
		ID:                uuidToString(row.ID),
		EventID:           uuidToString(row.EventID),
		CartID:            uuidToString(row.CartID),
		ProductID:         uuidToString(row.ProductID),
		ExternalProductID: row.ExternalProductID,
		Quantity:          int(row.Quantity),
		ERPMovementID:     row.ErpMovementID.String,
		Status:            row.Status,
		CreatedAt:         row.CreatedAt.Time,
	}, nil
}

// ListActiveReservationsByEvent returns all active reservations for an event.
func (r *Repository) ListActiveReservationsByEvent(ctx context.Context, eventID string) ([]StockReservationRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListActiveReservationsByEvent(ctx, eID)
	if err != nil {
		return nil, fmt.Errorf("listing active reservations by event: %w", err)
	}
	result := make([]StockReservationRow, len(rows))
	for i, row := range rows {
		result[i] = StockReservationRow{
			ID:                uuidToString(row.ID),
			EventID:           uuidToString(row.EventID),
			CartID:            uuidToString(row.CartID),
			ProductID:         uuidToString(row.ProductID),
			ExternalProductID: row.ExternalProductID,
			Quantity:          int(row.Quantity),
			ERPMovementID:     row.ErpMovementID.String,
			Status:            row.Status,
			CreatedAt:         row.CreatedAt.Time,
		}
	}
	return result, nil
}

// ListActiveReservationsByCartAndProduct returns active reservations for a cart+product.
func (r *Repository) ListActiveReservationsByCartAndProduct(ctx context.Context, cartID, productID string) ([]StockReservationRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListActiveReservationsByCartAndProduct(ctx, sqlc.ListActiveReservationsByCartAndProductParams{
		CartID:    cID,
		ProductID: pID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing active reservations by cart and product: %w", err)
	}
	result := make([]StockReservationRow, len(rows))
	for i, row := range rows {
		result[i] = StockReservationRow{
			ID:                uuidToString(row.ID),
			EventID:           uuidToString(row.EventID),
			CartID:            uuidToString(row.CartID),
			ProductID:         uuidToString(row.ProductID),
			ExternalProductID: row.ExternalProductID,
			Quantity:          int(row.Quantity),
			ERPMovementID:     row.ErpMovementID.String,
			Status:            row.Status,
			CreatedAt:         row.CreatedAt.Time,
		}
	}
	return result, nil
}

// ListActiveReservationsByCart returns all active reservations for a cart.
// Used by the payment-confirmed flow to reverse Tiny saída-manual entries before
// creating the final sales order.
func (r *Repository) ListActiveReservationsByCart(ctx context.Context, cartID string) ([]StockReservationRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListActiveReservationsByCart(ctx, cID)
	if err != nil {
		return nil, fmt.Errorf("listing active reservations by cart: %w", err)
	}
	result := make([]StockReservationRow, len(rows))
	for i, row := range rows {
		result[i] = StockReservationRow{
			ID:                uuidToString(row.ID),
			EventID:           uuidToString(row.EventID),
			CartID:            uuidToString(row.CartID),
			ProductID:         uuidToString(row.ProductID),
			ExternalProductID: row.ExternalProductID,
			Quantity:          int(row.Quantity),
			ERPMovementID:     row.ErpMovementID.String,
			Status:            row.Status,
			CreatedAt:         row.CreatedAt.Time,
		}
	}
	return result, nil
}

// ReverseReservationsByCart marks all active reservations for a cart as reversed.
func (r *Repository) ReverseReservationsByCart(ctx context.Context, cartID string) error {
	cID, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.ReverseReservationsByCart(ctx, cID)
}

// ReverseReservationsByCartAndProduct marks active reservations for a specific cart+product as reversed.
func (r *Repository) ReverseReservationsByCartAndProduct(ctx context.Context, cartID, productID string) error {
	cID, err := parseUUID(cartID)
	if err != nil {
		return fmt.Errorf("parsing cart ID: %w", err)
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return fmt.Errorf("parsing product ID: %w", err)
	}
	return r.queries.ReverseReservationsByCartAndProduct(ctx, sqlc.ReverseReservationsByCartAndProductParams{
		CartID:    cID,
		ProductID: pID,
	})
}

// AdjustActiveReservationQuantity bumps the quantity (positive or negative)
// on the active reservation row for a (cart, product). erpMovementID is the
// ID of the new ERP movement that produced the delta — empty leaves the
// existing one in place.
func (r *Repository) AdjustActiveReservationQuantity(ctx context.Context, cartID, productID string, delta int, erpMovementID string) (*StockReservationRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, fmt.Errorf("parsing cart ID: %w", err)
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, fmt.Errorf("parsing product ID: %w", err)
	}
	row, err := r.queries.AdjustActiveReservationQuantity(ctx, sqlc.AdjustActiveReservationQuantityParams{
		CartID:        cID,
		ProductID:     pID,
		DeltaQty:      int32(delta),
		ErpMovementID: erpMovementID,
	})
	if err != nil {
		return nil, err
	}
	return &StockReservationRow{
		ID:                uuidToString(row.ID),
		EventID:           uuidToString(row.EventID),
		CartID:            uuidToString(row.CartID),
		ProductID:         uuidToString(row.ProductID),
		ExternalProductID: row.ExternalProductID,
		Quantity:          int(row.Quantity),
		ERPMovementID:     row.ErpMovementID.String,
		Status:            row.Status,
		CreatedAt:         row.CreatedAt.Time,
	}, nil
}

// ConvertReservationsByEvent marks all active reservations for an event as converted.
func (r *Repository) ConvertReservationsByEvent(ctx context.Context, eventID string) error {
	eID, err := parseUUID(eventID)
	if err != nil {
		return err
	}
	return r.queries.ConvertReservationsByEvent(ctx, eID)
}

// HasActiveEventForProduct checks if a product has active reservations in a running event.
func (r *Repository) HasActiveEventForProduct(ctx context.Context, externalProductID string) (bool, error) {
	return r.queries.HasActiveEventForProduct(ctx, externalProductID)
}

// StoreInfo contains minimal store information needed for notifications.
type StoreInfo struct {
	Name                  string
	CartExpirationMinutes int
	MaxQuantityPerItem    int
}

// StoreShippingDefaults are the merchant-configured fallback dimensions used
// when an ERP-imported product carries weight only (e.g. Tiny camisetas).
// All four fields must be positive for the fallback to be enabled — any zero
// disables it.
type StoreShippingDefaults struct {
	WeightGrams   int
	HeightCm      int
	WidthCm       int
	LengthCm      int
	PackageFormat string
}

// IsUsableForDimensionFallback reports whether the merchant configured all
// three default dimensions (height/width/length). Default weight is optional —
// the ERP usually supplies the real weight.
func (d StoreShippingDefaults) IsUsableForDimensionFallback() bool {
	return d.HeightCm > 0 && d.WidthCm > 0 && d.LengthCm > 0
}

// GetStoreShippingDefaults returns the merchant-configured shipping defaults
// for a store. Returns a zero-value StoreShippingDefaults when the store is
// missing or has no defaults configured (no error).
func (r *Repository) GetStoreShippingDefaults(ctx context.Context, storeID string) (StoreShippingDefaults, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return StoreShippingDefaults{}, err
	}
	var d StoreShippingDefaults
	const q = `SELECT default_package_weight_grams, default_package_format,
		default_height_cm, default_width_cm, default_length_cm
		FROM stores WHERE id = $1`
	var weightGrams int32
	var format pgtype.Text
	var h, w, l pgtype.Int4
	if err := r.pool.QueryRow(ctx, q, uid).Scan(&weightGrams, &format, &h, &w, &l); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StoreShippingDefaults{}, nil
		}
		return StoreShippingDefaults{}, fmt.Errorf("loading store shipping defaults: %w", err)
	}
	d.WeightGrams = int(weightGrams)
	if format.Valid {
		d.PackageFormat = format.String
	}
	if h.Valid {
		d.HeightCm = int(h.Int32)
	}
	if w.Valid {
		d.WidthCm = int(w.Int32)
	}
	if l.Valid {
		d.LengthCm = int(l.Int32)
	}
	return d, nil
}

// GetStoreInfo returns minimal store information for notifications.
func (r *Repository) GetStoreInfo(ctx context.Context, storeID string) (*StoreInfo, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.queries.GetStoreNameByID(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("getting store info: %w", err)
	}

	return &StoreInfo{
		Name:                  row.Name,
		CartExpirationMinutes: int(row.CartExpirationMinutes),
		MaxQuantityPerItem:    int(row.CartMaxQuantityPerItem),
	}, nil
}

// GetProductQuantityInUserCart returns the current quantity of a specific product in a user's cart.
// Returns 0 if no cart exists or if the product is not in the cart.
func (r *Repository) GetProductQuantityInUserCart(ctx context.Context, eventID, platformUserID, productID string) (int, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}

	qty, err := r.queries.GetProductQuantityInUserCart(ctx, sqlc.GetProductQuantityInUserCartParams{
		EventID:        eventUID,
		PlatformUserID: platformUserID,
		ProductID:      productUID,
	})
	if err != nil {
		// No cart or no item - return 0
		return 0, nil
	}

	return int(qty), nil
}
