package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

type Repository struct {
	q    *sqlc.Queries
	pool *pgxpool.Pool
}

func NewRepository(q *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{q: q, pool: pool}
}

func (r *Repository) Create(ctx context.Context, params CreateLiveParams) (LiveRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return LiveRow{}, err
	}

	row, err := r.q.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
		StoreID:        storeUID,
		Title:          pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Platform:       params.Platform,
		PlatformLiveID: pgtype.Text{String: params.PlatformLiveID, Valid: params.PlatformLiveID != ""},
		Status:         params.Status,
	})
	if err != nil {
		return LiveRow{}, fmt.Errorf("creating live session: %w", err)
	}

	return toLiveRow(row), nil
}

func (r *Repository) GetByID(ctx context.Context, id, storeID string) (*LiveRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveSessionByID(ctx, sqlc.GetLiveSessionByIDParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("live session not found")
		}
		return nil, fmt.Errorf("getting live session: %w", err)
	}

	out := toLiveRow(row)
	return &out, nil
}

func (r *Repository) List(ctx context.Context, params ListLivesParams) (ListLivesResult, error) {
	// Build WHERE conditions
	conditions := []string{"store_id = $1"}
	args := []interface{}{params.StoreID}
	argIdx := 2

	// Search filter (title)
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("LOWER(title) LIKE $%d", argIdx))
		args = append(args, "%"+strings.ToLower(params.Search)+"%")
		argIdx++
	}

	// Status filter
	if len(params.Filters.Status) > 0 {
		placeholders := make([]string, len(params.Filters.Status))
		for i, status := range params.Filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, status)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Platform filter
	if len(params.Filters.Platform) > 0 {
		placeholders := make([]string, len(params.Filters.Platform))
		for i, platform := range params.Filters.Platform {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, platform)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("platform IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Date filters
	if params.Filters.DateFrom != nil && *params.Filters.DateFrom != "" {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", argIdx))
		args = append(args, *params.Filters.DateFrom)
		argIdx++
	}
	if params.Filters.DateTo != nil && *params.Filters.DateTo != "" {
		conditions = append(conditions, fmt.Sprintf("created_at <= $%d", argIdx))
		args = append(args, *params.Filters.DateTo)
		argIdx++
	}

	whereClause := strings.Join(conditions, " AND ")

	// Validate and build ORDER BY
	allowedSortFields := map[string]string{
		"title":      "title",
		"status":     "status",
		"platform":   "platform",
		"created_at": "created_at",
		"started_at": "started_at",
	}
	sortField, ok := allowedSortFields[params.Sorting.SortBy]
	if !ok {
		sortField = "created_at"
	}
	orderClause := fmt.Sprintf("%s %s", sortField, params.Sorting.OrderSQL())

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM live_sessions WHERE %s", whereClause)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return ListLivesResult{}, fmt.Errorf("counting live sessions: %w", err)
	}

	// Build main query with pagination
	query := fmt.Sprintf(`
		SELECT id, store_id, platform, platform_live_id, status, started_at, ended_at,
		       total_comments, total_orders, title, created_at, updated_at
		FROM live_sessions
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderClause, argIdx, argIdx+1)

	args = append(args, params.Pagination.Limit, params.Pagination.Offset())

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return ListLivesResult{}, fmt.Errorf("listing live sessions: %w", err)
	}
	defer rows.Close()

	lives := make([]LiveRow, 0)
	for rows.Next() {
		var row sqlc.LiveSession
		if err := rows.Scan(
			&row.ID,
			&row.StoreID,
			&row.Platform,
			&row.PlatformLiveID,
			&row.Status,
			&row.StartedAt,
			&row.EndedAt,
			&row.TotalComments,
			&row.TotalOrders,
			&row.Title,
			&row.CreatedAt,
			&row.UpdatedAt,
		); err != nil {
			return ListLivesResult{}, fmt.Errorf("scanning live session: %w", err)
		}
		lives = append(lives, toLiveRow(row))
	}

	if err := rows.Err(); err != nil {
		return ListLivesResult{}, fmt.Errorf("iterating live sessions: %w", err)
	}

	return ListLivesResult{
		Lives: lives,
		Total: total,
	}, nil
}

func (r *Repository) Update(ctx context.Context, params UpdateLiveParams) (LiveRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return LiveRow{}, err
	}
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return LiveRow{}, err
	}

	row, err := r.q.UpdateLiveSession(ctx, sqlc.UpdateLiveSessionParams{
		ID:             uid,
		StoreID:        storeUID,
		Title:          pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Platform:       params.Platform,
		PlatformLiveID: pgtype.Text{String: params.PlatformLiveID, Valid: params.PlatformLiveID != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LiveRow{}, httpx.ErrNotFound("live session not found")
		}
		return LiveRow{}, fmt.Errorf("updating live session: %w", err)
	}

	return toLiveRow(row), nil
}

func (r *Repository) Start(ctx context.Context, id, storeID string) (LiveRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return LiveRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveRow{}, err
	}

	row, err := r.q.StartLiveSession(ctx, sqlc.StartLiveSessionParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LiveRow{}, httpx.ErrNotFound("live session not found")
		}
		return LiveRow{}, fmt.Errorf("starting live session: %w", err)
	}

	return toLiveRow(row), nil
}

func (r *Repository) End(ctx context.Context, id, storeID string) (LiveRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return LiveRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveRow{}, err
	}

	row, err := r.q.EndLiveSession(ctx, sqlc.EndLiveSessionParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return LiveRow{}, httpx.ErrNotFound("live session not found")
		}
		return LiveRow{}, fmt.Errorf("ending live session: %w", err)
	}

	return toLiveRow(row), nil
}

func (r *Repository) Delete(ctx context.Context, id, storeID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	result, err := r.pool.Exec(ctx, "DELETE FROM live_sessions WHERE id = $1 AND store_id = $2", uid, storeUID)
	if err != nil {
		return fmt.Errorf("deleting live session: %w", err)
	}

	if result.RowsAffected() == 0 {
		return httpx.ErrNotFound("live session not found")
	}

	return nil
}

func (r *Repository) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveStatsOutput{}, err
	}

	query := `
		SELECT
			COUNT(*) as total_lives,
			COUNT(*) FILTER (WHERE status = 'live') as active_lives,
			COALESCE(SUM(total_orders), 0) as total_orders
		FROM live_sessions
		WHERE store_id = $1
	`

	var stats LiveStatsOutput
	err = r.pool.QueryRow(ctx, query, storeUID).Scan(
		&stats.TotalLives,
		&stats.ActiveLives,
		&stats.TotalOrders,
	)
	if err != nil {
		return LiveStatsOutput{}, fmt.Errorf("getting live stats: %w", err)
	}

	return stats, nil
}

// =============================================================================
// STORE SETTINGS
// =============================================================================

// GetStoreAutoSendSetting retrieves the auto_send_checkout_links setting for a store.
func (r *Repository) GetStoreAutoSendSetting(ctx context.Context, storeID string) (bool, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return false, err
	}

	store, err := r.q.GetStoreByID(ctx, uid)
	if err != nil {
		return false, fmt.Errorf("getting store: %w", err)
	}

	return store.AutoSendCheckoutLinks, nil
}

// =============================================================================
// CART OPERATIONS
// =============================================================================

// GetOrCreateCart finds an existing cart for a user in a session or creates a new one.
func (r *Repository) GetOrCreateCart(ctx context.Context, params GetOrCreateCartParams) (*CartRow, bool, error) {
	sessionID, err := parseUUID(params.SessionID)
	if err != nil {
		return nil, false, err
	}

	// Try to get existing cart first
	existing, err := r.q.GetCartBySessionAndUser(ctx, sqlc.GetCartBySessionAndUserParams{
		SessionID:      sessionID,
		PlatformUserID: params.PlatformUserID,
	})
	if err == nil {
		return &CartRow{
			ID:             existing.ID.String(),
			SessionID:      existing.SessionID.String(),
			PlatformUserID: existing.PlatformUserID,
			PlatformHandle: existing.PlatformHandle,
			Token:          existing.Token,
		}, false, nil
	}

	// Cart doesn't exist, create a new one
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("getting cart: %w", err)
	}

	// Create new cart with expiration 24h from now
	expiresAt := time.Now().Add(24 * time.Hour)
	created, err := r.q.CreateCart(ctx, sqlc.CreateCartParams{
		SessionID:      sessionID,
		PlatformUserID: params.PlatformUserID,
		PlatformHandle: params.PlatformHandle,
		Token:          params.Token,
		ExpiresAt:      pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, false, fmt.Errorf("creating cart: %w", err)
	}

	return &CartRow{
		ID:             created.ID.String(),
		SessionID:      created.SessionID.String(),
		PlatformUserID: created.PlatformUserID,
		PlatformHandle: created.PlatformHandle,
		Token:          created.Token,
	}, true, nil
}

// FinalizeCartsBySession marks all pending carts in a session as 'checkout'.
func (r *Repository) FinalizeCartsBySession(ctx context.Context, sessionID string) (int, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return 0, err
	}

	// Count first
	count, err := r.q.CountCartsBySession(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting carts: %w", err)
	}

	// Finalize
	if err := r.q.FinalizeCartsBySession(ctx, uid); err != nil {
		return 0, fmt.Errorf("finalizing carts: %w", err)
	}

	return int(count), nil
}

// AddCartItem adds a product to a cart (or increments quantity if already exists).
func (r *Repository) AddCartItem(ctx context.Context, params AddCartItemParams) error {
	cartID, err := parseUUID(params.CartID)
	if err != nil {
		return err
	}
	productID, err := parseUUID(params.ProductID)
	if err != nil {
		return err
	}

	_, err = r.q.UpsertCartItem(ctx, sqlc.UpsertCartItemParams{
		CartID:    cartID,
		ProductID: productID,
		Quantity:  pgtype.Int4{Int32: int32(params.Quantity), Valid: true},
		UnitPrice: pgtype.Int8{Int64: params.UnitPrice, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("upserting cart item: %w", err)
	}

	return nil
}

// =============================================================================
// PLATFORM AGGREGATION
// =============================================================================

// AddPlatformToSession adds a platform ID to a live session.
func (r *Repository) AddPlatformToSession(ctx context.Context, sessionID, platform, platformLiveID string) (*PlatformRow, error) {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
		SessionID:      sID,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
	})
	if err != nil {
		return nil, fmt.Errorf("adding platform to session: %w", err)
	}

	return &PlatformRow{
		ID:             row.ID.String(),
		SessionID:      row.SessionID.String(),
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt.Time,
	}, nil
}

// ListPlatformsBySession returns all platforms associated with a session.
func (r *Repository) ListPlatformsBySession(ctx context.Context, sessionID string) ([]PlatformRow, error) {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListPlatformsBySession(ctx, sID)
	if err != nil {
		return nil, fmt.Errorf("listing platforms: %w", err)
	}

	platforms := make([]PlatformRow, len(rows))
	for i, row := range rows {
		platforms[i] = PlatformRow{
			ID:             row.ID.String(),
			SessionID:      row.SessionID.String(),
			Platform:       row.Platform,
			PlatformLiveID: row.PlatformLiveID,
			AddedAt:        row.AddedAt.Time,
		}
	}

	return platforms, nil
}

// GetSessionByPlatformLiveID finds an active session by any associated platform_live_id.
func (r *Repository) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*LiveRow, error) {
	row, err := r.q.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting session by platform live id: %w", err)
	}

	result := toLiveRow(row)
	return &result, nil
}

// RemovePlatformFromSession removes a platform ID from a session.
func (r *Repository) RemovePlatformFromSession(ctx context.Context, sessionID, platformLiveID string) error {
	sID, err := parseUUID(sessionID)
	if err != nil {
		return err
	}

	return r.q.RemovePlatformFromSession(ctx, sqlc.RemovePlatformFromSessionParams{
		SessionID:      sID,
		PlatformLiveID: platformLiveID,
	})
}

// =============================================================================
// HELPERS
// =============================================================================

func toLiveRow(row sqlc.LiveSession) LiveRow {
	var title string
	if row.Title.Valid {
		title = row.Title.String
	}
	var platformLiveID string
	if row.PlatformLiveID.Valid {
		platformLiveID = row.PlatformLiveID.String
	}
	var startedAt *time.Time
	if row.StartedAt.Valid {
		startedAt = &row.StartedAt.Time
	}
	var endedAt *time.Time
	if row.EndedAt.Valid {
		endedAt = &row.EndedAt.Time
	}

	return LiveRow{
		ID:             row.ID.String(),
		StoreID:        row.StoreID.String(),
		Title:          title,
		Platform:       row.Platform,
		PlatformLiveID: platformLiveID,
		Status:         row.Status,
		StartedAt:      startedAt,
		EndedAt:        endedAt,
		TotalComments:  int(row.TotalComments.Int32),
		TotalOrders:    int(row.TotalOrders.Int32),
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
