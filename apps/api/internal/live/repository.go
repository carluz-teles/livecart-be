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

// =============================================================================
// EVENT OPERATIONS
// =============================================================================

func (r *Repository) CreateEvent(ctx context.Context, params CreateEventParams) (EventRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return EventRow{}, err
	}

	eventType := params.Type
	if eventType == "" {
		eventType = "single"
	}

	row, err := r.q.CreateLiveEvent(ctx, sqlc.CreateLiveEventParams{
		StoreID: storeUID,
		Title:   pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Type:    eventType,
		Status:  params.Status,
	})
	if err != nil {
		return EventRow{}, fmt.Errorf("creating live event: %w", err)
	}

	return toEventRow(row), nil
}

func (r *Repository) GetEventByID(ctx context.Context, id, storeID string) (*EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveEventByIDAndStore(ctx, sqlc.GetLiveEventByIDAndStoreParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("live event not found")
		}
		return nil, fmt.Errorf("getting live event: %w", err)
	}

	out := toEventRow(row)
	return &out, nil
}

func (r *Repository) GetActiveEventByStore(ctx context.Context, storeID string) (*EventRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetActiveLiveEventByStore(ctx, storeUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting active live event: %w", err)
	}

	out := toEventRow(row)
	return &out, nil
}

func (r *Repository) EndEvent(ctx context.Context, id, storeID string) (EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return EventRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.EndLiveEvent(ctx, sqlc.EndLiveEventParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("live event not found")
		}
		return EventRow{}, fmt.Errorf("ending live event: %w", err)
	}

	return toEventRow(row), nil
}

func (r *Repository) UpdateEventTitle(ctx context.Context, id, title string) (EventRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.UpdateLiveEventTitle(ctx, sqlc.UpdateLiveEventTitleParams{
		ID:    uid,
		Title: pgtype.Text{String: title, Valid: title != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("live event not found")
		}
		return EventRow{}, fmt.Errorf("updating live event title: %w", err)
	}

	return toEventRow(row), nil
}

func (r *Repository) ListEvents(ctx context.Context, storeID string, pagination, offset int) ([]EventRow, int, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.q.ListLiveEventsByStore(ctx, storeUID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing live events: %w", err)
	}

	events := make([]EventRow, len(rows))
	for i, row := range rows {
		events[i] = toEventRow(row)
	}

	return events, len(events), nil
}

func (r *Repository) GetEventByPlatformLiveID(ctx context.Context, platformLiveID string) (*EventRow, error) {
	row, err := r.q.GetEventByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting event by platform live id: %w", err)
	}

	out := toEventRow(row)
	return &out, nil
}

func (r *Repository) CountSessionsByEvent(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	count, err := r.q.CountSessionsByEvent(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting sessions: %w", err)
	}

	return int(count), nil
}

func (r *Repository) DeleteEvent(ctx context.Context, id, storeID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	result, err := r.pool.Exec(ctx, "DELETE FROM live_events WHERE id = $1 AND store_id = $2", uid, storeUID)
	if err != nil {
		return fmt.Errorf("deleting live event: %w", err)
	}

	if result.RowsAffected() == 0 {
		return httpx.ErrNotFound("live event not found")
	}

	return nil
}

// =============================================================================
// SESSION OPERATIONS
// =============================================================================

func (r *Repository) CreateSession(ctx context.Context, params CreateSessionParams) (SessionRow, error) {
	eventUID, err := parseUUID(params.EventID)
	if err != nil {
		return SessionRow{}, err
	}

	row, err := r.q.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
		EventID: eventUID,
		Status:  params.Status,
	})
	if err != nil {
		return SessionRow{}, fmt.Errorf("creating live session: %w", err)
	}

	return toSessionRow(row), nil
}

