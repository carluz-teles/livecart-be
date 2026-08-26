package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/inventory"
	"livecart/apps/api/internal/live"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/dbtx"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/query"
)

// Repository handles database operations for integrations.
type Repository struct {
	queries *sqlc.Queries
	pool    *pgxpool.Pool
	// lockSlots limita quantas goroutines podem SEGURAR o advisory lock de
	// finalização ao mesmo tempo. Ver AcquireCartFinalisationLock.
	lockSlots chan struct{}
}

// NewRepository creates a new integration repository.
func NewRepository(queries *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{
		queries:   queries,
		pool:      pool,
		lockSlots: make(chan struct{}, cartFinalisationLockSlots(pool)),
	}
}

// cartFinalisationLockSlots deriva o teto de detentores simultâneos do advisory
// lock a partir do tamanho do pool: METADE dele.
//
// Metade e não "MaxConns-1" porque a conta não é só de deadlock. Cada detentor
// prende uma conexão e pede no máximo mais UMA por vez (as queries sob o lock
// são sequenciais; as que abrem transação usam qtx e não o pool). Com N ≤
// MaxConns/2, os N detentores cabem com folga junto das conexões que estão
// pedindo — e sobra metade do pool para o resto da API, que compartilha o MESMO
// pool. Deixar MaxConns-1 também não travaria, mas jogaria todo handler HTTP
// para cima de uma única conexão livre: deixa de ser deadlock e vira apagão.
func cartFinalisationLockSlots(pool *pgxpool.Pool) int {
	maxConns := 4
	if pool != nil && pool.Config() != nil && pool.Config().MaxConns > 0 {
		maxConns = int(pool.Config().MaxConns)
	}
	if slots := maxConns / 2; slots > 0 {
		return slots
	}
	return 1
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

// GetByInstagramUserID returns an active Instagram integration by the Instagram
// account ID that the webhook carries in entry.id.
//
// Casa contra os DOIS ids porque a Meta tem dois e nós já gravamos o errado: o
// da troca do código é app-scoped (28139…) e o da conta profissional é o que
// aparece no webhook (17841…). Toda integração conectada antes da correção tem
// só o app-scoped gravado, e sem este OR ela continuaria sem resolver até o
// lojista reconectar — ou seja, a correção do código não chegaria em ninguém.
func (r *Repository) GetByInstagramUserID(ctx context.Context, instagramUserID string) (*IntegrationRow, error) {
	query := `
		SELECT id, store_id, type, provider, status, credentials, token_expires_at, metadata, last_synced_at, created_at
		FROM integrations
		WHERE provider = 'instagram'
		  AND status = 'active'
		  AND (metadata->>'instagram_user_id' = $1
		       OR metadata->>'instagram_app_scoped_id' = $1)
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

// UpdatePriority sets the integration's priority within its store.
func (r *Repository) UpdatePriority(ctx context.Context, id, storeID string, priority int) error {
	integrationID, err := parseUUID(id)
	if err != nil {
		return err
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	return r.queries.UpdateIntegrationPriority(ctx, sqlc.UpdateIntegrationPriorityParams{
		ID:       integrationID,
		StoreID:  sID,
		Priority: int32(priority),
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
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Não é falha de infra: outra tentativa da mesma operação já
			// registrou esta chave. Claim decide se devolve a resposta dela,
			// recusa por estar em voo ou toma posse da falha.
			return nil, fmt.Errorf("creating idempotency key: %w", idempotency.ErrDuplicateKey)
		}
		return nil, fmt.Errorf("creating idempotency key: %w", err)
	}

	return r.toIdempotencyRecord(row), nil
}

// Reclaim toma posse (CAS) de um registro 'failed' — ou 'pending' velho o
// bastante para a tentativa dona ter morrido. false = outra tentativa venceu.
func (r *IdempotencyRepository) Reclaim(ctx context.Context, id string) (bool, error) {
	idemID, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	rows, err := r.queries.ReclaimIdempotencyKey(ctx, idemID)
	if err != nil {
		return false, fmt.Errorf("reclaiming idempotency key: %w", err)
	}
	return rows > 0, nil
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
// ProductRow is an alias of live.ProductRow (moved to internal/live with the
// comment-ingest flow, Bloco B4a). The repository keeps building the same value
// and every existing caller — plus the live.IngestRepository port — sees one
// identical type, so no conversion is needed at the seam.
type ProductRow = live.ProductRow

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

// TryDecrementProductStock decrements stock all-or-nothing for one product,
// returning ok=false (sem erro) quando não há estoque suficiente — o UPDATE
// atômico (WHERE stock >= qty) casou 0 rows. Distingue "estoque insuficiente"
// de erro real de banco, para o caller decidir (ex.: recovery pula reabertura).
func (r *Repository) TryDecrementProductStock(ctx context.Context, productID string, quantity int) (bool, error) {
	id, err := parseUUID(productID)
	if err != nil {
		return false, err
	}
	_, err = r.queries.DecrementProductStock(ctx, sqlc.DecrementProductStockParams{
		ID:    id,
		Stock: pgtype.Int4{Int32: int32(quantity), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("decrementing product stock: %w", err)
	}
	return true, nil
}

// HealFromError devolve a integração para 'active' quando uma chamada volta a
// dar certo, e diz se curou de fato. Só age sobre linha em 'error'.
func (r *Repository) HealFromError(ctx context.Context, id string) (bool, error) {
	integrationID, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	n, err := r.queries.HealIntegrationFromError(ctx, integrationID)
	if err != nil {
		return false, fmt.Errorf("healing integration from error: %w", err)
	}
	return n > 0, nil
}

// EmitMessageReceived grava o envelope message.received no outbox numa única
// transação, espelhando EmitCommentReceived. É o que permite a borda HTTP do
// webhook de DM responder 200 sem tocar em Graph nem em ERP.
func (r *Repository) EmitMessageReceived(ctx context.Context, env events.Envelope) error {
	return dbtx.InTx(ctx, r.pool, r.queries, func(q *sqlc.Queries) error {
		return events.Emit(ctx, q, env)
	})
}

// DecrementProductStockUpTo takes up to `want` units atomically and returns
// how many were actually taken (0 when out of stock). Powers partial waitlist
// promotion — one freed unit can serve part of a larger queued request.
func (r *Repository) DecrementProductStockUpTo(ctx context.Context, productID string, want int) (int, error) {
	id, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	taken, err := r.queries.DecrementProductStockUpTo(ctx, sqlc.DecrementProductStockUpToParams{
		ID:   id,
		Want: int32(want),
	})
	return int(taken), err
}

// RequeueWaitlistItemPartial re-queues a partially promoted waitlist item with
// the remaining quantity (the customer got some units, still waits for the rest).
func (r *Repository) RequeueWaitlistItemPartial(ctx context.Context, id string, remainingQty int) error {
	itemID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.RequeueWaitlistItemPartial(ctx, sqlc.RequeueWaitlistItemPartialParams{
		ID:       itemID,
		Quantity: int32(remainingQty),
	})
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
// LiveCommentExistsByPlatformID reports whether a comment with the given
// Instagram comment id was already stored. Used by the post-comment polling
// capture to avoid reprocessing comments across polls (and vs the webhook).
// FindOpenCartUserIDByHandle devolve o platform_user_id do carrinho ABERTO
// deste @ no evento, se houver. Ver a query para o porquê: o mesmo comprador
// chega com ids diferentes conforme venha por webhook ou por polling.
func (r *Repository) FindOpenCartUserIDByHandle(ctx context.Context, eventID, handle string) (string, bool) {
	if eventID == "" || handle == "" {
		return "", false
	}
	eID, err := parseUUID(eventID)
	if err != nil {
		return "", false
	}
	userID, err := r.queries.FindOpenCartUserIDByHandle(ctx, sqlc.FindOpenCartUserIDByHandleParams{
		EventID:        eID,
		PlatformHandle: handle,
	})
	if err != nil || userID == "" {
		return "", false
	}
	return userID, true
}

// FindDMCapableUserIDByHandle procura, na loja inteira e não só no evento, uma
// identidade já conhecida deste @ diferente da que acabou de chegar. Serve ao
// caso da loja comentando na própria transmissão, em que o polling devolve o id
// da CONTA — que nunca é aceito como destinatário de DM. Ver a query.
func (r *Repository) FindDMCapableUserIDByHandle(ctx context.Context, storeID, handle, excludeUserID string) (string, bool) {
	if storeID == "" || handle == "" {
		return "", false
	}
	sID, err := parseUUID(storeID)
	if err != nil {
		return "", false
	}
	userID, err := r.queries.FindDMCapableUserIDByHandle(ctx, sqlc.FindDMCapableUserIDByHandleParams{
		StoreID:       sID,
		Handle:        handle,
		ExcludeUserID: excludeUserID,
	})
	if err != nil || userID == "" {
		return "", false
	}
	return userID, true
}

func (r *Repository) LiveCommentExistsByPlatformID(ctx context.Context, platformCommentID string) (bool, error) {
	if platformCommentID == "" {
		return false, nil
	}
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM live_comments WHERE platform_comment_id = $1)`,
		platformCommentID,
	).Scan(&exists)
	return exists, err
}

// MarkLiveCommentPrivateReplyUsed records that a comment consumed its single
// allowed private reply, so the resend lookup skips it next time.
func (r *Repository) MarkLiveCommentPrivateReplyUsed(ctx context.Context, platformCommentID string) error {
	if platformCommentID == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE live_comments SET private_reply_used = true WHERE platform_comment_id = $1`,
		platformCommentID,
	)
	return err
}

// SetLiveCommentHidden mirrors the Instagram hide/unhide state so the resend
// lookup never targets a comment that can't receive a private reply.
func (r *Repository) SetLiveCommentHidden(ctx context.Context, platformCommentID string, hidden bool) error {
	if platformCommentID == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE live_comments SET hidden = $2 WHERE platform_comment_id = $1`,
		platformCommentID, hidden,
	)
	return err
}

