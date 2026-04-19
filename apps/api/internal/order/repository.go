package order

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) List(ctx context.Context, params ListOrdersParams) (ListOrdersResult, error) {
	var result ListOrdersResult

	// Build dynamic query
	baseQuery := `
		SELECT
			c.id,
			c.event_id,
			c.platform_user_id,
			c.platform_handle,
			c.token,
			c.status,
			c.payment_status,
			c.paid_at,
			c.created_at,
			c.expires_at,
			e.title as live_title,
			COALESCE(
				(SELECT lsp.platform FROM live_session_platforms lsp
				 JOIN live_sessions ls ON ls.id = lsp.session_id
				 WHERE ls.event_id = e.id
				 ORDER BY lsp.added_at LIMIT 1),
				'instagram'
			) as live_platform,
			COALESCE(
				(SELECT SUM(ci.quantity * ci.unit_price)::BIGINT FROM cart_items ci WHERE ci.cart_id = c.id),
				0
			) as total_amount,
			COALESCE(
				(SELECT SUM(ci.quantity)::INT FROM cart_items ci WHERE ci.cart_id = c.id),
				0
			) as total_items
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
	`

	countQuery := `
		SELECT COUNT(*)
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
	`

	args := []interface{}{params.StoreID}
	argIndex := 2

	var conditions []string

	// Search filter
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("(c.platform_handle ILIKE $%d OR c.id::TEXT ILIKE $%d)", argIndex, argIndex))
		args = append(args, "%"+params.Search+"%")
		argIndex++
	}

	// Status filter
	if len(params.Filters.Status) > 0 {
		placeholders := make([]string, len(params.Filters.Status))
		for i, s := range params.Filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, s)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf("c.status IN (%s)", strings.Join(placeholders, ",")))
	}

	// Payment status filter
	if len(params.Filters.PaymentStatus) > 0 {
		placeholders := make([]string, len(params.Filters.PaymentStatus))
		for i, ps := range params.Filters.PaymentStatus {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, ps)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf("c.payment_status IN (%s)", strings.Join(placeholders, ",")))
	}

	// Live event filter (was live session filter)
	if params.Filters.LiveSessionID != nil && *params.Filters.LiveSessionID != "" {
		conditions = append(conditions, fmt.Sprintf("c.event_id = $%d", argIndex))
		args = append(args, *params.Filters.LiveSessionID)
		argIndex++
	}

	// Date filters
	if params.Filters.DateFrom != nil && *params.Filters.DateFrom != "" {
		conditions = append(conditions, fmt.Sprintf("c.created_at >= $%d", argIndex))
		args = append(args, *params.Filters.DateFrom)
		argIndex++
	}
	if params.Filters.DateTo != nil && *params.Filters.DateTo != "" {
		conditions = append(conditions, fmt.Sprintf("c.created_at <= $%d", argIndex))
		args = append(args, *params.Filters.DateTo)
		argIndex++
	}

	// Add conditions to queries
	if len(conditions) > 0 {
		condStr := " AND " + strings.Join(conditions, " AND ")
		baseQuery += condStr
		countQuery += condStr
	}

	// Get total count
	err := r.db.QueryRow(ctx, countQuery, args...).Scan(&result.Total)
	if err != nil {
		return result, fmt.Errorf("counting orders: %w", err)
	}

	// Sorting
	sortColumn := "c.created_at"
	allowedSortColumns := map[string]string{
		"created_at":     "c.created_at",
		"status":         "c.status",
		"payment_status": "c.payment_status",
		"total_amount":   "total_amount",
	}
	if col, ok := allowedSortColumns[params.Sorting.SortBy]; ok {
		sortColumn = col
	}
	sortOrder := "DESC"
	if strings.ToUpper(params.Sorting.SortOrder) == "ASC" {
		sortOrder = "ASC"
	}
	baseQuery += fmt.Sprintf(" ORDER BY %s %s", sortColumn, sortOrder)

	// Pagination
	limit := params.Pagination.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := (params.Pagination.Page - 1) * limit
	if offset < 0 {
		offset = 0
	}
	baseQuery += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	// Execute query
	rows, err := r.db.Query(ctx, baseQuery, args...)
	if err != nil {
		return result, fmt.Errorf("listing orders: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row OrderRow
		err := rows.Scan(
			&row.ID,
			&row.EventID,
			&row.PlatformUserID,
			&row.PlatformHandle,
			&row.Token,
			&row.Status,
			&row.PaymentStatus,
			&row.PaidAt,
			&row.CreatedAt,
			&row.ExpiresAt,
			&row.LiveTitle,
			&row.LivePlatform,
			&row.TotalAmount,
			&row.TotalItems,
		)
		if err != nil {
			return result, fmt.Errorf("scanning order row: %w", err)
		}
		result.Orders = append(result.Orders, row)
	}

	return result, nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*OrderDetailRow, error) {
	query := `
		SELECT
			c.id,
			c.event_id,
			c.platform_user_id,
			c.platform_handle,
			c.token,
			c.status,
			c.payment_status,
			c.paid_at,
			c.created_at,
			c.expires_at,
			e.title as live_title,
			COALESCE(
				(SELECT lsp.platform FROM live_session_platforms lsp
				 JOIN live_sessions ls ON ls.id = lsp.session_id
				 WHERE ls.event_id = e.id
				 ORDER BY lsp.added_at LIMIT 1),
				'instagram'
			) as live_platform,
			e.store_id
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE c.id = $1
	`

	var row OrderDetailRow
	err := r.db.QueryRow(ctx, query, id).Scan(
		&row.ID,
		&row.EventID,
		&row.PlatformUserID,
		&row.PlatformHandle,
		&row.Token,
		&row.Status,
		&row.PaymentStatus,
		&row.PaidAt,
		&row.CreatedAt,
		&row.ExpiresAt,
		&row.LiveTitle,
		&row.LivePlatform,
		&row.StoreID,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting order by id: %w", err)
	}

	return &row, nil
}