func (r *Repository) GetSessionByID(ctx context.Context, id string) (*SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveSessionByID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("live session not found")
		}
		return nil, fmt.Errorf("getting live session: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

func (r *Repository) GetActiveSessionByEvent(ctx context.Context, eventID string) (*SessionRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetActiveSessionByEvent(ctx, eventUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting active session: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

func (r *Repository) StartSession(ctx context.Context, id string) (SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return SessionRow{}, err
	}

	row, err := r.q.StartLiveSession(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("live session not found")
		}
		return SessionRow{}, fmt.Errorf("starting live session: %w", err)
	}

	return toSessionRow(row), nil
}

func (r *Repository) EndSession(ctx context.Context, id string) (SessionRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return SessionRow{}, err
	}

	row, err := r.q.EndLiveSession(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRow{}, httpx.ErrNotFound("live session not found")
		}
		return SessionRow{}, fmt.Errorf("ending live session: %w", err)
	}

	return toSessionRow(row), nil
}

func (r *Repository) ListSessionsByEvent(ctx context.Context, eventID string) ([]SessionRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListSessionsByEvent(ctx, eventUID)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}

	sessions := make([]SessionRow, len(rows))
	for i, row := range rows {
		sessions[i] = toSessionRow(row)
	}

	return sessions, nil
}

func (r *Repository) GetSessionByPlatformLiveID(ctx context.Context, platformLiveID string) (*SessionRow, error) {
	row, err := r.q.GetSessionByPlatformLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting session by platform live id: %w", err)
	}

	out := toSessionRow(row)
	return &out, nil
}

func (r *Repository) IncrementSessionComments(ctx context.Context, sessionID string) error {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return err
	}

	return r.q.IncrementLiveSessionComments(ctx, uid)
}

// =============================================================================
// PLATFORM OPERATIONS
// =============================================================================

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

func (r *Repository) GetPlatformByLiveID(ctx context.Context, platformLiveID string) (*PlatformRow, error) {
	row, err := r.q.GetPlatformByLiveID(ctx, platformLiveID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting platform by live id: %w", err)
	}

	return &PlatformRow{
		ID:             row.ID.String(),
		SessionID:      row.SessionID.String(),
		Platform:       row.Platform,
		PlatformLiveID: row.PlatformLiveID,
		AddedAt:        row.AddedAt.Time,
	}, nil
}

// =============================================================================
// STORE SETTINGS
// =============================================================================

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
// CART OPERATIONS (now use event_id)
// =============================================================================

func (r *Repository) GetOrCreateCart(ctx context.Context, params GetOrCreateCartParams) (*CartRow, bool, error) {
	eventID, err := parseUUID(params.EventID)
	if err != nil {
		return nil, false, err
	}

	// Try to get existing cart first
	existing, err := r.q.GetCartByEventAndUser(ctx, sqlc.GetCartByEventAndUserParams{
		EventID:        eventID,
		PlatformUserID: params.PlatformUserID,
	})
	if err == nil {
		return &CartRow{
			ID:             existing.ID.String(),
			EventID:        existing.EventID.String(),
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

	// Parse session ID if provided
	var sessionID pgtype.UUID
	if params.SessionID != nil {
		sid, err := parseUUID(*params.SessionID)
		if err != nil {
			return nil, false, fmt.Errorf("parsing session ID: %w", err)
		}
		sessionID = sid
	}

	created, err := r.q.CreateCart(ctx, sqlc.CreateCartParams{
		EventID:        eventID,
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
		EventID:        created.EventID.String(),
		PlatformUserID: created.PlatformUserID,
		PlatformHandle: created.PlatformHandle,
		Token:          created.Token,
	}, true, nil
}

func (r *Repository) FinalizeCartsByEvent(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	// Count first
	count, err := r.q.CountCartsByEvent(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting carts: %w", err)
	}

	// Finalize
	if err := r.q.FinalizeCartsByEvent(ctx, uid); err != nil {
		return 0, fmt.Errorf("finalizing carts: %w", err)
	}

	return int(count), nil
}

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
		CartID:     cartID,
		ProductID:  productID,
		Quantity:   pgtype.Int4{Int32: int32(params.Quantity), Valid: true},
		UnitPrice:  pgtype.Int8{Int64: params.UnitPrice, Valid: true},
		Waitlisted: params.Waitlisted,
	})
	if err != nil {
		return fmt.Errorf("upserting cart item: %w", err)
	}

	return nil
}

// =============================================================================
// STATS (now from events)
// =============================================================================

