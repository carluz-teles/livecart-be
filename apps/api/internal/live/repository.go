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

// CreateSessionWithPlatformTx creates a session and adds a platform in a single transaction.
// This ensures atomicity - either both operations succeed or both are rolled back.
func (r *Repository) CreateSessionWithPlatformTx(ctx context.Context, eventID, platform, platformLiveID string) (SessionRow, *PlatformRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return SessionRow{}, nil, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return SessionRow{}, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// Create the session
	sessionRow, err := qtx.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
		EventID: eventUID,
		Status:  "active",
	})
	if err != nil {
		return SessionRow{}, nil, fmt.Errorf("creating live session: %w", err)
	}

	// Add the platform to the session
	platformRow, err := qtx.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
		SessionID:      sessionRow.ID,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
	})
	if err != nil {
		return SessionRow{}, nil, fmt.Errorf("adding platform to session: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return SessionRow{}, nil, fmt.Errorf("committing transaction: %w", err)
	}

	session := toSessionRow(sessionRow)
	platformOut := &PlatformRow{
		ID:             platformRow.ID.String(),
		SessionID:      platformRow.SessionID.String(),
		Platform:       platformRow.Platform,
		PlatformLiveID: platformRow.PlatformLiveID,
		AddedAt:        platformRow.AddedAt.Time,
	}

	return session, platformOut, nil
}