func (r *Repository) GetItems(ctx context.Context, cartID string) ([]OrderItemRow, error) {
	query := `
		SELECT
			ci.id,
			ci.cart_id,
			ci.product_id,
			ci.size,
			ci.quantity,
			COALESCE(ci.unit_price, 0)::BIGINT as unit_price,
			p.name as product_name,
			p.image_url as product_image,
			p.keyword as product_keyword
		FROM cart_items ci
		JOIN products p ON p.id = ci.product_id
		WHERE ci.cart_id = $1
	`

	rows, err := r.db.Query(ctx, query, cartID)
	if err != nil {
		return nil, fmt.Errorf("getting order items: %w", err)
	}
	defer rows.Close()

	var items []OrderItemRow
	for rows.Next() {
		var item OrderItemRow
		err := rows.Scan(
			&item.ID,
			&item.CartID,
			&item.ProductID,
			&item.Size,
			&item.Quantity,
			&item.UnitPrice,
			&item.ProductName,
			&item.ProductImage,
			&item.ProductKeyword,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning order item row: %w", err)
		}
		items = append(items, item)
	}

	return items, nil
}

func (r *Repository) UpdateStatus(ctx context.Context, id string, status string) error {
	query := `UPDATE carts SET status = $2 WHERE id = $1`
	_, err := r.db.Exec(ctx, query, id, status)
	if err != nil {
		return fmt.Errorf("updating order status: %w", err)
	}
	return nil
}

func (r *Repository) UpdatePaymentStatus(ctx context.Context, id string, paymentStatus string) error {
	query := `
		UPDATE carts
		SET payment_status = $2, paid_at = CASE WHEN $2 = 'paid' THEN now() ELSE paid_at END
		WHERE id = $1
	`
	_, err := r.db.Exec(ctx, query, id, paymentStatus)
	if err != nil {
		return fmt.Errorf("updating order payment status: %w", err)
	}
	return nil
}

func (r *Repository) GetCustomerComments(ctx context.Context, eventID string, platformUserID string) ([]CommentRow, error) {
	query := `
		SELECT id, text, created_at
		FROM live_comments
		WHERE event_id = $1 AND platform_user_id = $2
		ORDER BY created_at
	`

	rows, err := r.db.Query(ctx, query, eventID, platformUserID)
	if err != nil {
		return nil, fmt.Errorf("getting customer comments: %w", err)
	}
	defer rows.Close()

	var comments []CommentRow
	for rows.Next() {
		var c CommentRow
		err := rows.Scan(&c.ID, &c.Text, &c.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		comments = append(comments, c)
	}

	return comments, nil
}

func (r *Repository) GetStats(ctx context.Context, storeID string) (*OrderStatsOutput, error) {
	query := `
		SELECT
			COUNT(*)::INT as total_orders,
			COUNT(*) FILTER (WHERE c.status = 'active')::INT as pending_orders,
			COALESCE(SUM(
				(SELECT SUM(ci.quantity * ci.unit_price) FROM cart_items ci WHERE ci.cart_id = c.id)
			), 0)::BIGINT as total_revenue,
			COALESCE(
				CASE
					WHEN COUNT(*) > 0 THEN
						SUM((SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)) / COUNT(*)
					ELSE 0
				END,
				0
			)::BIGINT as avg_ticket
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
	`

	var stats OrderStatsOutput
	err := r.db.QueryRow(ctx, query, storeID).Scan(
		&stats.TotalOrders,
		&stats.PendingOrders,
		&stats.TotalRevenue,
		&stats.AvgTicket,
	)
	if err != nil {
		return nil, fmt.Errorf("getting order stats: %w", err)
	}

	return &stats, nil
}
