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

// =============================================================================
// TOP BUYERS
// =============================================================================

// GetTopBuyers returns the top 5 buyers by total spent
func (r *Repository) GetTopBuyers(ctx context.Context, storeID string) ([]TopBuyerRow, error) {
	query := `
		SELECT
			c.platform_user_id as id,
			c.platform_handle as handle,
			COUNT(DISTINCT c.id)::INT as total_orders,
			COALESCE(SUM(
				(SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
			), 0)::BIGINT as total_spent,
			MAX(c.created_at) as last_purchase_at
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1 AND c.payment_status = 'paid'
		GROUP BY c.platform_user_id, c.platform_handle
		ORDER BY total_spent DESC
		LIMIT 5
	`

	rows, err := r.db.Query(ctx, query, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting top buyers: %w", err)
	}
	defer rows.Close()

	var buyers []TopBuyerRow
	for rows.Next() {
		var row TopBuyerRow
		var lastPurchaseAt interface{}
		err := rows.Scan(&row.ID, &row.Handle, &row.TotalOrders, &row.TotalSpent, &lastPurchaseAt)
		if err != nil {
			return nil, fmt.Errorf("scanning top buyer row: %w", err)
		}
		// Format timestamp as ISO string
		if t, ok := lastPurchaseAt.(interface{ Format(string) string }); ok {
			row.LastPurchaseAt = t.Format("2006-01-02T15:04:05Z")
		}
		buyers = append(buyers, row)
	}

	return buyers, nil
}

// =============================================================================
// PRODUCT SALES (Stacked Bar Chart)
// =============================================================================

// GetProductSales returns monthly sales data by product for stacked bar chart
func (r *Repository) GetProductSales(ctx context.Context, storeID string) (*ProductSalesOutput, error) {
	// First, get unique products that have sales
	productsQuery := `
		SELECT DISTINCT
			p.id,
			p.name,
			p.keyword
		FROM products p
		JOIN cart_items ci ON ci.product_id = p.id
		JOIN carts c ON c.id = ci.cart_id
		JOIN live_events le ON le.id = c.event_id
		WHERE le.store_id = $1
		ORDER BY p.name
		LIMIT 10
	`

	prodRows, err := r.db.Query(ctx, productsQuery, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting products for sales: %w", err)
	}
	defer prodRows.Close()

	var products []ProductSalesProductRow
	productIDs := make([]string, 0)
	for prodRows.Next() {
		var row ProductSalesProductRow
		if err := prodRows.Scan(&row.ID, &row.Name, &row.Keyword); err != nil {
			return nil, fmt.Errorf("scanning product row: %w", err)
		}
		products = append(products, row)
		productIDs = append(productIDs, row.ID)
	}

	if len(products) == 0 {
		return &ProductSalesOutput{
			Products: []ProductSalesProductRow{},
			Data:     []ProductSalesDataRow{},
		}, nil
	}

	// Get monthly sales by product
	salesQuery := `
		SELECT
			TO_CHAR(c.created_at, 'Mon') as month,
			EXTRACT(MONTH FROM c.created_at)::INT as month_num,
			p.id as product_id,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as revenue
		FROM carts c
		JOIN live_events le ON le.id = c.event_id
		JOIN cart_items ci ON ci.cart_id = c.id
		JOIN products p ON p.id = ci.product_id
		WHERE le.store_id = $1
		  AND c.created_at >= date_trunc('year', CURRENT_DATE)
		GROUP BY TO_CHAR(c.created_at, 'Mon'), EXTRACT(MONTH FROM c.created_at), p.id
		ORDER BY month_num, p.id
	`

	salesRows, err := r.db.Query(ctx, salesQuery, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting product sales: %w", err)
	}
	defer salesRows.Close()

	// Build monthly data with product values
	monthlyData := make(map[int]*ProductSalesDataRow)
	for salesRows.Next() {
		var month string
		var monthNum int
		var productID string
		var revenue int64

		if err := salesRows.Scan(&month, &monthNum, &productID, &revenue); err != nil {
			return nil, fmt.Errorf("scanning sales row: %w", err)
		}

		if _, exists := monthlyData[monthNum]; !exists {
			monthlyData[monthNum] = &ProductSalesDataRow{
				Month:    month,
				MonthNum: monthNum,
				Values:   make(map[string]int64),
			}
		}
		monthlyData[monthNum].Values[productID] = revenue
	}

	// Convert map to sorted slice
	var data []ProductSalesDataRow
	for i := 1; i <= 12; i++ {
		if d, exists := monthlyData[i]; exists {
			data = append(data, *d)
		}
	}

	return &ProductSalesOutput{
		Products: products,
		Data:     data,
	}, nil
}

// =============================================================================
// REVENUE BY PAYMENT METHOD (Pie Chart)
// =============================================================================

