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
				JOIN live_sessions ls ON ls.id = c.session_id
				WHERE ls.store_id = $1
			), 0)::BIGINT as total_revenue,
			-- Total orders
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_sessions ls ON ls.id = c.session_id
				WHERE ls.store_id = $1
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
				FROM live_sessions ls
				WHERE ls.store_id = $1
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
		JOIN live_sessions ls ON ls.id = c.session_id
		LEFT JOIN cart_items ci ON ci.cart_id = c.id
		WHERE ls.store_id = $1
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
		JOIN live_sessions ls ON ls.id = c.session_id
		WHERE ls.store_id = $1
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
