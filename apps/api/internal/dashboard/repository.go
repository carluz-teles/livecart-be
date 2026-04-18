package dashboard

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) GetStats(ctx context.Context, storeID string) (*DashboardStatsRow, error) {
	query := `
		SELECT
			-- Total revenue from orders
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM cart_items ci
				JOIN carts c ON c.id = ci.cart_id
				JOIN live_events le ON le.id = c.event_id
				WHERE le.store_id = $1
			), 0)::BIGINT as total_revenue,
			-- Total orders
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events le ON le.id = c.event_id
				WHERE le.store_id = $1
			), 0)::INT as total_orders,
			-- Active products
			COALESCE((
				SELECT COUNT(*)
				FROM products p
				WHERE p.store_id = $1 AND p.active = true
			), 0)::INT as active_products,
			-- Total lives
			COALESCE((
				SELECT COUNT(*)
				FROM live_events le
				WHERE le.store_id = $1
			), 0)::INT as total_lives
	`

	var row DashboardStatsRow
	err := r.db.QueryRow(ctx, query, storeID).Scan(
		&row.TotalRevenue,
		&row.TotalOrders,
		&row.ActiveProducts,
		&row.TotalLives,
	)
	if err != nil {
		return nil, fmt.Errorf("getting dashboard stats: %w", err)
	}

	return &row, nil
}

func (r *Repository) GetMonthlyRevenue(ctx context.Context, storeID string) ([]MonthlyRevenueRow, error) {
	query := `
		SELECT
			TO_CHAR(c.created_at, 'Mon') as month,
			EXTRACT(MONTH FROM c.created_at)::INT as month_num,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as revenue
		FROM carts c
		JOIN live_events le ON le.id = c.event_id
		LEFT JOIN cart_items ci ON ci.cart_id = c.id
		WHERE le.store_id = $1
		  AND c.created_at >= date_trunc('year', CURRENT_DATE)
		GROUP BY TO_CHAR(c.created_at, 'Mon'), EXTRACT(MONTH FROM c.created_at)
		ORDER BY month_num
	`

	rows, err := r.db.Query(ctx, query, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting monthly revenue: %w", err)
	}
	defer rows.Close()

	var items []MonthlyRevenueRow
	for rows.Next() {
		var row MonthlyRevenueRow
		err := rows.Scan(&row.Month, &row.MonthNum, &row.Revenue)
		if err != nil {
			return nil, fmt.Errorf("scanning monthly revenue row: %w", err)
		}
		items = append(items, row)
	}

	return items, nil
}

func (r *Repository) GetTopProducts(ctx context.Context, storeID string) ([]TopProductRow, error) {
	query := `
		SELECT
			p.id,
			p.name,
			p.keyword,
			COALESCE(SUM(ci.quantity), 0)::INT as total_sold,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as total_revenue
		FROM products p
		JOIN cart_items ci ON ci.product_id = p.id
		JOIN carts c ON c.id = ci.cart_id
		JOIN live_events le ON le.id = c.event_id
		WHERE le.store_id = $1
		GROUP BY p.id, p.name, p.keyword
		ORDER BY total_sold DESC
		LIMIT 5
	`

	rows, err := r.db.Query(ctx, query, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting top products: %w", err)
	}
	defer rows.Close()

	var products []TopProductRow
	for rows.Next() {
		var row TopProductRow
		err := rows.Scan(&row.ID, &row.Name, &row.Keyword, &row.TotalSold, &row.TotalRevenue)
		if err != nil {
			return nil, fmt.Errorf("scanning top product row: %w", err)
		}
		products = append(products, row)
	}

	return products, nil
}

// =============================================================================
// ANALYTICS - Revenue Attribution
// =============================================================================

