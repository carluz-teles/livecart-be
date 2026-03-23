package customer

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

func (r *Repository) List(ctx context.Context, params ListCustomersParams) (ListCustomersResult, error) {
	var result ListCustomersResult

	// Build dynamic query
	baseQuery := `
		SELECT
			c.platform_user_id as id,
			c.platform_handle as handle,
			COUNT(DISTINCT c.id)::INT as total_orders,
			COALESCE(SUM(
				(SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
			), 0)::BIGINT as total_spent,
			MAX(c.created_at) as last_order_at,
			MIN(c.created_at) as first_order_at
		FROM carts c
		JOIN live_sessions ls ON ls.id = c.session_id
		WHERE ls.store_id = $1
	`

	countQuery := `
		SELECT COUNT(DISTINCT c.platform_user_id)
		FROM carts c
		JOIN live_sessions ls ON ls.id = c.session_id
		WHERE ls.store_id = $1
	`

	args := []interface{}{params.StoreID}
	argIndex := 2

	var conditions []string
	var havingConditions []string

	// Search filter
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("c.platform_handle ILIKE $%d", argIndex))
		args = append(args, "%"+params.Search+"%")
		argIndex++
	}

	// Add conditions to queries
	if len(conditions) > 0 {
		condStr := " AND " + strings.Join(conditions, " AND ")
		baseQuery += condStr
		countQuery += condStr
	}

	// Group by
	baseQuery += " GROUP BY c.platform_user_id, c.platform_handle"

	// Having conditions (filters on aggregates)
	if params.Filters.HasOrders != nil && *params.Filters.HasOrders {
		havingConditions = append(havingConditions, "COUNT(DISTINCT c.id) > 0")
	}
	if params.Filters.OrderCountMin != nil {
		havingConditions = append(havingConditions, fmt.Sprintf("COUNT(DISTINCT c.id) >= %d", *params.Filters.OrderCountMin))
	}
	if params.Filters.OrderCountMax != nil {
		havingConditions = append(havingConditions, fmt.Sprintf("COUNT(DISTINCT c.id) <= %d", *params.Filters.OrderCountMax))
	}
	if params.Filters.TotalSpentMin != nil {
		havingConditions = append(havingConditions, fmt.Sprintf("COALESCE(SUM((SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)), 0) >= %d", *params.Filters.TotalSpentMin))
	}
	if params.Filters.TotalSpentMax != nil {
		havingConditions = append(havingConditions, fmt.Sprintf("COALESCE(SUM((SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)), 0) <= %d", *params.Filters.TotalSpentMax))
	}

	if len(havingConditions) > 0 {
		baseQuery += " HAVING " + strings.Join(havingConditions, " AND ")
	}

	// Get total count (simpler approach - count from subquery)
	countSubquery := fmt.Sprintf(`SELECT COUNT(*) FROM (%s) sub`, baseQuery)
	err := r.db.QueryRow(ctx, countSubquery, args...).Scan(&result.Total)
	if err != nil {
		return result, fmt.Errorf("counting customers: %w", err)
	}

	// Sorting
	sortColumn := "last_order_at"
	allowedSortColumns := map[string]string{
		"last_order_at":  "last_order_at",
		"first_order_at": "first_order_at",
		"total_orders":   "total_orders",
		"total_spent":    "total_spent",
		"handle":         "handle",
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
		return result, fmt.Errorf("listing customers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row CustomerRow
		err := rows.Scan(
			&row.ID,
			&row.Handle,
			&row.TotalOrders,
			&row.TotalSpent,
			&row.LastOrderAt,
			&row.FirstOrderAt,
		)
		if err != nil {
			return result, fmt.Errorf("scanning customer row: %w", err)
		}
		result.Customers = append(result.Customers, row)
	}

	return result, nil
}

func (r *Repository) GetByID(ctx context.Context, storeID, customerID string) (*CustomerRow, error) {
	query := `
		SELECT
			c.platform_user_id as id,
			c.platform_handle as handle,
			COUNT(DISTINCT c.id)::INT as total_orders,
			COALESCE(SUM(
				(SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
			), 0)::BIGINT as total_spent,
			MAX(c.created_at) as last_order_at,
			MIN(c.created_at) as first_order_at
		FROM carts c
		JOIN live_sessions ls ON ls.id = c.session_id
		WHERE ls.store_id = $1 AND c.platform_user_id = $2
		GROUP BY c.platform_user_id, c.platform_handle
	`

	var row CustomerRow
	err := r.db.QueryRow(ctx, query, storeID, customerID).Scan(
		&row.ID,
		&row.Handle,
		&row.TotalOrders,
		&row.TotalSpent,
		&row.LastOrderAt,
		&row.FirstOrderAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting customer by id: %w", err)
	}

	return &row, nil
}

func (r *Repository) GetStats(ctx context.Context, storeID string) (*CustomerStatsOutput, error) {
	query := `
		SELECT
			COUNT(DISTINCT c.platform_user_id)::INT as total_customers,
			COUNT(DISTINCT CASE
				WHEN c.created_at > now() - interval '30 days' THEN c.platform_user_id
			END)::INT as active_customers,
			COALESCE(
				CASE
					WHEN COUNT(DISTINCT c.platform_user_id) > 0 THEN
						SUM((SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id))
						/ COUNT(DISTINCT c.platform_user_id)
					ELSE 0
				END,
				0
			)::BIGINT as avg_spent_per_customer
		FROM carts c
		JOIN live_sessions ls ON ls.id = c.session_id
		WHERE ls.store_id = $1
	`

	var stats CustomerStatsOutput
	err := r.db.QueryRow(ctx, query, storeID).Scan(
		&stats.TotalCustomers,
		&stats.ActiveCustomers,
		&stats.AvgSpentPerCustomer,
	)
	if err != nil {
		return nil, fmt.Errorf("getting customer stats: %w", err)
	}

	return &stats, nil
}