// MarkLiveCommentDeleted records that the comment no longer exists on Instagram.
// Kept separate from hidden: a deleted comment disappears from the merchant's
// list (mirroring Instagram), while a hidden one stays visible and reversible.
func (r *Repository) MarkLiveCommentDeleted(ctx context.Context, platformCommentID string) error {
	if platformCommentID == "" {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE live_comments SET deleted_at = now() WHERE platform_comment_id = $1`,
		platformCommentID,
	)
	return err
}

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
// CreateLiveCommentParams's canonical home moved to internal/live (Bloco B4b);
// this alias keeps the Repository builder and the remaining integration call
// sites compiling unchanged while the ingest core owns the shape.
type CreateLiveCommentParams = live.CreateLiveCommentParams

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
	var cartID pgtype.UUID
	if params.CartID != "" {
		cID, err := parseUUID(params.CartID)
		if err != nil {
			return "", err
		}
		cartID = cID
	}
	// Create the row and emit waitlist.queued (keyed by the waitlist item id, the
	// catalog idempotency key) atomically in one tx via the shared runner.
	var itemID string
	err = dbtx.InTx(ctx, r.pool, r.queries, func(q *sqlc.Queries) error {
		row, err := q.CreateWaitlistItem(ctx, sqlc.CreateWaitlistItemParams{
			EventID:        eID,
			ProductID:      pID,
			PlatformUserID: params.PlatformUserID,
			PlatformHandle: params.PlatformHandle,
			Quantity:       int32(params.Quantity),
			Position:       int32(params.Position),
			CartID:         cartID,
		})
		if err != nil {
			return fmt.Errorf("creating waitlist item: %w", err)
		}
		itemID = uuidToString(row.ID)
		return events.EmitInternal(ctx, q, events.WaitlistQueued, "waitlist.queued:"+itemID, struct {
			WaitlistItemID string `json:"waitlist_item_id"`
			EventID        string `json:"event_id"`
			ProductID      string `json:"product_id"`
			CartID         string `json:"cart_id,omitempty"`
			PlatformHandle string `json:"platform_handle,omitempty"`
			Quantity       int    `json:"quantity"`
			Position       int    `json:"position"`
		}{
			WaitlistItemID: itemID,
			EventID:        params.EventID,
			ProductID:      params.ProductID,
			CartID:         params.CartID,
			PlatformHandle: params.PlatformHandle,
			Quantity:       params.Quantity,
			Position:       params.Position,
		})
	})
	if err != nil {
		return "", err
	}
	return itemID, nil
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
		CartID:         uuidToString(row.CartID),
		NotifiedAt:     timestamptzToPtr(row.NotifiedAt),
		ExpiresAt:      timestamptzToPtr(row.ExpiresAt),
	}, nil
}

// ClaimNextWaitlistItem atomically claims the next waiting buyer (lowest
// position) and flips it to 'notified' in the same statement — FOR UPDATE SKIP
// LOCKED guarantees concurrent callers claim DISTINCT buyers. Returns nil when
// the queue is empty. The stock gate still runs after the claim; on
// insufficient stock the caller reverts via RevertWaitlistToWaiting.
func (r *Repository) ClaimNextWaitlistItem(ctx context.Context, eventID, productID string, expiresAt time.Time) (*WaitlistItemRow, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.ClaimNextWaitlistItem(ctx, sqlc.ClaimNextWaitlistItemParams{
		EventID:   eID,
		ProductID: pID,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || err.Error() == "no rows in result set" {
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
		CartID:         uuidToString(row.CartID),
		NotifiedAt:     timestamptzToPtr(row.NotifiedAt),
		ExpiresAt:      timestamptzToPtr(row.ExpiresAt),
	}, nil
}

// RevertWaitlistToWaiting undoes a claim when the stock gate fails after it —
// returns the buyer to the head of the queue.
func (r *Repository) RevertWaitlistToWaiting(ctx context.Context, id string) error {
	itemID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.RevertWaitlistToWaiting(ctx, itemID)
}

// MarkWaitlistNotified flips a waitlist row to "notified" with the TTL window.
// notificationSentAt records that we actually fired the DM/email so the worker
// can avoid double-sending if it ever resumes processing the same row.
func (r *Repository) MarkWaitlistNotified(ctx context.Context, id string, expiresAt, notificationSentAt time.Time) error {
	itemID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.MarkWaitlistNotified(ctx, sqlc.MarkWaitlistNotifiedParams{
		ID:                 itemID,
		ExpiresAt:          pgtype.Timestamptz{Time: expiresAt, Valid: true},
		NotificationSentAt: pgtype.Timestamptz{Time: notificationSentAt, Valid: true},
	})
}

// EmitWaitlistNotifiedParams carries the identifiers for a waitlist.notified event.
// EmitWaitlistNotifiedParams is the payload of the waitlist.notified fact. Its
// canonical home moved to internal/inventory (Bloco B3b); this alias keeps the
// repository emitter compiling unchanged.
type EmitWaitlistNotifiedParams = inventory.EmitWaitlistNotifiedParams

// EmitWaitlistNotified publishes waitlist.notified best-effort (non-transactional
// outbox insert). Unlike waitlist.queued, the "notified" transition is a multi-step
// promotion saga (claim → stock gate → cart promotion → DM) with no single tx to
// bind to, so the producer emits at the definitive success point. Keyed by the
// waitlist item id.
func (r *Repository) EmitWaitlistNotified(ctx context.Context, p EmitWaitlistNotifiedParams) error {
	return events.EmitInternal(ctx, r.queries, events.WaitlistNotified, "waitlist.notified:"+p.WaitlistItemID, struct {
		WaitlistItemID string `json:"waitlist_item_id"`
		EventID        string `json:"event_id"`
		ProductID      string `json:"product_id"`
		CartID         string `json:"cart_id,omitempty"`
		Quantity       int    `json:"quantity"`
		Remaining      int    `json:"remaining"`
	}{
		WaitlistItemID: p.WaitlistItemID,
		EventID:        p.EventID,
		ProductID:      p.ProductID,
		CartID:         p.CartID,
		Quantity:       p.Quantity,
		Remaining:      p.Remaining,
	})
}

// EmitWaitlistExpired publishes waitlist.expired best-effort (non-transactional
// outbox insert), keyed by the waitlist item id. Emitted right after the
// idempotency-gate status flip to 'expired'; the surrounding stock/ERP reversal
// is itself best-effort, so binding a tx here would add no real guarantee.
func (r *Repository) EmitWaitlistExpired(ctx context.Context, waitlistItemID, eventID, productID, cartID string) error {
	return events.EmitInternal(ctx, r.queries, events.WaitlistExpired, "waitlist.expired:"+waitlistItemID, struct {
		WaitlistItemID string `json:"waitlist_item_id"`
		EventID        string `json:"event_id"`
		ProductID      string `json:"product_id"`
		CartID         string `json:"cart_id,omitempty"`
	}{
		WaitlistItemID: waitlistItemID,
		EventID:        eventID,
		ProductID:      productID,
		CartID:         cartID,
	})
}

// StockEventParams carries the identifiers for a stock.reserved / stock.released
// event. ReservationID is the ERP reservation row id when one exists (design-A
// stores); Op labels the business operation (see StockOp).
// StockEventParams is the stock-event payload; canonical home is internal/erp
// (Bloco B2b). Aliased here so the Repository emission code stays unchanged.
type StockEventParams = erp.StockEventParams

// EmitStockReserved publishes stock.reserved best-effort. Keyed by reservation
// id when present, else by cart+product+op — both stable within one operation.
func (r *Repository) EmitStockReserved(ctx context.Context, p StockEventParams) error {
	return r.emitStockEvent(ctx, r.queries, events.StockReserved, "stock.reserved:", p)
}

// EmitStockReleased publishes stock.released best-effort, keyed by cart+product+op.
func (r *Repository) EmitStockReleased(ctx context.Context, p StockEventParams) error {
	return r.emitStockEvent(ctx, r.queries, events.StockReleased, "stock.released:", p)
}

// emitStockEvent is the single emission shape for stock events. It takes the
// queries handle so both best-effort (r.queries) and transactional (a WithTx
// handle, e.g. the expiry release loop) callers share the payload + dedup logic.
func (r *Repository) emitStockEvent(ctx context.Context, q *sqlc.Queries, name events.Name, keyPrefix string, p StockEventParams) error {
	return events.EmitInternal(ctx, q, name, stockDedupKey(name, keyPrefix, p), struct {
		Op            string `json:"op"`
		ProductID     string `json:"product_id"`
		Quantity      int    `json:"quantity"`
		CartID        string `json:"cart_id,omitempty"`
		EventID       string `json:"event_id,omitempty"`
		ReservationID string `json:"reservation_id,omitempty"`
	}{
		Op:            p.Op,
		ProductID:     p.ProductID,
		Quantity:      p.Quantity,
		CartID:        p.CartID,
		EventID:       p.EventID,
		ReservationID: p.ReservationID,
	})
}

// stockDedupKey builds the idempotency key for a stock event: the ERP
// reservation id when one exists (reserved only), else cart+product+op. When
// there is no cart yet (e.g. cart_add reserves before the cart row exists) the
// key falls back to product+op, which is not unique across buyers — callers that
// need a stable key must supply a ReservationID.
func stockDedupKey(name events.Name, keyPrefix string, p StockEventParams) string {
	if name == events.StockReserved && p.ReservationID != "" {
		return keyPrefix + p.ReservationID
	}
	return keyPrefix + p.CartID + ":" + p.ProductID + ":" + p.Op
}

// CancelWaitlistItem flips status to 'cancelled' iff the row belongs to the
// given cart and is still actionable (waiting/notified). Ownership and
// status are enforced in the WHERE clause so we don't need a separate read.
// Returns true when a row was actually updated.
func (r *Repository) CancelWaitlistItem(ctx context.Context, id, cartID string) error {
	itemID, err := parseUUID(id)
	if err != nil {
		return err
	}
	cID, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.CancelWaitlistItem(ctx, sqlc.CancelWaitlistItemParams{
		ID:     itemID,
		CartID: cID,
	})
}

// GetWaitlistItemForCart fetches a single row scoped to a cart — used by the
// drop endpoint to know whether the row was 'notified' (and therefore needs
// stock release + queue advancement) before cancelling.
func (r *Repository) GetWaitlistItemForCart(ctx context.Context, id, cartID string) (*WaitlistItemRow, error) {
	itemID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetWaitlistItemForCart(ctx, sqlc.GetWaitlistItemForCartParams{
		ID:     itemID,
		CartID: cID,
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
		CartID:         uuidToString(row.CartID),
		NotifiedAt:     timestamptzToPtr(row.NotifiedAt),
		ExpiresAt:      timestamptzToPtr(row.ExpiresAt),
	}, nil
}

// CountActiveByEventProduct returns how many waiting+notified entries exist
// for an event/product. Used by the Tiny webhook to decide whether to even
// attempt a fila promotion after a stock change.
func (r *Repository) CountActiveByEventProduct(ctx context.Context, eventID, productID string) (int, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	count, err := r.queries.CountActiveByEventProduct(ctx, sqlc.CountActiveByEventProductParams{
		EventID:   eID,
		ProductID: pID,
	})
	return int(count), err
}

// ListActiveByCartRow is the projection returned to the public checkout. Its
// canonical home moved to internal/inventory (Bloco B3a); this alias keeps the
// repository builder and the checkout call sites compiling unchanged.
type ListActiveByCartRow = inventory.ListActiveByCartRow

// DecrementCartItem reduz a quantidade do (cart, product) por @delta. Se
// chegar a zero, executa o DELETE numa segunda chamada para manter a
// invariante "linha existe sse quantity > 0". Retorna a quantidade
// resultante (0 quando deletado).
func (r *Repository) DecrementCartItem(ctx context.Context, cartID, productID string, delta int) (int, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	q, err := r.queries.DecrementCartItemQuantity(ctx, sqlc.DecrementCartItemQuantityParams{
		CartID:    cID,
		ProductID: pID,
		Delta:     int32(delta),
	})
	if err != nil {
		return 0, err
	}
	if q.Valid && q.Int32 == 0 {
		if delErr := r.queries.DeleteCartItemByCartAndProduct(ctx, sqlc.DeleteCartItemByCartAndProductParams{
			CartID:    cID,
			ProductID: pID,
		}); delErr != nil {
			return 0, delErr
		}
	}
	if !q.Valid {
		return 0, nil
	}
	return int(q.Int32), nil
}

// ListExpiredNotifiedWaitlist returns rows with status='notified' whose
// expires_at já passou. Esses são os candidatos a expirar (devolver
// estoque) e ceder a vez para o próximo da fila.
func (r *Repository) ListExpiredNotifiedWaitlist(ctx context.Context) ([]WaitlistItemRow, error) {
	rows, err := r.queries.ListExpiredNotifiedWaitlistItems(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WaitlistItemRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, WaitlistItemRow{
			ID:             uuidToString(row.ID),
			EventID:        uuidToString(row.EventID),
			ProductID:      uuidToString(row.ProductID),
			PlatformUserID: row.PlatformUserID,
			PlatformHandle: row.PlatformHandle,
			Quantity:       int(row.Quantity),
			Position:       int(row.Position),
			Status:         row.Status,
			CartID:         uuidToString(row.CartID),
			NotifiedAt:     timestamptzToPtr(row.NotifiedAt),
			ExpiresAt:      timestamptzToPtr(row.ExpiresAt),
		})
	}
	return out, nil
}

// ListEventsWithWaitingByProduct returns event_ids that have at least one
// waiting (not notified) row for the given product. Used by the Tiny stock
// webhook to fan out promotion attempts.
func (r *Repository) ListEventsWithWaitingByProduct(ctx context.Context, productID string) ([]string, error) {
	pID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListEventsWithWaitingByProduct(ctx, pID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, uuidToString(row))
	}
	return out, nil
}

// GetProductIDByExternalID resolves a local product UUID from the ERP
// external_id (Tiny idProduto). Returns "" + nil error when no match.
func (r *Repository) GetProductIDByExternalID(ctx context.Context, storeID, externalSource, externalID string) (string, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}
	row, err := r.queries.GetProductByExternalID(ctx, sqlc.GetProductByExternalIDParams{
		StoreID:        sID,
		ExternalSource: externalSource,
		ExternalID:     pgtype.Text{String: externalID, Valid: externalID != ""},
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", nil
		}
		return "", err
	}
	return uuidToString(row.ID), nil
}

// ListActiveByCart returns the waitlist rows the public checkout should
// surface — only waiting/notified, ordered by created_at so the UI matches
// the order the customer asked for them.
func (r *Repository) ListActiveByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListActiveByCart(ctx, cID)
	if err != nil {
		return nil, err
	}
	out := make([]ListActiveByCartRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ListActiveByCartRow{
			ID:              uuidToString(row.ID),
			EventID:         uuidToString(row.EventID),
			ProductID:       uuidToString(row.ProductID),
			ProductName:     row.ProductName,
			ProductKeyword:  row.ProductKeyword,
			ProductImageURL: textToString(row.ProductImageUrl),
			ProductPrice:    row.ProductPrice.Int64,
			Quantity:        int(row.Quantity),
			Position:        int(row.Position),
			Status:          row.Status,
			NotifiedAt:      timestamptzToPtr(row.NotifiedAt),
			ExpiresAt:       timestamptzToPtr(row.ExpiresAt),
			CreatedAt:       timestamptzToPtr(row.CreatedAt),
		})
	}
	return out, nil
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
// CreateWaitlistItemParams's canonical home moved to internal/live (Bloco B4b);
// this alias keeps the Repository builder and the remaining integration call
// sites compiling unchanged.
type CreateWaitlistItemParams = live.CreateWaitlistItemParams

// WaitlistItemRow represents a waitlist item.
// WaitlistItemRow's canonical home moved to internal/inventory (Bloco B3a); this
// alias keeps the repository builder and the B3b flows still in integration
// compiling unchanged.
type WaitlistItemRow = inventory.WaitlistItemRow

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

// MarkCartERPFinalisationDone flips the Order's payment row out of pending/failed
// and into the terminal "done" state once the Tiny order is successfully created.
// The attempts counter is bumped so the admin retry UI can show "took N tries".
//
// Fatia 11b: authoritative on order_payments (resolved from cart_id via the Order).
// The cart's finalisation columns are no longer written — only the reserve-state
// columns (erp_order_state/…/external_order_id) still live on the cart. A no-op
// (0 rows) when the Order isn't materialised yet — never happens post-payment,
// where OnCartPaid always materialises the Order before the ERP reactor runs.
func (r *Repository) MarkCartERPFinalisationDone(ctx context.Context, cartID string) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.MarkOrderERPFinalisationDone(ctx, id)
}

// MarkCartERPFinalisationFailed records a finalisation failure on the Order's
// payment row so the admin order page can show the error and a retry button. The
// caller is responsible for re-creating the saída-manual reservations in Tiny
// BEFORE calling this — the row must reach the "failed" state already with the
// stock held against it, never released.
//
// paymentSnapshot is the JSON-serialised providers.PaymentStatus from the
// initial webhook attempt. It's stored COALESCE-style so the original
// gateway snapshot is preserved across retries (the SQL guards against
// overwrite); pass an empty slice on retry calls.
//
// Fatia 11b: authoritative on order_payments (resolved from cart_id via the Order).
func (r *Repository) MarkCartERPFinalisationFailed(ctx context.Context, cartID, errMsg string, paymentSnapshot []byte) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.MarkOrderERPFinalisationFailed(ctx, sqlc.MarkOrderERPFinalisationFailedParams{
		CartID:             id,
		ErpLastError:       pgtype.Text{String: errMsg, Valid: errMsg != ""},
		ErpPaymentSnapshot: paymentSnapshot,
	})
}

// CartERPFinalisationRow is the slim view used by the admin retry endpoint and
// order detail page (status pending|done|failed). Canonical home is internal/erp
// (Bloco B2c-2, where the legacy finalisation moved); aliased here so the
// Repository (which owns the SQL) satisfies erp.ERPRepository directly.
type CartERPFinalisationRow = erp.CartFinalisationStatus

// GetCartERPFinalisationStatus reads the Order payment row's ERP finalisation
// lifecycle fields. Used by the admin retry endpoint to gate the retry on
// status='failed' and surface the error verbatim on the order detail page.
//
// Fatia 11b: authoritative on order_payments (resolved from cart_id). Returns
// pgx.ErrNoRows when the Order isn't materialised for the cart — callers treat
// that as "nothing to finalise/retry". external_order_id stays authoritative on
// the cart (reserve column) and is joined in for the resume-vs-legacy decision.
func (r *Repository) GetCartERPFinalisationStatus(ctx context.Context, cartID string) (*CartERPFinalisationRow, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetOrderERPFinalisationStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &CartERPFinalisationRow{
		CartID:          uuidToString(row.CartID),
		Status:          row.ErpFinalisationStatus,
		AttemptsCount:   int(row.ErpAttemptsCount),
		PaymentSnapshot: row.ErpPaymentSnapshot,
		ExternalOrderID: row.ExternalOrderID,
	}
	if row.ErpLastError.Valid {
		out.LastError = row.ErpLastError.String
	}
	if row.ErpLastAttemptAt.Valid {
		t := row.ErpLastAttemptAt.Time
		out.LastAttemptAt = &t
	}
	return out, nil
}

// CartERPInvoiceRow mirrors the persisted NFe state on a cart. Status may be
// empty when no NFe has ever been linked.
type CartERPInvoiceRow struct {
	CartID          string
	InvoiceID       string
	InvoiceKey      string
	InvoiceStatus   string
	EmittedAt       *time.Time
	ExternalOrderID string
}

// GetCartERPInvoice returns the NFe state stored on the Order's payment row
// (authoritative since Fatia 11b, resolved from cart_id). Used by the order
// detail handler (to decide whether "Aguardando NFe" or "Criar envio" should be
// shown) and by the manual sync endpoint.
func (r *Repository) GetCartERPInvoice(ctx context.Context, cartID string) (*CartERPInvoiceRow, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetOrderERPInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &CartERPInvoiceRow{CartID: uuidToString(row.CartID), ExternalOrderID: row.ExternalOrderID}
	if row.InvoiceID.Valid {
		out.InvoiceID = row.InvoiceID.String
	}
	if row.InvoiceKey.Valid {
		out.InvoiceKey = row.InvoiceKey.String
	}
	if row.InvoiceStatus.Valid {
		out.InvoiceStatus = row.InvoiceStatus.String
	}
	if row.InvoiceEmittedAt.Valid {
		t := row.InvoiceEmittedAt.Time
		out.EmittedAt = &t
	}
	return out, nil
}

// UpsertCartERPInvoiceParams carries the NFe fields written to the Order's
// payment row (EmittedAt nil = preserve previous value, COALESCE on the SQL
// side). Canonical home is internal/erp (Bloco B2d — the invoice sync that
// builds it lives there); aliased here so the Repository (which owns the SQL)
// keeps compiling unchanged.
type UpsertCartERPInvoiceParams = erp.UpsertCartERPInvoiceParams

// UpsertCartERPInvoice persists the NFe pulled from the ERP onto the Order's
// payment row (authoritative since Fatia 11b, resolved from cart_id). Idempotent
// — both the webhook handler and the manual sync endpoint go through it so
// re-running the same fetch never produces a different state. Returns the number
// of rows written: 0 means no Order exists for the cart yet (benign skip — NF is
// always post-confirmation, so the caller logs and moves on).
func (r *Repository) UpsertCartERPInvoice(ctx context.Context, p UpsertCartERPInvoiceParams) (int64, error) {
	id, err := parseUUID(p.CartID)
	if err != nil {
		return 0, err
	}
	var emitted pgtype.Timestamptz
	if p.EmittedAt != nil && !p.EmittedAt.IsZero() {
		emitted = pgtype.Timestamptz{Time: *p.EmittedAt, Valid: true}
	}
	return r.queries.UpsertOrderERPInvoice(ctx, sqlc.UpsertOrderERPInvoiceParams{
		CartID:        id,
		InvoiceID:     pgtype.Text{String: p.InvoiceID, Valid: p.InvoiceID != ""},
		InvoiceKey:    pgtype.Text{String: p.InvoiceKey, Valid: p.InvoiceKey != ""},
		InvoiceStatus: pgtype.Text{String: p.InvoiceStatus, Valid: p.InvoiceStatus != ""},
		EmittedAt:     emitted,
	})
}

// FindCartByExternalOrderID locates the cart linked to an ERP pedido id for a
// given store. Used by the Tiny webhook handler — Tiny only sends the pedido
// id on nota_fiscal events, so we have to bridge back to the LiveCart cart
// before we can persist the invoice fields.
func (r *Repository) FindCartByExternalOrderID(ctx context.Context, externalOrderID, storeID string) (string, error) {
	store, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}
	row, err := r.queries.FindCartByExternalOrderID(ctx, sqlc.FindCartByExternalOrderIDParams{
		ExternalOrderID: pgtype.Text{String: externalOrderID, Valid: externalOrderID != ""},
		StoreID:         store,
	})
	if err != nil {
		return "", err
	}
	return uuidToString(row.ID), nil
}

// GetCartInvoiceAnchor returns just the two cart fields the NFe sync needs — the
// owning store and the ERP order id. It reuses GetCartForPaidOrder (single source
// for loading a paid cart) so the invoice sync never has to import CartRow; the
// erp package reaches the cart only through this enxuto port (Bloco B2d).
func (r *Repository) GetCartInvoiceAnchor(ctx context.Context, cartID string) (string, string, error) {
	cart, err := r.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return "", "", err
	}
	return cart.StoreID, cart.ExternalOrderID, nil
}

// GetCartShortID returns the cart's human-facing sequential number (#1189),
// stamped on ERP stock movements so the merchant can copy it from the Tiny
// extract and locate the cart in LiveCart. Enxuto reader over GetCartByID.
func (r *Repository) GetCartShortID(ctx context.Context, cartID string) (int32, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	cart, err := r.queries.GetCartByID(ctx, cID)
	if err != nil {
		return 0, fmt.Errorf("getting cart short id: %w", err)
	}
	return cart.ShortID, nil
}

// NonWaitlistedCartItem represents a cart item that is not waitlisted, with
// product info. Canonical home is internal/erp (Bloco B2c); aliased here so the
// Repository (which owns the SQL) and its ~call sites keep compiling unchanged.
type NonWaitlistedCartItem = erp.NonWaitlistedCartItem

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

// ExtendCartExpiration empurra cart.expires_at para no mínimo 'until'. Usado
// quando promovemos um cliente da waitlist para "notified" — ele precisa de
// uma janela extra para finalizar o checkout antes do cart expirar.
func (r *Repository) ExtendCartExpiration(ctx context.Context, cartID string, until time.Time) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.ExtendCartExpiration(ctx, sqlc.ExtendCartExpirationParams{
		ID:           id,
		NewExpiresAt: pgtype.Timestamptz{Time: until, Valid: true},
	})
}

// GetWaitlistNotifiedTTL retorna o TTL configurado para notified em um evento.
// Default 30min vem do schema (DEFAULT na coluna).
func (r *Repository) GetWaitlistNotifiedTTL(ctx context.Context, eventID string) (time.Duration, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	mins, err := r.queries.GetWaitlistNotifiedTTLByEvent(ctx, eID)
	if err != nil {
		return 0, err
	}
	return time.Duration(mins) * time.Minute, nil
}

// ExpireEventWaitlist encerra os itens de fila NÃO ATENDIDOS do evento e
// devolve os carrinhos afetados (RN-32). Ver o comentário da query
// ExpireWaitlistByEvent: o predicado poupa quem foi promovido e ainda está
// dentro da janela de TTL.
func (r *Repository) ExpireEventWaitlist(ctx context.Context, eventID string) ([]ExpiredWaitlistEntry, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ExpireWaitlistByEvent(ctx, eID)
	if err != nil {
		return nil, fmt.Errorf("expiring event waitlist: %w", err)
	}
	out := make([]ExpiredWaitlistEntry, 0, len(rows))
	for _, row := range rows {
		entry := ExpiredWaitlistEntry{
			PlatformUserID: row.PlatformUserID,
			PlatformHandle: row.PlatformHandle,
			ProductName:    row.ProductName,
		}
		if row.CartID.Valid {
			entry.CartID = row.CartID.String()
		}
		if row.CartToken.Valid {
			entry.CartToken = row.CartToken.String
		}
		out = append(out, entry)
	}
	return out, nil
}

// GetCartTotals devolve itens e valor de um carrinho, pela mesma query nomeada
// que o painel usa (cart_product_total_cents é a função canônica do GMV).
func (r *Repository) GetCartTotals(ctx context.Context, cartID string) (int, int64, error) {
	uid, err := parseUUID(cartID)
	if err != nil {
		return 0, 0, err
	}
	row, err := r.queries.GetCartTotals(ctx, uid)
	if err != nil {
		return 0, 0, fmt.Errorf("getting cart totals: %w", err)
	}
	return int(row.TotalItems), row.TotalValue, nil
}

// GetEventOwner devolve a loja e o título da campanha a partir do id do evento.
//
// Existe para os caminhos ASSÍNCRONOS (tasks asynq), que só recebem o eventID:
// não há request, não há store_id no contexto, e sem a loja não dá para ler as
// settings de notificação nem montar {loja}/{evento}. Título vem como string
// vazia quando NULL — live_events.title é nullable e já derrubou uma tela em
// produção por ser lido direto num string Go.
func (r *Repository) GetEventOwner(ctx context.Context, eventID string) (storeID, title string, err error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return "", "", err
	}
	event, err := r.queries.GetLiveEventByID(ctx, eID)
	if err != nil {
		return "", "", fmt.Errorf("getting event owner: %w", err)
	}
	return uuidToString(event.StoreID), event.Title.String, nil
}

// ExpiredWaitlistEntry é um item de fila que morreu com a campanha: um por
// (comprador, produto). O carrinho pode se repetir entre linhas — o mesmo
// comprador pode esperar dois produtos —, e por isso a deduplicação de
// carrinhos para re-armar cart.expire fica no chamador, junto da decisão de
// quantas DMs mandar.
type ExpiredWaitlistEntry struct {
	CartID         string
	CartToken      string
	PlatformUserID string
	PlatformHandle string
	ProductName    string
}

// GetEventCartExpirationMinutes devolve o prazo EFETIVO do carrinho para o
// evento (RN-34: curto ou estendido conforme close_cart_on_event_end, com
// fallback para a loja), lendo da fonte única GetEventCartSettings.
func (r *Repository) GetEventCartExpirationMinutes(ctx context.Context, eventID string) (int, error) {
	eID, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}
	settings, err := r.queries.GetEventCartSettings(ctx, eID)
	if err != nil {
		return 0, fmt.Errorf("reading event cart settings: %w", err)
	}
	return int(settings.EffectiveCartExpirationMinutes), nil
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

// ExpireCartResult is the outcome of the atomic flip+local-release transaction.
// ExpireCartResult is the outcome of ExpireCartAndReleaseStock. Its canonical
// home moved to internal/inventory (Bloco B3b); this alias keeps the repository
// transaction compiling unchanged.
type ExpireCartResult = inventory.ExpireCartResult

// ExpireCartAndReleaseStock faz, numa ÚNICA transação: (1) o flip guard-first
// do cart para 'expired' (ExpireCart — recusa cart pago/já-terminal) e, só se
// ele ganhou, (2) devolve ao estoque local a quantidade não-waitlisted de TODOS
// os itens do cart. Atomicidade é a garantia anti-dupla-devolução: uma vez
// commitado, o cart deixa de ser elegível (guard-first) e um retry pós-crash não redevolve;
// um crash antes do commit reverte tudo (o cart segue elegível, sem vazamento).
// As ações de ERP (best-effort, remotas) ficam FORA deste tx, no Service.
func (r *Repository) ExpireCartAndReleaseStock(ctx context.Context, cartID, storeID string) (ExpireCartResult, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return ExpireCartResult{}, err
	}

	var result ExpireCartResult
	err = dbtx.InTx(ctx, r.pool, r.queries, func(q *sqlc.Queries) error {
		cart, err := q.ExpireCart(ctx, cID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Guard pegou: cart pago ou já expirado/cancelado. Não elegível.
				result = ExpireCartResult{Eligible: false}
				return nil
			}
			return fmt.Errorf("flip cart to expired: %w", err)
		}

		items, err := q.ListNonWaitlistedCartItems(ctx, cID)
		if err != nil {
			return fmt.Errorf("listing items for expiry release: %w", err)
		}
		freed := make([]string, 0, len(items))
		eventID := uuidToString(cart.EventID)
		for _, item := range items {
			if _, err := q.IncrementProductStock(ctx, sqlc.IncrementProductStockParams{
				ID:    item.ProductID,
				Stock: pgtype.Int4{Int32: item.Quantity, Valid: true},
			}); err != nil {
				return fmt.Errorf("release local stock on expiry: %w", err)
			}
			productID := uuidToString(item.ProductID)
			// stock.released in the SAME tx as the release (transactional outbox).
			if err := r.emitStockEvent(ctx, q, events.StockReleased, "stock.released:", StockEventParams{
				Op:        string(StockOpCartExpiry),
				ProductID: productID,
				Quantity:  int(item.Quantity),
				CartID:    cartID,
				EventID:   eventID,
			}); err != nil {
				return fmt.Errorf("emitting stock.released on expiry: %w", err)
			}
			freed = append(freed, productID)
		}

		// cart.expired in the SAME tx: committed atomically with the flip + stock
		// release, so it is never lost. store_id lets the ERP reversal consumer
		// (ReactCartExpiredERP) act without a cart→store lookup.
		//
		// events.Emit direto (não EmitInternal): LiveEventID é um campo de
		// primeira classe do Envelope, e eventID já está em mãos (sem query extra).
		cartExpiredPayload, err := json.Marshal(struct {
			CartID          string   `json:"cart_id"`
			StoreID         string   `json:"store_id"`
			EventID         string   `json:"event_id"`
			FreedProductIDs []string `json:"freed_product_ids"`
		}{CartID: cartID, StoreID: storeID, EventID: eventID, FreedProductIDs: freed})
		if err != nil {
			return fmt.Errorf("marshaling cart.expired payload: %w", err)
		}
		if err := events.Emit(ctx, q, events.Envelope{
			Name:        events.CartExpired,
			Source:      events.SourceInternal,
			DedupKey:    "cart.expired:" + cartID,
			LiveEventID: eventID,
			Payload:     cartExpiredPayload,
		}); err != nil {
			return fmt.Errorf("emitting cart.expired: %w", err)
		}

		result = ExpireCartResult{Eligible: true, EventID: eventID, FreedProductIDs: freed}
		return nil
	})
	if err != nil {
		return ExpireCartResult{}, err
	}
	return result, nil
}

// CancelCartResult is the outcome of the atomic cancel+local-release transaction.
type CancelCartResult struct {
	// Eligible=false quando o guard do UPDATE devolveu 0 rows: o cart foi PAGO
	// (o pagamento vence o cancelamento) ou já estava expirado/cancelado. O
	// caller aborta sem devolver estoque nem tocar o ERP.
	Eligible bool
	EventID  string
	// FreedProductIDs: produtos cujo estoque local foi devolvido (para promover
	// a waitlist depois do commit).
	FreedProductIDs []string
}

// CancelCartAndReleaseStock faz, numa ÚNICA transação: (1) o flip guard-first do
// cart para 'cancelled' com reason='store_cancelled' (CancelCart — recusa cart
// pago/já-terminal), (2) a devolução ao estoque local da quantidade
// não-waitlisted de TODOS os itens e (3) o cancelamento dos itens de waitlist
// deste cart (o carrinho morreu, então ninguém deve ser promovido para ele).
//
// Mesmo desenho do ExpireCartAndReleaseStock: a atomicidade é a garantia
// anti-dupla-devolução (commitado, o cart deixa de ser elegível; um retry não
// redevolve), e as ações remotas de ERP ficam FORA do tx — no reactor de
// cart.cancelled, com retry + DLQ próprios.
// CancelCartOnRefund flips um carrinho reembolsado para 'cancelled'
// (reason 'refunded'). Guard-first e idempotente — ver a query. Devolve se o
// flip aconteceu nesta chamada (false = já era terminal, redelivery do asynq).
func (r *Repository) CancelCartOnRefund(ctx context.Context, cartID string) (bool, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return false, err
	}
	n, err := r.queries.CancelCartOnRefund(ctx, cID)
	if err != nil {
		return false, fmt.Errorf("cancelling refunded cart: %w", err)
	}
	return n > 0, nil
}

func (r *Repository) CancelCartAndReleaseStock(ctx context.Context, cartID, storeID string) (CancelCartResult, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return CancelCartResult{}, err
	}

	var result CancelCartResult
	err = dbtx.InTx(ctx, r.pool, r.queries, func(q *sqlc.Queries) error {
		cart, err := q.CancelCart(ctx, cID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Guard pegou: cart pago no intervalo ou já terminal.
				result = CancelCartResult{Eligible: false}
				return nil
			}
			return fmt.Errorf("flip cart to cancelled: %w", err)
		}

		items, err := q.ListNonWaitlistedCartItems(ctx, cID)
		if err != nil {
			return fmt.Errorf("listing items for cancel release: %w", err)
		}
		freed := make([]string, 0, len(items))
		eventID := uuidToString(cart.EventID)
		for _, item := range items {
			if _, err := q.IncrementProductStock(ctx, sqlc.IncrementProductStockParams{
				ID:    item.ProductID,
				Stock: pgtype.Int4{Int32: item.Quantity, Valid: true},
			}); err != nil {
				return fmt.Errorf("release local stock on cancel: %w", err)
			}
			productID := uuidToString(item.ProductID)
			// stock.released na MESMA tx do release (outbox transacional).
			if err := r.emitStockEvent(ctx, q, events.StockReleased, "stock.released:", StockEventParams{
				Op:        string(StockOpCartCancelled),
				ProductID: productID,
				Quantity:  int(item.Quantity),
				CartID:    cartID,
				EventID:   eventID,
			}); err != nil {
				return fmt.Errorf("emitting stock.released on cancel: %w", err)
			}
			freed = append(freed, productID)
		}

		// Fila do próprio cart morre junto — sem isso o cliente seria promovido
		// (e receberia DM) para um checkout que responde cancelado.
		if _, err := q.CancelWaitlistItemsByCart(ctx, cID); err != nil {
			return fmt.Errorf("cancelling waitlist items on cancel: %w", err)
		}

		// cart.cancelled na MESMA tx: commitado atomicamente com o flip e a
		// devolução, então nunca se perde. reason='store_cancelled' distingue
		// este produtor dos outros dois (bloqueio de handle e cancelamento de
		// cobrança) — o reactor de ERP só age neste.
		//
		// dedup_key VAZIO de propósito (opt-out do índice único do outbox).
		//
		// A justificativa original era o reopen: o MESMO cart voltava à vida e
		// podia ser cancelado outra vez. Isso acabou na 000107 — o carrinho tem
		// ciclo de vida linear e cada cancelamento é de um cart distinto. A
		// chave vazia fica assim mesmo por ser o lado seguro: ela permite um
		// duplicado que não deve acontecer, enquanto "cart.cancelled:<id>"
		// engoliria em silêncio um cancelamento legítimo, deixando-o sem estorno
		// de ERP. A emissão já é exatamente-uma-vez por cancelamento: só
		// acontece dentro do tx do flip guard-first, que um retry não repete.
		// events.Emit direto (não EmitInternal): LiveEventID é campo de primeira
		// classe do Envelope, e eventID já está em mãos (sem query extra).
		cartCancelledPayload, err := json.Marshal(struct {
			CartID          string   `json:"cart_id"`
			StoreID         string   `json:"store_id"`
			EventID         string   `json:"event_id"`
			Reason          string   `json:"reason"`
			FreedProductIDs []string `json:"freed_product_ids"`
		}{CartID: cartID, StoreID: storeID, EventID: eventID, Reason: CancelReasonStore, FreedProductIDs: freed})
		if err != nil {
			return fmt.Errorf("marshaling cart.cancelled payload: %w", err)
		}
		if err := events.Emit(ctx, q, events.Envelope{
			Name:        events.CartCancelled,
			Source:      events.SourceInternal,
			DedupKey:    "",
			LiveEventID: eventID,
			Payload:     cartCancelledPayload,
		}); err != nil {
			return fmt.Errorf("emitting cart.cancelled: %w", err)
		}

		result = CancelCartResult{Eligible: true, EventID: eventID, FreedProductIDs: freed}
		return nil
	})
	if err != nil {
		return CancelCartResult{}, err
	}
	return result, nil
}

// RestoreCancelledCartAsPaid desfaz um cancelamento manual quando o pagamento
// chega depois: numa ÚNICA transação restaura o cart para 'checkout'+pago,
// retoma o estoque local devolvido pelo cancelamento (sem piso em zero — a
// unidade foi vendida de fato; saldo negativo é o sinal honesto de venda a mais)
// e emite cart.cancellation_reverted para auditoria.
//
// Devolve restored=false quando o cart não é um cancelamento de lojista
// pendente de pagamento — o caller trata como "não aplicável" e segue o fluxo
// normal (ou o skip benigno de ErrCartNotPayable).
//
// Também devolve o live_event_id do cart (a partir do RETURNING já feito
// dentro da tx, sem query extra) — o cart.paid emitido pelo caller logo depois
// do restore precisa dele.
func (r *Repository) RestoreCancelledCartAsPaid(
	ctx context.Context, cartID, storeID, paymentStatus, paymentID string, paidAt *time.Time, paymentMethod string,
) (restored bool, liveEventID string, err error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return false, "", err
	}
	var paidAtPg pgtype.Timestamptz
	if paidAt != nil {
		paidAtPg = pgtype.Timestamptz{Time: *paidAt, Valid: true}
	}

	err = dbtx.InTx(ctx, r.pool, r.queries, func(q *sqlc.Queries) error {
		cart, err := q.RestoreCancelledCartAsPaid(ctx, sqlc.RestoreCancelledCartAsPaidParams{
			ID:            cID,
			PaymentStatus: pgtype.Text{String: paymentStatus, Valid: true},
			CheckoutID:    pgtype.Text{String: paymentID, Valid: paymentID != ""},
			PaidAt:        paidAtPg,
			PaymentMethod: pgtype.Text{String: paymentMethod, Valid: paymentMethod != ""},
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // não é um cancelamento de lojista restaurável
			}
			return fmt.Errorf("restoring cancelled cart as paid: %w", err)
		}

		items, err := q.ListNonWaitlistedCartItems(ctx, cID)
		if err != nil {
			return fmt.Errorf("listing items for cancel revert: %w", err)
		}
		eventID := uuidToString(cart.EventID)
		retaken := make([]string, 0, len(items))
		for _, item := range items {
			if _, err := q.ForceDecrementProductStock(ctx, sqlc.ForceDecrementProductStockParams{
				ID:    item.ProductID,
				Stock: pgtype.Int4{Int32: item.Quantity, Valid: true},
			}); err != nil {
				return fmt.Errorf("retake local stock on cancel revert: %w", err)
			}
			productID := uuidToString(item.ProductID)
			if err := r.emitStockEvent(ctx, q, events.StockReserved, "stock.reserved:", StockEventParams{
				Op:        string(StockOpCancelReverted),
				ProductID: productID,
				Quantity:  int(item.Quantity),
				CartID:    cartID,
				EventID:   eventID,
			}); err != nil {
				return fmt.Errorf("emitting stock.reserved on cancel revert: %w", err)
			}
			retaken = append(retaken, productID)
		}

		// dedup_key vazio pelo mesmo motivo do cart.cancelled: um cart reaberto
		// pode passar pelo ciclo cancelar→pagar mais de uma vez, e o segundo
		// fato não pode sumir. A emissão é única por restore (roda dentro do tx
		// guardado que só um vencedor executa).
		// store_id, short_id e handle viajam no payload porque o consumidor (o
		// aviso no sino do painel) precisa saber PARA QUEM notificar e COMO
		// nomear o pedido, sem uma segunda ida ao banco.
		if err := events.EmitInternal(ctx, q, events.CartCancellationReverted, "", struct {
			CartID            string   `json:"cart_id"`
			StoreID           string   `json:"store_id"`
			EventID           string   `json:"event_id"`
			ShortID           int32    `json:"short_id"`
			PlatformHandle    string   `json:"platform_handle"`
			PaymentID         string   `json:"payment_id"`
			RetakenProductIDs []string `json:"retaken_product_ids"`
		}{
			CartID:            cartID,
			StoreID:           storeID,
			EventID:           eventID,
			ShortID:           cart.ShortID,
			PlatformHandle:    cart.PlatformHandle,
			PaymentID:         paymentID,
			RetakenProductIDs: retaken,
		}); err != nil {
			return fmt.Errorf("emitting cart.cancellation_reverted: %w", err)
		}

		restored = true
		liveEventID = eventID
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return restored, liveEventID, nil
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
	PaymentStatus   string
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

// GetCartByID loads a cart with the resolved storeID. Slim version of
// GetCartForPaidOrder — used in flows (waitlist expire, drop endpoint) that
// just need ownership/store context, not the customer/address blob.
func (r *Repository) GetCartByID(ctx context.Context, cartID string) (*CartRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	cart, err := r.queries.GetCartByID(ctx, cID)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting cart: %w", err)
	}
	event, err := r.queries.GetLiveEventByID(ctx, cart.EventID)
	if err != nil {
		return nil, fmt.Errorf("getting live event for cart: %w", err)
	}
	return &CartRow{
		ID:             uuidToString(cart.ID),
		EventID:        uuidToString(cart.EventID),
		StoreID:        uuidToString(event.StoreID),
		PlatformUserID: cart.PlatformUserID,
		PlatformHandle: cart.PlatformHandle,
		PaymentStatus:  cart.PaymentStatus.String,
		CreatedAt:      cart.CreatedAt.Time,
	}, nil
}

// CartExpirySnapshot is the slim view the scheduled cart.expire handler needs:
// the store (for ExpireCart), the lifecycle status and the current expires_at.
// The window matters because ExpireCartAndReleaseStock's guard does NOT check
// expires_at (the sweep pre-filters by it), so a scheduled task firing on a cart
// whose window was extended must NOT expire it prematurely.
// CartExpirySnapshot holds a cart's expiry-relevant fields. Its canonical home
// moved to internal/inventory (Bloco B3b); this alias keeps the repository
// builder and the ScheduleExpiry/RunScheduledExpiry bridge compiling unchanged.
type CartExpirySnapshot = inventory.CartExpirySnapshot

// GetCartExpirySnapshot loads the expiry-relevant fields for a cart, with the
// store resolved from the event. Returns (nil, nil) when the cart is gone.
func (r *Repository) GetCartExpirySnapshot(ctx context.Context, cartID string) (*CartExpirySnapshot, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	cart, err := r.queries.GetCartByID(ctx, cID)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("getting cart for expiry snapshot: %w", err)
	}
	event, err := r.queries.GetLiveEventByID(ctx, cart.EventID)
	if err != nil {
		return nil, fmt.Errorf("getting live event for expiry snapshot: %w", err)
	}
	var expiresAt *time.Time
	if cart.ExpiresAt.Valid {
		t := cart.ExpiresAt.Time
		expiresAt = &t
	}
	return &CartExpirySnapshot{
		StoreID:       uuidToString(event.StoreID),
		Status:        cart.Status,
		PaymentStatus: cart.PaymentStatus.String,
		ExpiresAt:     expiresAt,
	}, nil
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

// ListStockPositionsForReconciliation devolve, por produto ligado ao ERP, o
// contador local e quanto está segurado por reserva ativa. É a entrada da
// reconciliação: `local - held` é o saldo que o ERP deveria estar reportando.
func (r *Repository) ListStockPositionsForReconciliation(ctx context.Context, storeID, externalSource string) ([]erp.StockPosition, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListStockPositionsForReconciliation(ctx, sqlc.ListStockPositionsForReconciliationParams{
		StoreID:        sID,
		ExternalSource: externalSource,
	})
	if err != nil {
		return nil, fmt.Errorf("listing stock positions: %w", err)
	}
	out := make([]erp.StockPosition, 0, len(rows))
	for _, row := range rows {
		out = append(out, erp.StockPosition{
			ProductID:  uuidToString(row.ID),
			Name:       row.Name,
			ExternalID: row.ExternalID.String,
			LocalStock: int(row.LocalStock),
			Held:       int(row.Held),
		})
	}
	return out, nil
}

// CancelWaitlistForCartProduct mata a fila de um produto do carrinho e devolve
// quantas linhas morreram.
//
// Chamada quando o comprador reduz a quantidade no checkout: a parcela em fila
// é a primeira a sair (splitQuantityChange), mas a linha em waitlist_items
// continuava viva e era reivindicada pela promoção seguinte — que debitava
// estoque e emitia saída no Tiny sem entregar unidade a ninguém.
func (r *Repository) CancelWaitlistForCartProduct(ctx context.Context, cartID, productID string) (int, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	rows, err := r.queries.CancelWaitlistItemsByCartAndProduct(ctx, sqlc.CancelWaitlistItemsByCartAndProductParams{
		CartID:    cID,
		ProductID: pID,
	})
	if err != nil {
		return 0, fmt.Errorf("cancelling waitlist for cart product: %w", err)
	}
	return len(rows), nil
}

// DecrementCartItemWaitlistedQuantity subtrai `delta` da parcela em fila do
// item e devolve se a subtração ACONTECEU de verdade. Usada pela promoção da
// waitlist para virar a linha de "em fila" para "segurada" sem passar pelo
// UpsertCartItem (que soma quantity e waitlisted_quantity no ON CONFLICT e
// dobraria o pedido do comprador).
//
// A condição `waitlisted_quantity >= delta` é o que dá sentido ao retorno.
// Antes era `GREATEST(waitlisted_quantity - delta, 0)` sem filtro: o UPDATE
// casava a linha mesmo quando não havia parcela em fila para entregar, o
// GREATEST silenciava o resultado negativo, e `RowsAffected > 0` devolvia
// "consegui" de qualquer jeito.
//
// O chamador lê esse booleano como "entreguei uma unidade a este comprador" e
// então debita o estoque local e emite uma SAÍDA no Tiny. Com o retorno mentindo,
// isso vira estoque consumido por ninguém — e é a origem crônica do sintoma que
// o lojista descreve como "o comprador tira uma unidade e o sistema devolve
// errado": a redução no checkout zera a parcela em fila mas não cancela a linha
// de waitlist, e a promoção seguinte reivindica essa linha órfã.
func (r *Repository) DecrementCartItemWaitlistedQuantity(ctx context.Context, cartID, productID string, delta int) (bool, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return false, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return false, err
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE cart_items
		SET waitlisted_quantity = waitlisted_quantity - $3::int
		WHERE cart_id = $1 AND product_id = $2 AND waitlisted_quantity >= $3::int
	`, cID, pID, delta)
	if err != nil {
		return false, fmt.Errorf("decrementing waitlisted quantity: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetCartTokenByID returns the public checkout token for a cart. Used after
// a waitlist promotion to build the DM that drops the customer back into
// their checkout — we already know the cart_id (waitlist row points at it),
// only the token is missing.
func (r *Repository) GetCartTokenByID(ctx context.Context, cartID string) (string, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return "", err
	}
	var token string
	if err := r.pool.QueryRow(ctx, `SELECT token FROM carts WHERE id = $1`, cID).Scan(&token); err != nil {
		return "", fmt.Errorf("reading cart token: %w", err)
	}
	return token, nil
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
// It returns the cart's live_event_id from the RETURNING row — the cart.paid
// (etc.) fact emitted right after by the caller needs it and this avoids a
// second query.
func (r *Repository) UpdateCartPaymentStatus(ctx context.Context, cartID string, paymentStatus string, paymentID string, paidAt *time.Time, paymentMethod string) (liveEventID string, err error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return "", err
	}

	var paidAtPg pgtype.Timestamptz
	if paidAt != nil {
		paidAtPg = pgtype.Timestamptz{Time: *paidAt, Valid: true}
	}

	cart, err := r.queries.UpdateCartPayment(ctx, sqlc.UpdateCartPaymentParams{
		ID:            cID,
		PaymentStatus: pgtype.Text{String: paymentStatus, Valid: true},
		CheckoutID:    pgtype.Text{String: paymentID, Valid: paymentID != ""},
		PaidAt:        paidAtPg,
		PaymentMethod: pgtype.Text{String: paymentMethod, Valid: paymentMethod != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Guard da query recusou: cart expirado/cancelado. Não é falha.
			// Sentinel vive no pacote payment (B1d) — fonte única, consumido pelo
			// webhook de pagamento que agora mora lá.
			return "", paymentdomain.ErrCartNotPayable
		}
		return "", fmt.Errorf("updating cart payment status: %w", err)
	}
	return uuidToString(cart.EventID), nil
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
		Priority:       int(row.Priority),
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

func timestamptzToPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func textToString(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
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
// StockReservationRow / CreateStockReservationParams have their canonical home
// in internal/erp (Bloco B2b). Aliased here so the Repository (which owns the
// SQL) and the integration call sites keep compiling unchanged.
type StockReservationRow = erp.StockReservationRow

// CreateStockReservationParams holds params for creating a stock reservation.
type CreateStockReservationParams = erp.CreateStockReservationParams

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

// IsHandleBlocked is a fast lookup used by the comment processor to skip
// blocked customers without round-tripping through the customer package.
func (r *Repository) IsHandleBlocked(ctx context.Context, storeID, handle string) (bool, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return false, fmt.Errorf("parsing store id: %w", err)
	}
	return r.queries.IsHandleBlocked(ctx, sqlc.IsHandleBlockedParams{
		StoreID:        sID,
		PlatformHandle: handle,
	})
}

// ListOpenCartsByHandle returns non-paid carts for the given (store, handle),
// used by the customer-block flow to find what needs cancelling.
// ActivateEternalCartsForHandle marca os carrinhos abertos do @ como eternos e
// anula a expiração deles. Devolve os ids afetados.
func (r *Repository) ActivateEternalCartsForHandle(ctx context.Context, storeID, handle string) ([]string, error) {
	sid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ActivateEternalCartsForHandle(ctx, sqlc.ActivateEternalCartsForHandleParams{
		StoreID:        sid,
		PlatformHandle: normalizeVipHandle(handle),
	})
	if err != nil {
		return nil, fmt.Errorf("activating eternal carts: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, id := range rows {
		out = append(out, uuidToString(id))
	}
	return out, nil
}

// normalizeVipHandle espelha customer.normalizeHandle (@ + lowercase) — o
// carrinho grava platform_handle sem @, e a lista VIP guarda normalizado.
func normalizeVipHandle(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	return strings.ToLower(h)
}

func (r *Repository) ListOpenCartsByHandle(ctx context.Context, storeID, handle string) ([]CartRow, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return nil, fmt.Errorf("parsing store id: %w", err)
	}
	rows, err := r.queries.ListOpenCartsByHandle(ctx, sqlc.ListOpenCartsByHandleParams{
		StoreID:        sID,
		PlatformHandle: handle,
	})
	if err != nil {
		return nil, fmt.Errorf("listing open carts by handle: %w", err)
	}
	out := make([]CartRow, len(rows))
	for i, c := range rows {
		out[i] = CartRow{
			ID:             uuidToString(c.ID),
			EventID:        uuidToString(c.EventID),
			PlatformUserID: c.PlatformUserID,
			PlatformHandle: c.PlatformHandle,
			CreatedAt:      c.CreatedAt.Time,
		}
	}
	return out, nil
}

// CancelCartAsBlocked flips a cart to status='cancelled' with reason='customer_blocked'.
// Idempotent — already-paid carts are left untouched by the underlying query.
func (r *Repository) CancelCartAsBlocked(ctx context.Context, cartID string) error {
	cID, err := parseUUID(cartID)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cancel-cart tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := r.queries.WithTx(tx)

	cart, err := qtx.CancelCartAsBlocked(ctx, cID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || err.Error() == "no rows in result set" {
			// Cart was already paid or cancelled — nothing to do.
			return nil
		}
		return fmt.Errorf("cancelling cart as blocked: %w", err)
	}

	// cart.cancelled in the same tx (transactional outbox).
	payload, err := json.Marshal(struct {
		CartID  string `json:"cart_id"`
		EventID string `json:"event_id"`
		Reason  string `json:"reason"`
	}{CartID: cartID, EventID: cart.EventID.String(), Reason: "customer_blocked"})
	if err != nil {
		return fmt.Errorf("marshaling cart.cancelled payload: %w", err)
	}
	if err := events.Emit(ctx, qtx, events.Envelope{
		Name:     events.CartCancelled,
		Source:   events.SourceInternal,
		DedupKey: "cart.cancelled:" + cartID,
		Payload:  payload,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
// DecrementActiveReservationQuantity baixa `dec` unidades da reserva ativa e diz
// se conseguiu, junto do que sobrou. `applied=false` significa que a reserva
// tinha menos que isso — leitura obsoleta, ou outra requisição do mesmo
// comprador chegou primeiro.
//
// Substitui o par "ler quantidade / decidir o ramo / chamar o ERP / gravar",
// que é uma corrida: em 12/08/2026 um PATCH e um DELETE do mesmo item se
// cruzaram, o segundo decidiu pelo número obsoleto, mandou a entrada ao Tiny e
// só então bateu no CHECK (quantity > 0) — deixando um movimento sem lastro.
// UpsertActiveReservationQuantity soma unidades à reserva ativa, criando a linha
// se não existir. Uma chamada, sem leitura prévia — ler a lista de reservas para
// escolher entre CREATE e ADJUST é uma corrida, e as duas pontas dela apareceram
// em produção em 12/08/2026 ("no rows in result set" e "duplicate key ...
// uq_stock_reservations_active"), sempre depois de o movimento já ter ido ao ERP.
func (r *Repository) UpsertActiveReservationQuantity(ctx context.Context, p erp.UpsertReservationParams) (*erp.StockReservationRow, error) {
	eID, err := parseUUID(p.EventID)
	if err != nil {
		return nil, fmt.Errorf("parsing event ID: %w", err)
	}
	cID, err := parseUUID(p.CartID)
	if err != nil {
		return nil, fmt.Errorf("parsing cart ID: %w", err)
	}
	pID, err := parseUUID(p.ProductID)
	if err != nil {
		return nil, fmt.Errorf("parsing product ID: %w", err)
	}
	row, err := r.queries.UpsertActiveReservationQuantity(ctx, sqlc.UpsertActiveReservationQuantityParams{
		EventID:           eID,
		CartID:            cID,
		ProductID:         pID,
		ExternalProductID: p.ExternalProductID,
		IncQty:            int32(p.IncQty),
		ErpMovementID:     pgtype.Text{String: p.ERPMovementID, Valid: p.ERPMovementID != ""},
	})
	if err != nil {
		return nil, fmt.Errorf("upserting reservation quantity: %w", err)
	}
	return &erp.StockReservationRow{
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

func (r *Repository) DecrementActiveReservationQuantity(ctx context.Context, cartID, productID string, dec int) (erp.ReservationDecrement, error) {
	var out erp.ReservationDecrement
	cID, err := parseUUID(cartID)
	if err != nil {
		return out, fmt.Errorf("parsing cart ID: %w", err)
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return out, fmt.Errorf("parsing product ID: %w", err)
	}
	rows, err := r.queries.DecrementActiveReservationQuantity(ctx, sqlc.DecrementActiveReservationQuantityParams{
		CartID:        cID,
		ProductID:     pID,
		DecQty:        int32(dec),
		ErpMovementID: "",
	})
	if err != nil {
		return out, fmt.Errorf("decrementing reservation quantity: %w", err)
	}
	if len(rows) == 0 {
		return out, nil
	}
	out.Applied = true
	for _, row := range rows {
		out.ReservationIDs = append(out.ReservationIDs, uuidToString(row.ID))
		if row.Status == "active" {
			out.Remaining += int(row.Quantity)
		}
	}
	return out, nil
}

// RestoreReservationQuantityByID devolve `inc` unidades à reserva. É a
// compensação de DecrementActiveReservationQuantity: sem ela, um ERP que recusa
// depois do decremento deixaria o banco dizendo "livre" e o Tiny dizendo
// "reservada", divergência que nada reconcilia.
func (r *Repository) RestoreReservationQuantityByID(ctx context.Context, reservationID string, inc int) error {
	id, err := parseUUID(reservationID)
	if err != nil {
		return fmt.Errorf("parsing reservation ID: %w", err)
	}
	if _, err := r.queries.RestoreReservationQuantityByID(ctx, sqlc.RestoreReservationQuantityByIDParams{
		ID:     id,
		IncQty: int32(inc),
	}); err != nil {
		return fmt.Errorf("restoring reservation quantity: %w", err)
	}
	return nil
}

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

// MarkCartERPFinalisationAttempt persists the gateway snapshot (first write
// wins) and stamps erp_last_attempt_at BEFORE the finalisation touches the ERP —
// S1 of the resumable state machine.
//
// Fatia 11b: authoritative on order_payments (resolved from cart_id via the Order).
func (r *Repository) MarkCartERPFinalisationAttempt(ctx context.Context, cartID string, paymentSnapshot []byte) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.MarkOrderERPFinalisationAttempt(ctx, sqlc.MarkOrderERPFinalisationAttemptParams{
		CartID:             id,
		ErpPaymentSnapshot: paymentSnapshot,
	})
}

// ReverseReservationByID marks a single reservation reversed — only after the
// ERP confirmed the corresponding entrada E. The resume path re-reverses only
// rows still 'active'.
func (r *Repository) ReverseReservationByID(ctx context.Context, reservationID string) error {
	id, err := parseUUID(reservationID)
	if err != nil {
		return err
	}
	return r.queries.ReverseReservationByID(ctx, id)
}

// ClaimReservationForReversal reivindica a reserva antes de falar com o ERP.
// Devolve true só para quem ganhou a corrida — ver a query para o estoque
// fantasma que a ordem inversa produziu.
func (r *Repository) ClaimReservationForReversal(ctx context.Context, reservationID string) (bool, error) {
	id, err := parseUUID(reservationID)
	if err != nil {
		return false, err
	}
	rows, err := r.queries.ClaimReservationForReversal(ctx, id)
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// RestoreReservationToActive desfaz a reivindicação quando o ERP recusou o
// estorno, para que a próxima tentativa volte a enxergar a reserva.
func (r *Repository) RestoreReservationToActive(ctx context.Context, reservationID string) error {
	id, err := parseUUID(reservationID)
	if err != nil {
		return err
	}
	_, err = r.queries.RestoreReservationToActive(ctx, id)
	return err
}

// CartERPOrderState is the order-as-reservation lifecycle snapshot (design C).
// Canonical home is internal/erp (Bloco B2b); aliased here for the Repository.
type CartERPOrderState = erp.CartERPOrderState

// GetCartERPOrderState reads the cart's order-as-reservation state.
func (r *Repository) GetCartERPOrderState(ctx context.Context, cartID string) (*CartERPOrderState, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetCartERPOrderState(ctx, id)
	if err != nil {
		return nil, err
	}
	return &CartERPOrderState{
		State:           row.ErpOrderState,
		StockLaunched:   row.ErpStockLaunched,
		ExternalOrderID: row.ExternalOrderID,
	}, nil
}

// TransitionCartERPOrderState is the CAS of the order-as-reservation state
// machine. Returns false when the current state differs from `from` — the
// single-flight primitive of conversion/mutation.
func (r *Repository) TransitionCartERPOrderState(ctx context.Context, cartID, from, to string) (bool, error) {
	id, err := parseUUID(cartID)
	if err != nil {
		return false, err
	}
	rows, err := r.queries.TransitionCartERPOrderState(ctx, sqlc.TransitionCartERPOrderStateParams{
		CartID:    id,
		FromState: from,
		ToState:   to,
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// SetCartERPStockLaunched flips the durable "order stock launched" marker.
func (r *Repository) SetCartERPStockLaunched(ctx context.Context, cartID string, launched bool) error {
	id, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	return r.queries.SetCartERPStockLaunched(ctx, sqlc.SetCartERPStockLaunchedParams{
		ID:               id,
		ErpStockLaunched: launched,
	})
}

// StuckERPOrderOp is a conversion/mutation stuck in flight (process died).
// Canonical home is internal/erp (Bloco B2c); aliased here.
type StuckERPOrderOp = erp.StuckERPOrderOp

// ListStuckERPOrderOps lists carts stuck in converting/mutating older than
// the threshold — input for the reconciliation sweep.
func (r *Repository) ListStuckERPOrderOps(ctx context.Context, olderThan time.Duration) ([]StuckERPOrderOp, error) {
	rows, err := r.queries.ListStuckERPOrderOps(ctx, int32(olderThan.Seconds()))
	if err != nil {
		return nil, err
	}
	out := make([]StuckERPOrderOp, 0, len(rows))
	for _, row := range rows {
		out = append(out, StuckERPOrderOp{
			CartID:          uuidToString(row.ID),
			State:           row.ErpOrderState,
			ExternalOrderID: row.ExternalOrderID,
			StoreID:         uuidToString(row.StoreID),
		})
	}
	return out, nil
}

// StaleStockWebhookIntegration is an active Tiny integration that stopped
// receiving stock webhooks — the Tiny side silently removes the URL after
// consecutive delivery failures (field lesson, 11/07/2026). Canonical home is
// internal/erp (Bloco B2c); aliased here.
type StaleStockWebhookIntegration = erp.StaleStockWebhookIntegration

// ListTinyIntegrationsWithStaleStockWebhook lists active Tiny integrations
// with zero 'estoque' webhook events in the window and no alert in the last
// 24h (dedupe stamped in metadata).
func (r *Repository) ListTinyIntegrationsWithStaleStockWebhook(ctx context.Context, staleAfter time.Duration) ([]StaleStockWebhookIntegration, error) {
	rows, err := r.queries.ListTinyIntegrationsWithStaleStockWebhook(ctx, int32(staleAfter.Hours()))
	if err != nil {
		return nil, err
	}
	out := make([]StaleStockWebhookIntegration, 0, len(rows))
	for _, row := range rows {
		item := StaleStockWebhookIntegration{
			IntegrationID: uuidToString(row.ID),
			StoreID:       uuidToString(row.StoreID),
		}
		if row.LastStockEventAt.Valid {
			t := row.LastStockEventAt.Time
			item.LastStockEventAt = &t
		}
		out = append(out, item)
	}
	return out, nil
}

// StampIntegrationStockWebhookAlert records the alert moment in the
// integration metadata (24h dedupe for the delivery health-check).
func (r *Repository) StampIntegrationStockWebhookAlert(ctx context.Context, integrationID string) error {
	id, err := parseUUID(integrationID)
	if err != nil {
		return err
	}
	return r.queries.StampIntegrationStockWebhookAlert(ctx, id)
}

// lockSlotWait é quanto uma finalização espera por uma vaga de detentor antes
// de desistir. Generoso de propósito: quem espera aqui não segura conexão
// nenhuma, e desistir cedo transformaria contenção normal em erro. O ctx do
// chamador continua mandando — este é só o teto de quem tem ctx sem prazo.
const lockSlotWait = 30 * time.Second

// AcquireCartFinalisationLock takes a session-scoped Postgres advisory lock
// keyed on the cart id, held on a dedicated pool connection (advisory locks
// are per-connection). acquired=false means another finalisation of the SAME
// cart is running right now — gateway webhooks arrive duplicated and each one
// spawns its own goroutine (webhook_handler.go), so the loser just bails.
// The caller MUST call release() when acquired.
//
// # O DEADLOCK QUE ESTE SEMÁFORO FECHA
//
// Segurar a conexão é obrigatório pelo desenho: advisory lock é por SESSÃO de
// Postgres, então soltar a conexão soltaria o lock. Só que TUDO o que roda sob
// o lock consulta o mesmo pool — GetCartExpirySnapshot, GetProductByID,
// DecrementCartItemWaitlistedQuantity, a finalização ERP inteira. Ou seja: o
// detentor segura uma conexão e pede uma segunda. Isso é hold-and-wait.
//
// Quando o número de detentores simultâneos chega a MaxConns, todas as conexões
// do pool estão nas mãos de goroutines que esperam por mais uma. pgxpool.Acquire
// bloqueia no semáforo do puddle sem prazo, e o ctx desses caminhos é
// context.Background(): o travamento é PERMANENTE. E como o pool é único para a
// API inteira, não trava só a fila — trava todo handler HTTP, porque qualquer
// query passa pelo mesmo Acquire. Em produção MaxConns é 10
// (lib/database/postgres.go), então bastam 10 finalizações concorrentes.
//
// São cinco caminhos com esse formato, incluindo o de PAGAMENTO
// (finalizeCartERPOrder, ConfirmERPOrderPayment), ExpireCart, CancelCart e a
// promoção da fila. O gate de estoque movido para antes do lock em
// ProcessWaitlistForProduct reduz o número de detentores de "quantos chamaram"
// para "quantas unidades liberaram" — mas não o limita a nada. Era a única
// mitigação, e o invariante que faltava ("detentores < MaxConns") não era
// imposto por lugar nenhum.
//
// Agora é. O semáforo é o ponto único onde ele existe, porque é o ponto único
// por onde os cinco caminhos passam. A correção estrutural — passar ESTA
// conexão adiante para que o trecho crítico use uma só — continua sendo a
// melhor, e continua fora de alcance: exigiria um handle de queries em dezenas
// de assinaturas, atravessando o domínio live (AddToCart) e a finalização ERP
// inteira. Trocar deadlock por espera limitada no caminho mais quente do
// produto vale mais do que essa cirurgia agora.
//
// Falta de vaga vira ERRO, nunca acquired=false. São coisas diferentes:
// acquired=false significa "outra finalização DESTE cart está em voo, pode
// pular", e finalizeCartERPOrder confia nisso para não refazer trabalho —
// devolver false por contenção faria um cart pago perder a finalização ERP em
// silêncio. Erro é retentável e os cinco chamadores já o tratam.
func (r *Repository) AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error) {
	slotCtx, cancel := context.WithTimeout(ctx, lockSlotWait)
	defer cancel()
	select {
	case r.lockSlots <- struct{}{}:
	case <-slotCtx.Done():
		return nil, false, fmt.Errorf("waiting for a cart finalisation slot: %w", slotCtx.Err())
	}
	slotFreed := false
	freeSlot := func() {
		if !slotFreed {
			slotFreed = true
			<-r.lockSlots
		}
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte("erp_finalisation:" + cartID))
	key := int64(h.Sum64())

	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		freeSlot()
		return nil, false, fmt.Errorf("acquiring connection for advisory lock: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		conn.Release()
		freeSlot()
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !ok {
		conn.Release()
		freeSlot()
		return nil, false, nil
	}
	release = func() {
		// context.Background(): the unlock must run even when the caller's
		// ctx was cancelled mid-finalisation.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
		freeSlot()
	}
	return release, true, nil
}

// HasInFlightFinalisationForProduct reports whether some paid cart containing
// the product still has its ERP finalisation pending/failed within the guard
// window — promotion triggered by stock webhooks must wait it out.
func (r *Repository) HasInFlightFinalisationForProduct(ctx context.Context, productID string) (bool, error) {
	pID, err := parseUUID(productID)
	if err != nil {
		return false, err
	}
	return r.queries.HasInFlightFinalisationForProduct(ctx, pID)
}

// GetCartItemAvailableQty returns quantity - waitlisted_quantity for one cart
// item — the units the live add actually decremented from local stock.
func (r *Repository) GetCartItemAvailableQty(ctx context.Context, cartID, productID string) (int, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return 0, err
	}
	pID, err := parseUUID(productID)
	if err != nil {
		return 0, err
	}
	qty, err := r.queries.GetCartItemAvailableQty(ctx, sqlc.GetCartItemAvailableQtyParams{
		CartID:    cID,
		ProductID: pID,
	})
	return int(qty), err
}

// StoreInfo contains minimal store information needed for notifications.
// StoreInfo's canonical home moved to internal/live (Bloco B4b); this alias
// keeps GetStoreInfo and its callers compiling unchanged.
type StoreInfo = live.StoreInfo

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

// ProductSeqByExternalID resolve o produto local pelo código do ERP e devolve o
// `erp_seq` do mesmo instante.
//
// Id vazio quando o produto não está cadastrado — caso normal, não erro: o ERP
// notifica sobre o catálogo inteiro dele, e nós só espelhamos o que o lojista
// importou.
func (r *Repository) ProductSeqByExternalID(ctx context.Context, storeID, externalSource, externalID string) (string, int64, error) {
	sID, err := parseUUID(storeID)
	if err != nil {
		return "", 0, err
	}
	row, err := r.queries.ProductSeqByExternalID(ctx, sqlc.ProductSeqByExternalIDParams{
		StoreID:        sID,
		ExternalSource: externalSource,
		ExternalID:     pgtype.Text{String: externalID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil
		}
		return "", 0, fmt.Errorf("resolving product seq by external id: %w", err)
	}
	return uuidToString(row.ID), row.ErpSeq, nil
}

// ApplyERPStockMirror grava o saldo lido do ERP, e só se nenhum movimento nosso
// tiver acontecido desde a leitura.
//
// false significa leitura vencida — e descartar é a única resposta correta,
// porque não há como saber quanto daquele número já estava desatualizado. Uma
// leitura nova chega no próximo webhook ou na reconciliação.
// SumInFlightOutMovements soma as unidades que já prometemos e que o saldo lido
// do ERP ainda NÃO reflete — tudo que saiu do razão e não está `confirmed`.
//
// É o "em voo" da regra de admissão. Sem subtrair isto, o espelho reabastece o
// portão com estoque que já tem dono, que foi como uma live de 25 comentários
// sobre 20 unidades terminou com o saldo do Tiny em −13 (medido em 26/08).
func (r *Repository) SumInFlightOutMovements(ctx context.Context, externalProductID string) (int, error) {
	n, err := r.queries.SumInFlightOutMovements(ctx, externalProductID)
	if err != nil {
		return 0, fmt.Errorf("summing in-flight out movements: %w", err)
	}
	return int(n), nil
}

func (r *Repository) ApplyERPStockMirror(ctx context.Context, productID string, erpStock int, seenSeq int64) (bool, error) {
	id, err := parseUUID(productID)
	if err != nil {
		return false, err
	}
	n, err := r.queries.ApplyERPStockMirror(ctx, sqlc.ApplyERPStockMirrorParams{
		ID: id, ErpStock: int32(erpStock), SeenSeq: seenSeq,
	})
	if err != nil {
		return false, fmt.Errorf("applying ERP stock mirror: %w", err)
	}
	return n > 0, nil
}