func (r *Repository) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveStatsOutput{}, err
	}

	query := `
		SELECT
			COUNT(*) as total_lives,
			COUNT(*) FILTER (WHERE status = 'active') as active_lives,
			COALESCE(SUM(total_orders), 0) as total_orders
		FROM live_events
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
// LEGACY LIST (joins events with sessions and platforms)
// =============================================================================

type ListLivesParams struct {
	StoreID    string
	Search     string
	Pagination struct {
		Limit  int
		Offset int
	}
	Sorting struct {
		SortBy    string
		SortOrder string
	}
	Filters LiveFilters
}

func (r *Repository) ListLives(ctx context.Context, params ListLivesParams) ([]LiveOutput, int, error) {
	// Build WHERE conditions
	conditions := []string{"e.store_id = $1"}
	args := []interface{}{params.StoreID}
	argIdx := 2

	// Search filter (title)
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("LOWER(e.title) LIKE $%d", argIdx))
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
		conditions = append(conditions, fmt.Sprintf("e.status IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Date filters
	if params.Filters.DateFrom != nil && *params.Filters.DateFrom != "" {
		conditions = append(conditions, fmt.Sprintf("e.created_at >= $%d", argIdx))
		args = append(args, *params.Filters.DateFrom)
		argIdx++
	}
	if params.Filters.DateTo != nil && *params.Filters.DateTo != "" {
		conditions = append(conditions, fmt.Sprintf("e.created_at <= $%d", argIdx))
		args = append(args, *params.Filters.DateTo)
		argIdx++
	}

	whereClause := strings.Join(conditions, " AND ")

	// Validate and build ORDER BY
	allowedSortFields := map[string]string{
		"title":      "e.title",
		"status":     "e.status",
		"created_at": "e.created_at",
	}
	sortField, ok := allowedSortFields[params.Sorting.SortBy]
	if !ok {
		sortField = "e.created_at"
	}
	sortOrder := "DESC"
	if strings.ToUpper(params.Sorting.SortOrder) == "ASC" {
		sortOrder = "ASC"
	}
	orderClause := fmt.Sprintf("%s %s", sortField, sortOrder)

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM live_events e WHERE %s", whereClause)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting live events: %w", err)
	}

	// Build main query with pagination
	// Join with sessions to get the first session's start/end times
	// Join with platforms to get the primary platform
	query := fmt.Sprintf(`
		SELECT
			e.id, e.store_id, e.title, e.type, e.status, e.total_orders, e.created_at, e.updated_at,
			s.started_at, s.ended_at, COALESCE(s.total_comments, 0),
			COALESCE(p.platform, ''), COALESCE(p.platform_live_id, '')
		FROM live_events e
		LEFT JOIN LATERAL (
			SELECT id, started_at, ended_at, total_comments
			FROM live_sessions
			WHERE event_id = e.id
			ORDER BY created_at ASC
			LIMIT 1
		) s ON true
		LEFT JOIN LATERAL (
			SELECT platform, platform_live_id
			FROM live_session_platforms
			WHERE session_id = s.id
			ORDER BY added_at ASC
			LIMIT 1
		) p ON true
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderClause, argIdx, argIdx+1)

	args = append(args, params.Pagination.Limit, params.Pagination.Offset)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing live events: %w", err)
	}
	defer rows.Close()

	lives := make([]LiveOutput, 0)
	for rows.Next() {
		var live LiveOutput
		var title, eventType, platform, platformLiveID pgtype.Text
		var startedAt, endedAt pgtype.Timestamptz

		if err := rows.Scan(
			&live.ID,
			&live.StoreID,
			&title,
			&eventType,
			&live.Status,
			&live.TotalOrders,
			&live.CreatedAt,
			&live.UpdatedAt,
			&startedAt,
			&endedAt,
			&live.TotalComments,
			&platform,
			&platformLiveID,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning live event: %w", err)
		}

		if title.Valid {
			live.Title = title.String
		}
		if eventType.Valid {
			live.Type = eventType.String
		} else {
			live.Type = "single"
		}
		if platform.Valid {
			live.Platform = platform.String
		}
		if platformLiveID.Valid {
			live.PlatformLiveID = platformLiveID.String
		}
		if startedAt.Valid {
			live.StartedAt = &startedAt.Time
		}
		if endedAt.Valid {
			live.EndedAt = &endedAt.Time
		}

		lives = append(lives, live)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating live events: %w", err)
	}

	return lives, total, nil
}