// GetEventsWithRevenue returns all events with their revenue metrics
func (r *Repository) GetEventsWithRevenue(ctx context.Context, storeID string, limit int) ([]EventWithRevenueRow, error) {
	query := `
		SELECT
			e.id,
			COALESCE(e.title, '') as title,
			e.status,
			e.created_at,
			COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = e.id), 0)::int AS total_comments,
			COALESCE((SELECT COUNT(*) FROM carts c WHERE c.event_id = e.id), 0)::int AS total_carts,
			COALESCE((SELECT COUNT(*) FROM carts c WHERE c.event_id = e.id AND c.payment_status = 'paid'), 0)::int AS paid_carts,
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM carts c
				JOIN cart_items ci ON ci.cart_id = c.id
				WHERE c.event_id = e.id AND c.payment_status = 'paid'
			), 0)::bigint AS confirmed_revenue
		FROM live_events e
		WHERE e.store_id = $1
		ORDER BY e.created_at DESC
		LIMIT $2
	`

	rows, err := r.db.Query(ctx, query, storeID, limit)
	if err != nil {
		return nil, fmt.Errorf("getting events with revenue: %w", err)
	}
	defer rows.Close()

	var events []EventWithRevenueRow
	for rows.Next() {
		var row EventWithRevenueRow
		var createdAt interface{}
		err := rows.Scan(
			&row.ID,
			&row.Title,
			&row.Status,
			&createdAt,
			&row.TotalComments,
			&row.TotalCarts,
			&row.PaidCarts,
			&row.ConfirmedRevenue,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning event with revenue row: %w", err)
		}
		// Format timestamp as ISO string
		if t, ok := createdAt.(interface{ Format(string) string }); ok {
			row.CreatedAt = t.Format("2006-01-02T15:04:05Z")
		}
		events = append(events, row)
	}

	return events, nil
}

// GetAggregatedFunnel returns aggregated funnel metrics for the store
func (r *Repository) GetAggregatedFunnel(ctx context.Context, storeID string, days int) (*AggregatedFunnelRow, error) {
	query := `
		SELECT
			-- Total comments across all events
			COALESCE((
				SELECT SUM(ls.total_comments)
				FROM live_sessions ls
				JOIN live_events e ON e.id = ls.event_id
				WHERE e.store_id = $1 AND ls.created_at >= NOW() - INTERVAL '1 day' * $2
			), 0)::int AS total_comments,
			-- Total carts created
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= NOW() - INTERVAL '1 day' * $2
			), 0)::int AS total_carts,
			-- Carts that reached checkout
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1
				  AND c.created_at >= NOW() - INTERVAL '1 day' * $2
				  AND (c.status = 'checkout' OR c.checkout_url IS NOT NULL)
			), 0)::int AS checkout_carts,
			-- Paid carts
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1
				  AND c.created_at >= NOW() - INTERVAL '1 day' * $2
				  AND c.payment_status = 'paid'
			), 0)::int AS paid_carts,
			-- Confirmed revenue (GMV)
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM carts c
				JOIN cart_items ci ON ci.cart_id = c.id
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1
				  AND c.created_at >= NOW() - INTERVAL '1 day' * $2
				  AND c.payment_status = 'paid'
			), 0)::bigint AS confirmed_revenue,
			-- Average ticket
			COALESCE((
				SELECT AVG(cart_total)
				FROM (
					SELECT SUM(ci.quantity * ci.unit_price) as cart_total
					FROM carts c
					JOIN cart_items ci ON ci.cart_id = c.id
					JOIN live_events e ON e.id = c.event_id
					WHERE e.store_id = $1
					  AND c.created_at >= NOW() - INTERVAL '1 day' * $2
					  AND c.payment_status = 'paid'
					GROUP BY c.id
				) sub
			), 0)::bigint AS average_ticket
	`

	var row AggregatedFunnelRow
	err := r.db.QueryRow(ctx, query, storeID, days).Scan(
		&row.TotalComments,
		&row.TotalCarts,
		&row.CheckoutCarts,
		&row.PaidCarts,
		&row.ConfirmedRevenue,
		&row.AverageTicket,
	)
	if err != nil {
		return nil, fmt.Errorf("getting aggregated funnel: %w", err)
	}

	return &row, nil
}