// CreateEventWithSessionTx creates an event, session, and platform in a single transaction.
// This ensures atomicity - either all operations succeed or all are rolled back.
func (r *Repository) CreateEventWithSessionTx(ctx context.Context, params CreateEventParams, platform, platformLiveID string) (EventRow, SessionRow, *PlatformRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return EventRow{}, SessionRow{}, nil, err
	}

	eventType := params.Type
	if eventType == "" {
		eventType = "single"
	}

	// Convert nullable ints to pgtype.Int4
	var cartExpirationMinutes, cartMaxQuantityPerItem pgtype.Int4
	if params.CartExpirationMinutes != nil {
		cartExpirationMinutes = pgtype.Int4{Int32: int32(*params.CartExpirationMinutes), Valid: true}
	}
	if params.CartMaxQuantityPerItem != nil {
		cartMaxQuantityPerItem = pgtype.Int4{Int32: int32(*params.CartMaxQuantityPerItem), Valid: true}
	}

	// Convert nullable bool to pgtype.Bool
	var autoSendCheckoutLinks pgtype.Bool
	if params.SendOnLiveEnd != nil {
		autoSendCheckoutLinks = pgtype.Bool{Bool: *params.SendOnLiveEnd, Valid: true}
	}

	// Convert scheduling fields
	var scheduledAt pgtype.Timestamptz
	if params.ScheduledAt != nil {
		scheduledAt = pgtype.Timestamptz{Time: *params.ScheduledAt, Valid: true}
	}
	var description pgtype.Text
	if params.Description != nil {
		description = pgtype.Text{String: *params.Description, Valid: true}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// 1. Create the event
	eventRow, err := qtx.CreateLiveEventFull(ctx, sqlc.CreateLiveEventFullParams{
		StoreID:                storeUID,
		Title:                  pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Type:                   eventType,
		Status:                 params.Status,
		CloseCartOnEventEnd:    params.CloseCartOnEventEnd,
		CartExpirationMinutes:  cartExpirationMinutes,
		CartMaxQuantityPerItem: cartMaxQuantityPerItem,
		SendOnLiveEnd:          autoSendCheckoutLinks,
		ScheduledAt:            scheduledAt,
		Description:            description,
	})
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("creating live event: %w", err)
	}

	// 2. Create the session
	sessionRow, err := qtx.CreateLiveSession(ctx, sqlc.CreateLiveSessionParams{
		EventID: eventRow.ID,
		Status:  "active",
	})
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("creating live session: %w", err)
	}

	// 3. Add the platform to the session
	platformRow, err := qtx.AddPlatformToSession(ctx, sqlc.AddPlatformToSessionParams{
		SessionID:      sessionRow.ID,
		Platform:       platform,
		PlatformLiveID: platformLiveID,
	})
	if err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("adding platform to session: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventRow{}, SessionRow{}, nil, fmt.Errorf("committing transaction: %w", err)
	}

	event := toEventRow(eventRow)
	session := toSessionRow(sessionRow)
	platformOut := &PlatformRow{
		ID:             platformRow.ID.String(),
		SessionID:      platformRow.SessionID.String(),
		Platform:       platformRow.Platform,
		PlatformLiveID: platformRow.PlatformLiveID,
		AddedAt:        platformRow.AddedAt.Time,
	}

	return event, session, platformOut, nil
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

	// Convert nullable ints to pgtype.Int4
	var cartExpirationMinutes, cartMaxQuantityPerItem pgtype.Int4
	if params.CartExpirationMinutes != nil {
		cartExpirationMinutes = pgtype.Int4{Int32: int32(*params.CartExpirationMinutes), Valid: true}
	}
	if params.CartMaxQuantityPerItem != nil {
		cartMaxQuantityPerItem = pgtype.Int4{Int32: int32(*params.CartMaxQuantityPerItem), Valid: true}
	}

	// Convert nullable bool to pgtype.Bool
	var autoSendCheckoutLinks pgtype.Bool
	if params.SendOnLiveEnd != nil {
		autoSendCheckoutLinks = pgtype.Bool{Bool: *params.SendOnLiveEnd, Valid: true}
	}

	// Convert scheduling fields
	var scheduledAt pgtype.Timestamptz
	if params.ScheduledAt != nil {
		scheduledAt = pgtype.Timestamptz{Time: *params.ScheduledAt, Valid: true}
	}
	var description pgtype.Text
	if params.Description != nil {
		description = pgtype.Text{String: *params.Description, Valid: true}
	}

	// Use CreateLiveEventFull to include scheduling fields
	row, err := r.q.CreateLiveEventFull(ctx, sqlc.CreateLiveEventFullParams{
		StoreID:                storeUID,
		Title:                  pgtype.Text{String: params.Title, Valid: params.Title != ""},
		Type:                   eventType,
		Status:                 params.Status,
		CloseCartOnEventEnd:    params.CloseCartOnEventEnd,
		CartExpirationMinutes:  cartExpirationMinutes,
		CartMaxQuantityPerItem: cartMaxQuantityPerItem,
		SendOnLiveEnd:          autoSendCheckoutLinks,
		ScheduledAt:            scheduledAt,
		Description:            description,
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

// ListCommentsBySession returns all comments for a session.
func (r *Repository) ListCommentsBySession(ctx context.Context, sessionID string, limit, offset int) ([]CommentRow, error) {
	uid, err := parseUUID(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListCommentsBySession(ctx, sqlc.ListCommentsBySessionParams{
		SessionID: uid,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("listing comments by session: %w", err)
	}

	comments := make([]CommentRow, 0, len(rows))
	for _, row := range rows {
		comments = append(comments, CommentRow{
			ID:             row.ID.String(),
			SessionID:      row.SessionID.String(),
			PlatformHandle: row.PlatformHandle,
			Text:           row.Text,
			CreatedAt:      row.CreatedAt.Time,
		})
	}

	return comments, nil
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

	return store.SendOnLiveEnd, nil
}

// =============================================================================
// CART OPERATIONS (now use event_id)
// =============================================================================

func (r *Repository) GetOrCreateCart(ctx context.Context, params GetOrCreateCartParams) (*CartRow, bool, error) {
	eventID, err := parseUUID(params.EventID)
	if err != nil {
		return nil, false, err
	}

	// Parse session ID if provided (before transaction)
	var sessionID pgtype.UUID
	if params.SessionID != nil {
		sid, err := parseUUID(*params.SessionID)
		if err != nil {
			return nil, false, fmt.Errorf("parsing session ID: %w", err)
		}
		sessionID = sid
	}

	// Parse customer ID if provided
	var customerID pgtype.UUID
	if params.CustomerID != nil {
		cid, err := parseUUID(*params.CustomerID)
		if err != nil {
			return nil, false, fmt.Errorf("parsing customer ID: %w", err)
		}
		customerID = cid
	}

	// Use transaction to ensure atomicity (SELECT + INSERT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// Try to get existing cart first
	existing, err := qtx.GetCartByEventAndUser(ctx, sqlc.GetCartByEventAndUserParams{
		EventID:        eventID,
		PlatformUserID: params.PlatformUserID,
	})
	if err == nil {
		// Cart exists, commit and return
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("committing transaction: %w", err)
		}
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

	// Note: expires_at is NOT set on creation. It will be set when the live event ends.
	created, err := qtx.CreateCart(ctx, sqlc.CreateCartParams{
		EventID:        eventID,
		SessionID:      sessionID,
		PlatformUserID: params.PlatformUserID,
		PlatformHandle: params.PlatformHandle,
		Token:          params.Token,
		CustomerID:     customerID,
	})
	if err != nil {
		return nil, false, fmt.Errorf("creating cart: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("committing transaction: %w", err)
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

	// Use transaction to ensure atomicity (COUNT + UPDATE)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	// Count first
	count, err := qtx.CountCartsByEvent(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting carts: %w", err)
	}

	// Finalize
	if err := qtx.FinalizeCartsByEvent(ctx, uid); err != nil {
		return 0, fmt.Errorf("finalizing carts: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
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
		CartID:             cartID,
		ProductID:          productID,
		Quantity:           pgtype.Int4{Int32: int32(params.Quantity), Valid: true},
		UnitPrice:          pgtype.Int8{Int64: params.UnitPrice, Valid: true},
		WaitlistedQuantity: int32(params.WaitlistedQuantity),
	})
	if err != nil {
		return fmt.Errorf("upserting cart item: %w", err)
	}

	return nil
}

// GetCartTotals returns total items and value for a cart.
func (r *Repository) GetCartTotals(ctx context.Context, cartID string) (int, int64, error) {
	uid, err := parseUUID(cartID)
	if err != nil {
		return 0, 0, err
	}

	row, err := r.q.GetCartTotals(ctx, uid)
	if err != nil {
		return 0, 0, fmt.Errorf("getting cart totals: %w", err)
	}

	return int(row.TotalItems), row.TotalValue, nil
}

// =============================================================================
// STATS (now from events)
// =============================================================================

func (r *Repository) GetStats(ctx context.Context, storeID string) (LiveStatsOutput, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return LiveStatsOutput{}, err
	}

	// total_revenue mirrors dashboard.Repository.GetStats: sum of every
	// cart item across every cart attached to this store's events, with no
	// payment-status filter. Keeping the two surfaces in sync so the card on
	// /events matches "Faturamento Total" on the dashboard.
	query := `
		SELECT
			COUNT(*) as total_lives,
			COUNT(*) FILTER (WHERE status = 'active') as active_lives,
			COALESCE(SUM(total_orders), 0) as total_orders,
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM cart_items ci
				JOIN carts c ON c.id = ci.cart_id
				JOIN live_events le ON le.id = c.event_id
				WHERE le.store_id = $1
			), 0)::BIGINT as total_revenue
		FROM live_events
		WHERE store_id = $1
	`

	var stats LiveStatsOutput
	err = r.pool.QueryRow(ctx, query, storeUID).Scan(
		&stats.TotalLives,
		&stats.ActiveLives,
		&stats.TotalOrders,
		&stats.TotalRevenue,
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
			e.close_cart_on_event_end, e.cart_expiration_minutes, e.cart_max_quantity_per_item, e.send_on_live_end,
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
		var cartExpirationMinutes, cartMaxQuantityPerItem pgtype.Int4
		var autoSendCheckoutLinks pgtype.Bool

		if err := rows.Scan(
			&live.ID,
			&live.StoreID,
			&title,
			&eventType,
			&live.Status,
			&live.TotalOrders,
			&live.CreatedAt,
			&live.UpdatedAt,
			&live.CloseCartOnEventEnd,
			&cartExpirationMinutes,
			&cartMaxQuantityPerItem,
			&autoSendCheckoutLinks,
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
		if cartExpirationMinutes.Valid {
			v := int(cartExpirationMinutes.Int32)
			live.CartExpirationMinutes = &v
		}
		if cartMaxQuantityPerItem.Valid {
			v := int(cartMaxQuantityPerItem.Int32)
			live.CartMaxQuantityPerItem = &v
		}
		if autoSendCheckoutLinks.Valid {
			live.SendOnLiveEnd = &autoSendCheckoutLinks.Bool
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

	// Convert nullable fields
	var cartExpirationMinutes, cartMaxQuantityPerItem *int
	if row.CartExpirationMinutes.Valid {
		v := int(row.CartExpirationMinutes.Int32)
		cartExpirationMinutes = &v
	}
	if row.CartMaxQuantityPerItem.Valid {
		v := int(row.CartMaxQuantityPerItem.Int32)
		cartMaxQuantityPerItem = &v
	}
	var autoSendCheckoutLinks *bool
	if row.SendOnLiveEnd.Valid {
		autoSendCheckoutLinks = &row.SendOnLiveEnd.Bool
	}
	var currentActiveProductID *string
	if row.CurrentActiveProductID.Valid {
		id := row.CurrentActiveProductID.String()
		currentActiveProductID = &id
	}

	// Scheduling fields
	var scheduledAt *time.Time
	if row.ScheduledAt.Valid {
		scheduledAt = &row.ScheduledAt.Time
	}
	var description *string
	if row.Description.Valid {
		description = &row.Description.String
	}

	return EventRow{
		ID:                      row.ID.String(),
		StoreID:                 row.StoreID.String(),
		Title:                   title,
		Type:                    eventType,
		Status:                  row.Status,
		TotalOrders:             int(row.TotalOrders),
		CloseCartOnEventEnd:     row.CloseCartOnEventEnd,
		CartExpirationMinutes:   cartExpirationMinutes,
		CartMaxQuantityPerItem:  cartMaxQuantityPerItem,
		SendOnLiveEnd:           autoSendCheckoutLinks,
		CurrentActiveProductID:  currentActiveProductID,
		ProcessingPaused:        row.ProcessingPaused,
		ScheduledAt:             scheduledAt,
		Description:             description,
		CreatedAt:               row.CreatedAt.Time,
		UpdatedAt:               row.UpdatedAt.Time,
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
		TotalCarts:        int(row.TotalCarts),
		OpenCarts:         int(row.OpenCarts),
		CheckoutCarts:     int(row.CheckoutCarts),
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
		var sessionID *string
		if row.SessionID.Valid {
			s := row.SessionID.String()
			sessionID = &s
		}
		var paymentStatus *string
		if row.PaymentStatus.Valid {
			paymentStatus = &row.PaymentStatus.String
		}
		var expiresAt *time.Time
		if row.ExpiresAt.Valid {
			expiresAt = &row.ExpiresAt.Time
		}

		carts[i] = CartWithTotalRow{
			ID:              row.ID.String(),
			EventID:         row.EventID.String(),
			SessionID:       sessionID,
			PlatformUserID:  row.PlatformUserID,
			PlatformHandle:  row.PlatformHandle,
			Token:           row.Token,
			Status:          row.Status,
			PaymentStatus:   paymentStatus,
			TotalValue:      row.TotalValue,
			TotalItems:      int(row.TotalItems),
			AvailableItems:  int(row.AvailableItems),
			WaitlistedItems: int(row.WaitlistedItems),
			CreatedAt:       row.CreatedAt.Time,
			ExpiresAt:       expiresAt,
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

// =============================================================================
// LIVE MODE - Active Product and Processing Control
// =============================================================================

// SetActiveProduct sets the active product for an event
func (r *Repository) SetActiveProduct(ctx context.Context, eventID, storeID, productID string) (EventRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return EventRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return EventRow{}, err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.SetActiveProduct(ctx, sqlc.SetActiveProductParams{
		ID:                     eventUID,
		CurrentActiveProductID: productUID,
		StoreID:                storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("event not found")
		}
		return EventRow{}, fmt.Errorf("setting active product: %w", err)
	}

	return toEventRow(row), nil
}

// ClearActiveProduct clears the active product for an event
func (r *Repository) ClearActiveProduct(ctx context.Context, eventID, storeID string) (EventRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return EventRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.ClearActiveProduct(ctx, sqlc.ClearActiveProductParams{
		ID:      eventUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("event not found")
		}
		return EventRow{}, fmt.Errorf("clearing active product: %w", err)
	}

	return toEventRow(row), nil
}

// SetProcessingPaused sets the processing paused state for an event
func (r *Repository) SetProcessingPaused(ctx context.Context, eventID, storeID string, paused bool) (EventRow, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return EventRow{}, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return EventRow{}, err
	}

	row, err := r.q.SetProcessingPaused(ctx, sqlc.SetProcessingPausedParams{
		ID:               eventUID,
		ProcessingPaused: paused,
		StoreID:          storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventRow{}, httpx.ErrNotFound("event not found")
		}
		return EventRow{}, fmt.Errorf("setting processing paused: %w", err)
	}

	return toEventRow(row), nil
}

// GetLiveModeState returns the live mode state for an event
func (r *Repository) GetLiveModeState(ctx context.Context, eventID, storeID string) (*LiveModeStateOutput, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetLiveModeState(ctx, sqlc.GetLiveModeStateParams{
		ID:      eventUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("event not found")
		}
		return nil, fmt.Errorf("getting live mode state: %w", err)
	}

	output := &LiveModeStateOutput{
		ProcessingPaused: row.ProcessingPaused,
	}

	// Include active product if set
	if row.CurrentActiveProductID.Valid && row.ActiveProductName.Valid {
		var imageURL *string
		if row.ActiveProductImageUrl.Valid {
			imageURL = &row.ActiveProductImageUrl.String
		}

		output.ActiveProduct = &ActiveProductOutput{
			ID:       row.CurrentActiveProductID.String(),
			Name:     row.ActiveProductName.String,
			Keyword:  row.ActiveProductKeyword.String,
			Price:    row.ActiveProductPrice.Int64,
			ImageURL: imageURL,
		}
	}

	return output, nil
}

// =============================================================================
// EVENT PRODUCTS (Whitelist)
// =============================================================================

// AddEventProduct adds a product to an event's whitelist
func (r *Repository) AddEventProduct(ctx context.Context, input AddEventProductInput) (EventProductOutput, error) {
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventProductOutput{}, err
	}
	productUID, err := parseUUID(input.ProductID)
	if err != nil {
		return EventProductOutput{}, err
	}

	// Convert nullable fields
	var specialPrice pgtype.Int4
	if input.SpecialPrice != nil {
		specialPrice = pgtype.Int4{Int32: int32(*input.SpecialPrice), Valid: true}
	}
	var maxQuantity pgtype.Int4
	if input.MaxQuantity != nil {
		maxQuantity = pgtype.Int4{Int32: *input.MaxQuantity, Valid: true}
	}

	// Use transaction to ensure atomicity (INSERT + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventProductOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	_, err = qtx.CreateEventProduct(ctx, sqlc.CreateEventProductParams{
		EventID:      eventUID,
		ProductID:    productUID,
		SpecialPrice: specialPrice,
		MaxQuantity:  maxQuantity,
		DisplayOrder: input.DisplayOrder,
		Featured:     input.Featured,
	})
	if err != nil {
		return EventProductOutput{}, fmt.Errorf("adding event product: %w", err)
	}

	// Get the created product with joined product data
	row, err := qtx.GetEventProductByProductID(ctx, sqlc.GetEventProductByProductIDParams{
		EventID:   eventUID,
		ProductID: productUID,
	})
	if err != nil {
		return EventProductOutput{}, fmt.Errorf("getting created event product: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventProductOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventProductOutput(row), nil
}

// ListEventProducts returns all products in an event's whitelist
func (r *Repository) ListEventProducts(ctx context.Context, eventID string) ([]EventProductOutput, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListEventProducts(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing event products: %w", err)
	}

	products := make([]EventProductOutput, len(rows))
	for i, row := range rows {
		products[i] = toEventProductOutputFromList(row)
	}

	return products, nil
}

// UpdateEventProduct updates a product's configuration in an event
func (r *Repository) UpdateEventProduct(ctx context.Context, input UpdateEventProductInput) (EventProductOutput, error) {
	uid, err := parseUUID(input.ID)
	if err != nil {
		return EventProductOutput{}, err
	}
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventProductOutput{}, err
	}

	// Convert nullable fields
	var specialPrice pgtype.Int4
	if input.SpecialPrice != nil {
		specialPrice = pgtype.Int4{Int32: int32(*input.SpecialPrice), Valid: true}
	}
	var maxQuantity pgtype.Int4
	if input.MaxQuantity != nil {
		maxQuantity = pgtype.Int4{Int32: *input.MaxQuantity, Valid: true}
	}

	// Use transaction to ensure atomicity (UPDATE + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventProductOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	updated, err := qtx.UpdateEventProduct(ctx, sqlc.UpdateEventProductParams{
		ID:           uid,
		EventID:      eventUID,
		SpecialPrice: specialPrice,
		MaxQuantity:  maxQuantity,
		DisplayOrder: input.DisplayOrder,
		Featured:     input.Featured,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventProductOutput{}, httpx.ErrNotFound("event product not found")
		}
		return EventProductOutput{}, fmt.Errorf("updating event product: %w", err)
	}

	// Get with joined product data
	row, err := qtx.GetEventProductByID(ctx, updated.ID)
	if err != nil {
		return EventProductOutput{}, fmt.Errorf("getting updated event product: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventProductOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventProductOutputFromGet(row), nil
}

// DeleteEventProduct removes a product from an event's whitelist
func (r *Repository) DeleteEventProduct(ctx context.Context, id, eventID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}

	return r.q.DeleteEventProduct(ctx, sqlc.DeleteEventProductParams{
		ID:      uid,
		EventID: eventUID,
	})
}

// CountEventProducts returns the number of products in an event's whitelist
func (r *Repository) CountEventProducts(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	count, err := r.q.CountEventProducts(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting event products: %w", err)
	}

	return int(count), nil
}

// GetEventProductConfig returns product config for cart validation
func (r *Repository) GetEventProductConfig(ctx context.Context, eventID, productID, storeID string) (*ProductValidationResult, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}
	productUID, err := parseUUID(productID)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetEventProductConfig(ctx, sqlc.GetEventProductConfigParams{
		EventID: eventUID,
		ID:      productUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product not found")
		}
		return nil, fmt.Errorf("getting event product config: %w", err)
	}

	var specialPrice *int64
	if row.SpecialPrice.Valid {
		v := int64(row.SpecialPrice.Int32)
		specialPrice = &v
	}
	var maxQuantity *int32
	if row.MaxQuantity.Valid {
		maxQuantity = &row.MaxQuantity.Int32
	}

	return &ProductValidationResult{
		ProductID:      row.ProductID.String(),
		ProductName:    row.ProductName,
		Keyword:        row.ProductKeyword,
		OriginalPrice:  row.OriginalPrice.Int64,
		EffectivePrice: row.EffectivePrice,
		SpecialPrice:   specialPrice,
		MaxQuantity:    maxQuantity,
		Stock:          row.ProductStock.Int32,
		IsAllowed:      row.IsAllowed,
		IsActive:       row.ProductActive.Bool,
	}, nil
}

// =============================================================================
// EVENT UPSELLS
// =============================================================================

// AddEventUpsell adds an upsell to an event
func (r *Repository) AddEventUpsell(ctx context.Context, input AddEventUpsellInput) (EventUpsellOutput, error) {
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventUpsellOutput{}, err
	}
	productUID, err := parseUUID(input.ProductID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	var messageTemplate pgtype.Text
	if input.MessageTemplate != nil {
		messageTemplate = pgtype.Text{String: *input.MessageTemplate, Valid: true}
	}

	// Use transaction to ensure atomicity (INSERT + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	created, err := qtx.CreateEventUpsell(ctx, sqlc.CreateEventUpsellParams{
		EventID:         eventUID,
		ProductID:       productUID,
		DiscountPercent: input.DiscountPercent,
		MessageTemplate: messageTemplate,
		DisplayOrder:    input.DisplayOrder,
		Active:          input.Active,
	})
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("adding event upsell: %w", err)
	}

	// Get with joined product data
	row, err := qtx.GetEventUpsellByID(ctx, created.ID)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("getting created event upsell: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventUpsellOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventUpsellOutputFromGet(row), nil
}

// ListEventUpsells returns all upsells for an event
func (r *Repository) ListEventUpsells(ctx context.Context, eventID string) ([]EventUpsellOutput, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListEventUpsells(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing event upsells: %w", err)
	}

	upsells := make([]EventUpsellOutput, len(rows))
	for i, row := range rows {
		upsells[i] = toEventUpsellOutputFromList(row)
	}

	return upsells, nil
}

// UpdateEventUpsell updates an upsell's configuration
func (r *Repository) UpdateEventUpsell(ctx context.Context, input UpdateEventUpsellInput) (EventUpsellOutput, error) {
	uid, err := parseUUID(input.ID)
	if err != nil {
		return EventUpsellOutput{}, err
	}
	eventUID, err := parseUUID(input.EventID)
	if err != nil {
		return EventUpsellOutput{}, err
	}

	var messageTemplate pgtype.Text
	if input.MessageTemplate != nil {
		messageTemplate = pgtype.Text{String: *input.MessageTemplate, Valid: true}
	}

	// Use transaction to ensure atomicity (UPDATE + SELECT)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) // No-op if already committed

	qtx := r.q.WithTx(tx)

	updated, err := qtx.UpdateEventUpsell(ctx, sqlc.UpdateEventUpsellParams{
		ID:              uid,
		EventID:         eventUID,
		DiscountPercent: input.DiscountPercent,
		MessageTemplate: messageTemplate,
		DisplayOrder:    input.DisplayOrder,
		Active:          input.Active,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EventUpsellOutput{}, httpx.ErrNotFound("event upsell not found")
		}
		return EventUpsellOutput{}, fmt.Errorf("updating event upsell: %w", err)
	}

	// Get with joined product data
	row, err := qtx.GetEventUpsellByID(ctx, updated.ID)
	if err != nil {
		return EventUpsellOutput{}, fmt.Errorf("getting updated event upsell: %w", err)
	}

	// Commit the transaction
	if err := tx.Commit(ctx); err != nil {
		return EventUpsellOutput{}, fmt.Errorf("committing transaction: %w", err)
	}

	return toEventUpsellOutputFromGet(row), nil
}

// DeleteEventUpsell removes an upsell from an event
func (r *Repository) DeleteEventUpsell(ctx context.Context, id, eventID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return err
	}

	return r.q.DeleteEventUpsell(ctx, sqlc.DeleteEventUpsellParams{
		ID:      uid,
		EventID: eventUID,
	})
}

// CountEventUpsells returns the number of upsells for an event
func (r *Repository) CountEventUpsells(ctx context.Context, eventID string) (int, error) {
	uid, err := parseUUID(eventID)
	if err != nil {
		return 0, err
	}

	count, err := r.q.CountEventUpsells(ctx, uid)
	if err != nil {
		return 0, fmt.Errorf("counting event upsells: %w", err)
	}

	return int(count), nil
}

// GetEventWithCounts returns an event with product and upsell counts
func (r *Repository) GetEventWithCounts(ctx context.Context, eventID, storeID string) (*EventRow, int, int, error) {
	eventUID, err := parseUUID(eventID)
	if err != nil {
		return nil, 0, 0, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, 0, 0, err
	}

	row, err := r.q.GetLiveEventWithCounts(ctx, sqlc.GetLiveEventWithCountsParams{
		ID:      eventUID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, 0, httpx.ErrNotFound("event not found")
		}
		return nil, 0, 0, fmt.Errorf("getting event with counts: %w", err)
	}

	eventRow := toEventRowFromWithCounts(row)
	return &eventRow, int(row.ProductCount), int(row.UpsellCount), nil
}

// =============================================================================
// EVENT PRODUCT/UPSELL HELPERS
// =============================================================================

func toEventProductOutput(row sqlc.GetEventProductByProductIDRow) EventProductOutput {
	var specialPrice *int64
	if row.SpecialPrice.Valid {
		v := int64(row.SpecialPrice.Int32)
		specialPrice = &v
	}
	var maxQuantity *int32
	if row.MaxQuantity.Valid {
		maxQuantity = &row.MaxQuantity.Int32
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	effectivePrice := row.OriginalPrice.Int64
	if specialPrice != nil {
		effectivePrice = *specialPrice
	}

	return EventProductOutput{
		ID:             row.ID.String(),
		ProductID:      row.ProductID.String(),
		Name:           row.ProductName,
		Keyword:        row.ProductKeyword,
		ImageURL:       imageURL,
		OriginalPrice:  row.OriginalPrice.Int64,
		SpecialPrice:   specialPrice,
		EffectivePrice: effectivePrice,
		MaxQuantity:    maxQuantity,
		DisplayOrder:   row.DisplayOrder,
		Featured:       row.Featured,
		Stock:          row.ProductStock.Int32,
		ProductActive:  row.ProductActive.Bool,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func toEventProductOutputFromList(row sqlc.ListEventProductsRow) EventProductOutput {
	var specialPrice *int64
	if row.SpecialPrice.Valid {
		v := int64(row.SpecialPrice.Int32)
		specialPrice = &v
	}
	var maxQuantity *int32
	if row.MaxQuantity.Valid {
		maxQuantity = &row.MaxQuantity.Int32
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	effectivePrice := row.OriginalPrice.Int64
	if specialPrice != nil {
		effectivePrice = *specialPrice
	}

	return EventProductOutput{
		ID:             row.ID.String(),
		ProductID:      row.ProductID.String(),
		Name:           row.ProductName,
		Keyword:        row.ProductKeyword,
		ImageURL:       imageURL,
		OriginalPrice:  row.OriginalPrice.Int64,
		SpecialPrice:   specialPrice,
		EffectivePrice: effectivePrice,
		MaxQuantity:    maxQuantity,
		DisplayOrder:   row.DisplayOrder,
		Featured:       row.Featured,
		Stock:          row.ProductStock.Int32,
		ProductActive:  row.ProductActive.Bool,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func toEventProductOutputFromGet(row sqlc.GetEventProductByIDRow) EventProductOutput {
	var specialPrice *int64
	if row.SpecialPrice.Valid {
		v := int64(row.SpecialPrice.Int32)
		specialPrice = &v
	}
	var maxQuantity *int32
	if row.MaxQuantity.Valid {
		maxQuantity = &row.MaxQuantity.Int32
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	effectivePrice := row.OriginalPrice.Int64
	if specialPrice != nil {
		effectivePrice = *specialPrice
	}

	return EventProductOutput{
		ID:             row.ID.String(),
		ProductID:      row.ProductID.String(),
		Name:           row.ProductName,
		Keyword:        row.ProductKeyword,
		ImageURL:       imageURL,
		OriginalPrice:  row.OriginalPrice.Int64,
		SpecialPrice:   specialPrice,
		EffectivePrice: effectivePrice,
		MaxQuantity:    maxQuantity,
		DisplayOrder:   row.DisplayOrder,
		Featured:       row.Featured,
		Stock:          row.ProductStock.Int32,
		ProductActive:  row.ProductActive.Bool,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func toEventUpsellOutputFromGet(row sqlc.GetEventUpsellByIDRow) EventUpsellOutput {
	var messageTemplate *string
	if row.MessageTemplate.Valid {
		messageTemplate = &row.MessageTemplate.String
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	discountedPrice := row.OriginalPrice.Int64 * int64(100-row.DiscountPercent) / 100

	return EventUpsellOutput{
		ID:              row.ID.String(),
		ProductID:       row.ProductID.String(),
		Name:            row.ProductName,
		Keyword:         row.ProductKeyword,
		ImageURL:        imageURL,
		OriginalPrice:   row.OriginalPrice.Int64,
		DiscountPercent: row.DiscountPercent,
		DiscountedPrice: discountedPrice,
		MessageTemplate: messageTemplate,
		DisplayOrder:    row.DisplayOrder,
		Active:          row.Active,
		Stock:           row.ProductStock.Int32,
		CreatedAt:       row.CreatedAt.Time,
		UpdatedAt:       row.UpdatedAt.Time,
	}
}

func toEventUpsellOutputFromList(row sqlc.ListEventUpsellsRow) EventUpsellOutput {
	var messageTemplate *string
	if row.MessageTemplate.Valid {
		messageTemplate = &row.MessageTemplate.String
	}
	var imageURL *string
	if row.ProductImageUrl.Valid {
		imageURL = &row.ProductImageUrl.String
	}

	discountedPrice := row.OriginalPrice.Int64 * int64(100-row.DiscountPercent) / 100

	return EventUpsellOutput{
		ID:              row.ID.String(),
		ProductID:       row.ProductID.String(),
		Name:            row.ProductName,
		Keyword:         row.ProductKeyword,
		ImageURL:        imageURL,
		OriginalPrice:   row.OriginalPrice.Int64,
		DiscountPercent: row.DiscountPercent,
		DiscountedPrice: discountedPrice,
		MessageTemplate: messageTemplate,
		DisplayOrder:    row.DisplayOrder,
		Active:          row.Active,
		Stock:           row.ProductStock.Int32,
		CreatedAt:       row.CreatedAt.Time,
		UpdatedAt:       row.UpdatedAt.Time,
	}
}

func toEventRowFromWithCounts(row sqlc.GetLiveEventWithCountsRow) EventRow {
	var title string
	if row.Title.Valid {
		title = row.Title.String
	}

	eventType := row.Type
	if eventType == "" {
		eventType = "single"
	}

	var cartExpirationMinutes, cartMaxQuantityPerItem *int
	if row.CartExpirationMinutes.Valid {
		v := int(row.CartExpirationMinutes.Int32)
		cartExpirationMinutes = &v
	}
	if row.CartMaxQuantityPerItem.Valid {
		v := int(row.CartMaxQuantityPerItem.Int32)
		cartMaxQuantityPerItem = &v
	}
	var autoSendCheckoutLinks *bool
	if row.SendOnLiveEnd.Valid {
		autoSendCheckoutLinks = &row.SendOnLiveEnd.Bool
	}
	var currentActiveProductID *string
	if row.CurrentActiveProductID.Valid {
		id := row.CurrentActiveProductID.String()
		currentActiveProductID = &id
	}
	var scheduledAt *time.Time
	if row.ScheduledAt.Valid {
		scheduledAt = &row.ScheduledAt.Time
	}
	var description *string
	if row.Description.Valid {
		description = &row.Description.String
	}

	return EventRow{
		ID:                      row.ID.String(),
		StoreID:                 row.StoreID.String(),
		Title:                   title,
		Type:                    eventType,
		Status:                  row.Status,
		TotalOrders:             int(row.TotalOrders),
		CloseCartOnEventEnd:     row.CloseCartOnEventEnd,
		CartExpirationMinutes:   cartExpirationMinutes,
		CartMaxQuantityPerItem:  cartMaxQuantityPerItem,
		SendOnLiveEnd:           autoSendCheckoutLinks,
		CurrentActiveProductID:  currentActiveProductID,
		ProcessingPaused:        row.ProcessingPaused,
		ScheduledAt:             scheduledAt,
		Description:             description,
		CreatedAt:               row.CreatedAt.Time,
		UpdatedAt:               row.UpdatedAt.Time,
	}
}