// =============================================================================
// HELPERS
// =============================================================================

func toEventRow(row sqlc.LiveEvent) EventRow {
	var title string
	if row.Title.Valid {
		title = row.Title.String
	}

	eventType := row.Type
	if eventType == "" {
		eventType = "single"
	}

	return EventRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		Title:       title,
		Type:        eventType,
		Status:      row.Status,
		TotalOrders: int(row.TotalOrders),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

func toSessionRow(row sqlc.LiveSession) SessionRow {
	var startedAt, endedAt *time.Time
	if row.StartedAt.Valid {
		startedAt = &row.StartedAt.Time
	}
	if row.EndedAt.Valid {
		endedAt = &row.EndedAt.Time
	}

	return SessionRow{
		ID:            row.ID.String(),
		EventID:       row.EventID.String(),
		Status:        row.Status,
		StartedAt:     startedAt,
		EndedAt:       endedAt,
		TotalComments: int(row.TotalComments.Int32),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

// =============================================================================
// EVENT DETAILS - Stats and Cart Listing
// =============================================================================

func (r *Repository) GetEventStats(ctx context.Context, eventID string) (EventStatsRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return EventStatsRow{}, err
	}

	row, err := r.q.GetEventStats(ctx, uid)
	if err != nil {
		return EventStatsRow{}, fmt.Errorf("getting event stats: %w", err)
	}

	return EventStatsRow{
		TotalComments:     int(row.TotalComments),
		OpenCarts:         int(row.OpenCarts),
		PaidCarts:         int(row.PaidCarts),
		TotalProductsSold: int(row.TotalProductsSold),
		ProjectedRevenue:  row.ProjectedRevenue,
		ConfirmedRevenue:  row.ConfirmedRevenue,
	}, nil
}

func (r *Repository) ListCartsWithTotalByEvent(ctx context.Context, eventID string) ([]CartWithTotalRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListCartsWithTotalByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing carts with total: %w", err)
	}

	carts := make([]CartWithTotalRow, len(rows))
	for i, row := range rows {
		var paymentStatus *string
		if row.PaymentStatus.Valid {
			paymentStatus = &row.PaymentStatus.String
		}
		var expiresAt *time.Time
		if row.ExpiresAt.Valid {
			expiresAt = &row.ExpiresAt.Time
		}

		carts[i] = CartWithTotalRow{
			ID:             row.ID.String(),
			EventID:        row.EventID.String(),
			PlatformUserID: row.PlatformUserID,
			PlatformHandle: row.PlatformHandle,
			Status:         row.Status,
			PaymentStatus:  paymentStatus,
			TotalValue:     row.TotalValue,
			TotalItems:     int(row.TotalItems),
			CreatedAt:      row.CreatedAt.Time,
			ExpiresAt:      expiresAt,
		}
	}

	return carts, nil
}

func (r *Repository) ListProductsByEvent(ctx context.Context, eventID string) ([]EventProductRow, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListProductsByEvent(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing products by event: %w", err)
	}

	products := make([]EventProductRow, len(rows))
	for i, row := range rows {
		var imageURL *string
		if row.ImageUrl.Valid {
			imageURL = &row.ImageUrl.String
		}

		products[i] = EventProductRow{
			ID:            row.ID.String(),
			Name:          row.Name,
			ImageURL:      imageURL,
			Keyword:       row.Keyword,
			TotalQuantity: int(row.TotalQuantity),
			TotalRevenue:  row.TotalRevenue,
		}
	}

	return products, nil
}

// SessionStatsRow holds cart statistics for a session
type SessionStatsRow struct {
	TotalCarts   int
	PaidCarts    int
	TotalRevenue int64
	PaidRevenue  int64
}

func (r *Repository) GetSessionStats(ctx context.Context, sessionID string) (*SessionStatsRow, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetSessionStats(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("getting session stats: %w", err)
	}

	return &SessionStatsRow{
		TotalCarts:   int(row.TotalCarts),
		PaidCarts:    int(row.PaidCarts),
		TotalRevenue: row.TotalRevenue,
		PaidRevenue:  row.PaidRevenue,
	}, nil
}