// GetRevenueByPaymentMethod returns revenue grouped by payment method
func (r *Repository) GetRevenueByPaymentMethod(ctx context.Context, storeID string) ([]RevenueByPaymentRow, error) {
	query := `
		SELECT
			COALESCE(c.payment_method, 'other') as payment_method,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT as revenue,
			COUNT(DISTINCT c.id)::INT as count
		FROM carts c
		JOIN live_events le ON le.id = c.event_id
		JOIN cart_items ci ON ci.cart_id = c.id
		WHERE le.store_id = $1
		  AND c.payment_status = 'paid'
		GROUP BY c.payment_method
		ORDER BY revenue DESC
	`

	rows, err := r.db.Query(ctx, query, storeID)
	if err != nil {
		return nil, fmt.Errorf("getting revenue by payment method: %w", err)
	}
	defer rows.Close()

	var items []RevenueByPaymentRow
	for rows.Next() {
		var row RevenueByPaymentRow
		if err := rows.Scan(&row.PaymentMethod, &row.Revenue, &row.Count); err != nil {
			return nil, fmt.Errorf("scanning revenue by payment row: %w", err)
		}
		items = append(items, row)
	}

	return items, nil
}

// =============================================================================
// CHECKOUT UPSELL / DOWNSELL — aggregated metrics for the merchant dashboard
// =============================================================================

// GetCheckoutUpsell aggregates upsell/downsell numbers for paid carts. eventID
// is optional — empty means all paid carts in the store.
func (r *Repository) GetCheckoutUpsell(ctx context.Context, storeID, eventID string) (*CheckoutUpsellRow, error) {
	q := `
		WITH per_cart AS (
			SELECT
				c.id AS cart_id,
				COALESCE(c.initial_subtotal_cents, 0) AS initial_cents,
				COALESCE((
					SELECT SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price)
					FROM cart_items ci
					WHERE ci.cart_id = c.id AND ci.quantity > ci.waitlisted_quantity
				), 0)::bigint AS final_cents,
				EXISTS (
					SELECT 1 FROM cart_mutations m
					WHERE m.cart_id = c.id AND m.source = 'buyer_checkout'
				) AS has_mut
			FROM carts c
			JOIN live_events le ON le.id = c.event_id
			WHERE le.store_id = $1
			  AND c.payment_status = 'paid'
			  AND ($2::uuid IS NULL OR c.event_id = $2::uuid)
		)
		SELECT
			COUNT(*) FILTER (WHERE has_mut)::int                                         AS carts_with_mutations,
			COALESCE(SUM(GREATEST(final_cents - initial_cents, 0)), 0)::bigint           AS upsell_cents,
			COALESCE(SUM(GREATEST(initial_cents - final_cents, 0)), 0)::bigint           AS downsell_cents,
			COUNT(*)::int                                                                AS total_paid_carts
		FROM per_cart
	`
	var (
		row      CheckoutUpsellRow
		eventArg interface{}
	)
	if eventID != "" {
		eventArg = eventID
	}
	if err := r.db.QueryRow(ctx, q, storeID, eventArg).Scan(
		&row.CartsWithMutations,
		&row.UpsellCents,
		&row.DownsellCents,
		&row.TotalPaidCarts,
	); err != nil {
		return nil, fmt.Errorf("getting checkout upsell: %w", err)
	}
	return &row, nil
}

// ListTopCheckoutMutations returns the top N products added or removed at
// checkout. direction is "added" (item_added | quantity_increased) or
// "removed" (item_removed | quantity_decreased).
func (r *Repository) ListTopCheckoutMutations(ctx context.Context, storeID, eventID, direction string, limit int) ([]CheckoutMutationRow, error) {
	if direction != "added" && direction != "removed" {
		return nil, fmt.Errorf("invalid direction %q", direction)
	}
	if limit <= 0 {
		limit = 5
	}

	deltaExpr := "(cm.quantity_after - cm.quantity_before)"
	mutTypes := "('item_added','quantity_increased')"
	if direction == "removed" {
		deltaExpr = "(cm.quantity_before - cm.quantity_after)"
		mutTypes = "('item_removed','quantity_decreased')"
	}

	q := fmt.Sprintf(`
		SELECT
			cm.product_id,
			p.name,
			p.image_url,
			SUM(%s)::int AS units,
			SUM(%s * cm.unit_price)::bigint AS revenue_cents
		FROM cart_mutations cm
		JOIN carts c ON c.id = cm.cart_id
		JOIN live_events le ON le.id = c.event_id
		JOIN products p ON p.id = cm.product_id
		WHERE le.store_id = $1
		  AND c.payment_status = 'paid'
		  AND cm.source = 'buyer_checkout'
		  AND cm.mutation_type IN %s
		  AND ($2::uuid IS NULL OR c.event_id = $2::uuid)
		GROUP BY cm.product_id, p.name, p.image_url
		ORDER BY units DESC
		LIMIT $3
	`, deltaExpr, deltaExpr, mutTypes)

	var eventArg interface{}
	if eventID != "" {
		eventArg = eventID
	}
	rows, err := r.db.Query(ctx, q, storeID, eventArg, limit)
	if err != nil {
		return nil, fmt.Errorf("listing top mutations (%s): %w", direction, err)
	}
	defer rows.Close()
	out := make([]CheckoutMutationRow, 0)
	for rows.Next() {
		var row CheckoutMutationRow
		if err := rows.Scan(&row.ProductID, &row.ProductName, &row.ImageURL, &row.Units, &row.RevenueCents); err != nil {
			return nil, fmt.Errorf("scanning mutation row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
