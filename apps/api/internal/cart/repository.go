package cart

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// List returns carts that do NOT have a corresponding order (non-paid carts).
// Carts cancelados ficam de fora: a lista existe para recuperação de carrinho e
// um carrinho que a loja cancelou (ou que morreu com o bloqueio do comprador)
// não é recuperável — o estoque já voltou e o link não aceita pagamento.
// cart_product_total_cents is the canonical GMV function — never re-sums items inline.
func (r *Repository) List(ctx context.Context, params ListCartsParams) (ListCartsResult, error) {
	var result ListCartsResult

	baseQuery := `
		SELECT
			c.id,
			c.short_id,
			COALESCE(c.platform_handle, '') as platform_handle,
			COALESCE(c.customer_name, '') as customer_name,
			COALESCE(c.customer_phone, '') as customer_phone,
			COALESCE(cart_product_total_cents(c.id), 0) as total_cents,
			c.status,
			c.expires_at,
			c.created_at
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
		  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.cart_id = c.id)
		  AND c.status <> 'cancelled'
	`

	countQuery := `
		SELECT COUNT(*)
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
		  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.cart_id = c.id)
		  AND c.status <> 'cancelled'
	`

	conditions, args := buildCartListConditions(params.StoreID, params.Search, params.Filters)

	if len(conditions) > 0 {
		condStr := " AND " + strings.Join(conditions, " AND ")
		baseQuery += condStr
		countQuery += condStr
	}

	if err := r.db.QueryRow(ctx, countQuery, args...).Scan(&result.Total); err != nil {
		return result, fmt.Errorf("counting carts: %w", err)
	}

	sortColumn := "c.created_at"
	allowedSortColumns := map[string]string{
		"created_at": "c.created_at",
		"status":     "c.status",
		"total_cents": "total_cents",
		"short_id":   "c.short_id",
	}
	if col, ok := allowedSortColumns[params.Sorting.SortBy]; ok {
		sortColumn = col
	}
	sortOrder := "DESC"
	if strings.ToUpper(params.Sorting.SortOrder) == "ASC" {
		sortOrder = "ASC"
	}
	baseQuery += fmt.Sprintf(" ORDER BY %s %s", sortColumn, sortOrder)

	limit := params.Pagination.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := (params.Pagination.Page - 1) * limit
	if offset < 0 {
		offset = 0
	}
	baseQuery += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)

	rows, err := r.db.Query(ctx, baseQuery, args...)
	if err != nil {
		return result, fmt.Errorf("listing carts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row CartRow
		if err := rows.Scan(
			&row.ID,
			&row.ShortID,
			&row.PlatformHandle,
			&row.CustomerName,
			&row.CustomerPhone,
			&row.TotalCents,
			&row.Status,
			&row.ExpiresAt,
			&row.CreatedAt,
		); err != nil {
			return result, fmt.Errorf("scanning cart row: %w", err)
		}
		result.Carts = append(result.Carts, row)
	}
	return result, rows.Err()
}

// GetStats returns aggregate metrics for non-paid carts.
// "open" = non-paid AND not cancelled AND not yet expired (expires_at > now()
// OR expires_at IS NULL).
// Uses cart_product_total_cents for all value computations — never re-sums items inline.
func (r *Repository) GetStats(ctx context.Context, storeID string) (*CartStatsRow, error) {
	const q = `
		SELECT
			COUNT(*) FILTER (
				WHERE c.expires_at IS NULL OR c.expires_at > now()
			)::INT as open_carts,
			COALESCE(SUM(cart_product_total_cents(c.id)) FILTER (
				WHERE c.expires_at IS NULL OR c.expires_at > now()
			), 0)::BIGINT as open_value_cents,
			COALESCE(
				CASE
					WHEN COUNT(*) FILTER (WHERE c.expires_at IS NULL OR c.expires_at > now()) > 0 THEN
						SUM(cart_product_total_cents(c.id)) FILTER (WHERE c.expires_at IS NULL OR c.expires_at > now()) /
						COUNT(*) FILTER (WHERE c.expires_at IS NULL OR c.expires_at > now())
					ELSE 0
				END, 0
			)::BIGINT as avg_ticket_cents,
			COUNT(*) FILTER (
				WHERE (c.expires_at IS NULL OR c.expires_at > now())
				  AND (c.platform_handle <> '' OR (c.customer_phone IS NOT NULL AND c.customer_phone <> ''))
			)::INT as recoverable_carts
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1
		  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.cart_id = c.id)
		  AND c.status <> 'cancelled'
	`

	var row CartStatsRow
	if err := r.db.QueryRow(ctx, q, storeID).Scan(
		&row.OpenCarts,
		&row.OpenValueCents,
		&row.AvgTicketCents,
		&row.RecoverableCarts,
	); err != nil {
		return nil, fmt.Errorf("getting cart stats: %w", err)
	}
	return &row, nil
}

// buildCartListConditions builds WHERE conditions for the cart list queries.
// args always starts with storeID as $1.
func buildCartListConditions(storeID string, search string, filters CartFilters) ([]string, []interface{}) {
	args := []interface{}{storeID}
	argIndex := 2
	var conditions []string

	trimmedSearch := strings.TrimSpace(search)
	trimmedSearch = strings.TrimPrefix(trimmedSearch, "#")
	if trimmedSearch != "" {
		conditions = append(conditions, fmt.Sprintf(
			"(c.platform_handle ILIKE $%d OR c.id::TEXT ILIKE $%d OR c.short_id::TEXT ILIKE $%d OR c.customer_name ILIKE $%d OR c.customer_phone ILIKE $%d)",
			argIndex, argIndex, argIndex, argIndex, argIndex,
		))
		args = append(args, "%"+trimmedSearch+"%")
		argIndex++
	}

	if len(filters.Status) > 0 {
		placeholders := make([]string, len(filters.Status))
		for i, s := range filters.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, s)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf("c.status IN (%s)", strings.Join(placeholders, ",")))
	}

	return conditions, args
}
