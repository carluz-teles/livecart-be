package dashboard

// Dashboard redesign (jul/2026): range-aware overview. Every KPI, the funnel
// (with exit states: expired / recovered / refunded) and the revenue series
// answer for the SAME [from, to] window — replacing the old mix of all-time
// cards, hardcoded 30d funnel and calendar-year chart.

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// TYPES
// =============================================================================

// OverviewRow bundles KPIs + funnel-with-states for one time window.
type OverviewRow struct {
	// KPIs
	GMVCents      int64 `json:"gmvCents"`      // pedidos pagos no período (paid_at)
	PaidOrders    int   `json:"paidOrders"`    //
	AverageTicket int64 `json:"averageTicket"` // gmv / pedidos (0-safe)

	// Funil (por created_at do carrinho, exceto pagos/estorno por transição)
	Lives         int   `json:"lives"`
	TotalComments int   `json:"totalComments"`
	TotalCarts    int   `json:"totalCarts"`
	CheckoutCarts int   `json:"checkoutCarts"`
	PaidCarts     int   `json:"paidCarts"`

	// Estados de saída
	ExpiredCarts     int   `json:"expiredCarts"`
	ExpiredValue     int64 `json:"expiredValueCents"` // dinheiro em risco (gancho da recuperação)
	RecoveredCarts   int   `json:"recoveredCarts"`    // pagos após mensagem de recuperação (PRD 006)
	RecoveredRevenue int64 `json:"recoveredRevenueCents"`
	RefundedCarts    int   `json:"refundedCarts"`
}

// SeriesPoint is one revenue bucket for the adaptive chart.
type SeriesPoint struct {
	Bucket  string `json:"bucket"` // ISO date of the bucket start
	Revenue int64  `json:"revenueCents"`
	Orders  int    `json:"orders"`
}

// =============================================================================
// REPOSITORY
// =============================================================================

// GetOverviewRange computes KPIs + funnel states for [from, to].
func (r *Repository) GetOverviewRange(ctx context.Context, storeID string, from, to time.Time) (*OverviewRow, error) {
	query := `
		SELECT
			-- KPIs: pedidos PAGOS dentro do período (por paid_at)
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM carts c
				JOIN cart_items ci ON ci.cart_id = c.id
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.payment_status = 'paid'
				  AND c.paid_at >= $2 AND c.paid_at < $3
			), 0)::BIGINT AS gmv_cents,
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.payment_status = 'paid'
				  AND c.paid_at >= $2 AND c.paid_at < $3
			), 0)::INT AS paid_orders,

			-- Funil: atividade criada no período
			COALESCE((
				SELECT COUNT(DISTINCT ls.event_id)
				FROM live_sessions ls
				JOIN live_events e ON e.id = ls.event_id
				WHERE e.store_id = $1 AND ls.created_at >= $2 AND ls.created_at < $3
			), 0)::INT AS lives,
			COALESCE((
				SELECT SUM(ls.total_comments)
				FROM live_sessions ls
				JOIN live_events e ON e.id = ls.event_id
				WHERE e.store_id = $1 AND ls.created_at >= $2 AND ls.created_at < $3
			), 0)::INT AS total_comments,
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= $2 AND c.created_at < $3
			), 0)::INT AS total_carts,
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= $2 AND c.created_at < $3
				  AND (c.status = 'checkout' OR c.checkout_url IS NOT NULL)
			), 0)::INT AS checkout_carts,
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= $2 AND c.created_at < $3
				  AND c.payment_status = 'paid'
			), 0)::INT AS paid_carts,

			-- Saída: expirados sem pagar (dinheiro em risco)
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= $2 AND c.created_at < $3
				  AND c.payment_status IS DISTINCT FROM 'paid'
				  AND c.expires_at IS NOT NULL AND c.expires_at < NOW()
			), 0)::INT AS expired_carts,
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM carts c
				JOIN cart_items ci ON ci.cart_id = c.id
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.created_at >= $2 AND c.created_at < $3
				  AND c.payment_status IS DISTINCT FROM 'paid'
				  AND c.expires_at IS NOT NULL AND c.expires_at < NOW()
			), 0)::BIGINT AS expired_value,

			-- Saída: recuperados (pagos após mensagem cart_recovery — PRD 006)
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.payment_status = 'paid'
				  AND c.paid_at >= $2 AND c.paid_at < $3
				  AND EXISTS (
					SELECT 1 FROM notification_logs nl
					WHERE nl.cart_id = c.id AND nl.notification_type = 'cart_recovery'
					  AND nl.status IN ('sent', 'delivered', 'read')
					  AND nl.created_at < c.paid_at
				  )
			), 0)::INT AS recovered_carts,
			COALESCE((
				SELECT SUM(ci.quantity * ci.unit_price)
				FROM carts c
				JOIN cart_items ci ON ci.cart_id = c.id
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.payment_status = 'paid'
				  AND c.paid_at >= $2 AND c.paid_at < $3
				  AND EXISTS (
					SELECT 1 FROM notification_logs nl
					WHERE nl.cart_id = c.id AND nl.notification_type = 'cart_recovery'
					  AND nl.status IN ('sent', 'delivered', 'read')
					  AND nl.created_at < c.paid_at
				  )
			), 0)::BIGINT AS recovered_revenue,

			-- Saída: estornados no período
			COALESCE((
				SELECT COUNT(*)
				FROM carts c
				JOIN live_events e ON e.id = c.event_id
				WHERE e.store_id = $1 AND c.payment_status = 'refunded'
				  AND c.created_at >= $2 AND c.created_at < $3
			), 0)::INT AS refunded_carts
	`

	var row OverviewRow
	err := r.db.QueryRow(ctx, query, storeID, from, to).Scan(
		&row.GMVCents,
		&row.PaidOrders,
		&row.Lives,
		&row.TotalComments,
		&row.TotalCarts,
		&row.CheckoutCarts,
		&row.PaidCarts,
		&row.ExpiredCarts,
		&row.ExpiredValue,
		&row.RecoveredCarts,
		&row.RecoveredRevenue,
		&row.RefundedCarts,
	)
	if err != nil {
		return nil, fmt.Errorf("getting overview range: %w", err)
	}
	if row.PaidOrders > 0 {
		row.AverageTicket = row.GMVCents / int64(row.PaidOrders)
	}
	return &row, nil
}

// GetRevenueSeriesRange buckets paid revenue by day/week/month.
func (r *Repository) GetRevenueSeriesRange(ctx context.Context, storeID string, from, to time.Time, bucket string) ([]SeriesPoint, error) {
	switch bucket {
	case "day", "week", "month":
	default:
		bucket = "day"
	}

	query := `
		SELECT
			date_trunc($4, c.paid_at)::date::text AS bucket,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT AS revenue,
			COUNT(DISTINCT c.id)::INT AS orders
		FROM carts c
		JOIN cart_items ci ON ci.cart_id = c.id
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1 AND c.payment_status = 'paid'
		  AND c.paid_at >= $2 AND c.paid_at < $3
		GROUP BY 1
		ORDER BY 1
	`

	rows, err := r.db.Query(ctx, query, storeID, from, to, bucket)
	if err != nil {
		return nil, fmt.Errorf("getting revenue series: %w", err)
	}
	defer rows.Close()

	points := []SeriesPoint{}
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Revenue, &p.Orders); err != nil {
			return nil, fmt.Errorf("scanning series point: %w", err)
		}
		points = append(points, p)
	}
	return points, nil
}

// GetTopProductsRange mirrors GetTopProducts filtered to paid carts in range.
func (r *Repository) GetTopProductsRange(ctx context.Context, storeID string, from, to time.Time) ([]TopProductRow, error) {
	query := `
		SELECT
			p.id, p.name, p.keyword,
			COALESCE(SUM(ci.quantity), 0)::INT AS total_sold,
			COALESCE(SUM(ci.quantity * ci.unit_price), 0)::BIGINT AS total_revenue
		FROM products p
		JOIN cart_items ci ON ci.product_id = p.id
		JOIN carts c ON c.id = ci.cart_id
		JOIN live_events le ON le.id = c.event_id
		WHERE le.store_id = $1 AND c.payment_status = 'paid'
		  AND c.paid_at >= $2 AND c.paid_at < $3
		GROUP BY p.id, p.name, p.keyword
		ORDER BY total_sold DESC
		LIMIT 5
	`
	rows, err := r.db.Query(ctx, query, storeID, from, to)
	if err != nil {
		return nil, fmt.Errorf("getting top products range: %w", err)
	}
	defer rows.Close()

	products := []TopProductRow{}
	for rows.Next() {
		var row TopProductRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Keyword, &row.TotalSold, &row.TotalRevenue); err != nil {
			return nil, fmt.Errorf("scanning top product row: %w", err)
		}
		products = append(products, row)
	}
	return products, nil
}

// GetTopBuyersRange mirrors GetTopBuyers filtered to paid carts in range.
func (r *Repository) GetTopBuyersRange(ctx context.Context, storeID string, from, to time.Time) ([]TopBuyerRow, error) {
	query := `
		SELECT
			c.platform_user_id AS id,
			c.platform_handle AS handle,
			COUNT(DISTINCT c.id)::INT AS total_orders,
			COALESCE(SUM(
				(SELECT COALESCE(SUM(ci.quantity * ci.unit_price), 0) FROM cart_items ci WHERE ci.cart_id = c.id)
			), 0)::BIGINT AS total_spent,
			MAX(c.created_at) AS last_purchase_at
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		WHERE e.store_id = $1 AND c.payment_status = 'paid'
		  AND c.paid_at >= $2 AND c.paid_at < $3
		GROUP BY c.platform_user_id, c.platform_handle
		ORDER BY total_spent DESC
		LIMIT 5
	`
	rows, err := r.db.Query(ctx, query, storeID, from, to)
	if err != nil {
		return nil, fmt.Errorf("getting top buyers range: %w", err)
	}
	defer rows.Close()

	buyers := []TopBuyerRow{}
	for rows.Next() {
		var row TopBuyerRow
		var lastPurchaseAt time.Time
		if err := rows.Scan(&row.ID, &row.Handle, &row.TotalOrders, &row.TotalSpent, &lastPurchaseAt); err != nil {
			return nil, fmt.Errorf("scanning top buyer row: %w", err)
		}
		row.LastPurchaseAt = lastPurchaseAt.Format("2006-01-02T15:04:05Z")
		buyers = append(buyers, row)
	}
	return buyers, nil
}

// =============================================================================
// SERVICE
// =============================================================================

func (s *Service) GetOverviewRange(ctx context.Context, storeID string, from, to time.Time) (*OverviewRow, error) {
	return s.repo.GetOverviewRange(ctx, storeID, from, to)
}

func (s *Service) GetRevenueSeriesRange(ctx context.Context, storeID string, from, to time.Time, bucket string) ([]SeriesPoint, error) {
	return s.repo.GetRevenueSeriesRange(ctx, storeID, from, to, bucket)
}

func (s *Service) GetTopProductsRange(ctx context.Context, storeID string, from, to time.Time) ([]TopProductRow, error) {
	return s.repo.GetTopProductsRange(ctx, storeID, from, to)
}

func (s *Service) GetTopBuyersRange(ctx context.Context, storeID string, from, to time.Time) ([]TopBuyerRow, error) {
	return s.repo.GetTopBuyersRange(ctx, storeID, from, to)
}

// =============================================================================
// HANDLERS
// =============================================================================

// parseRange reads from/to (RFC3339 or YYYY-MM-DD), defaulting to last 30d.
// `to` is exclusive-upper (we add 1 day to date-only inputs so "today" counts).
func parseRange(c *fiber.Ctx) (time.Time, time.Time, error) {
	now := time.Now()
	from := now.AddDate(0, 0, -30)
	to := now

	parse := func(s string, dateOnlyBump bool) (time.Time, error) {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return time.Time{}, err
		}
		if dateOnlyBump {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}

	if v := c.Query("from"); v != "" {
		t, err := parse(v, false)
		if err != nil {
			return from, to, fmt.Errorf("parâmetro 'from' inválido")
		}
		from = t
	}
	if v := c.Query("to"); v != "" {
		t, err := parse(v, true)
		if err != nil {
			return from, to, fmt.Errorf("parâmetro 'to' inválido")
		}
		to = t
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("'to' deve ser posterior a 'from'")
	}
	return from, to, nil
}

// GetOverview returns range-aware KPIs + funnel with exit states.
// @Summary Dashboard overview for a period
// @Tags dashboard
// @Produce json
// @Param storeId path string true "Store ID"
// @Param from query string false "Start (YYYY-MM-DD ou RFC3339); default: 30d atrás"
// @Param to query string false "End (YYYY-MM-DD ou RFC3339); default: agora"
// @Success 200 {object} httpx.Envelope{data=OverviewRow}
// @Router /api/v1/stores/{storeId}/dashboard/overview [get]
// @Security BearerAuth
func (h *Handler) GetOverview(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	from, to, err := parseRange(c)
	if err != nil {
		return httpx.BadRequest(c, err.Error())
	}
	row, err := h.service.GetOverviewRange(c.Context(), storeID, from, to)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, row)
}

// GetRevenueSeries returns the adaptive revenue chart series.
// @Summary Revenue series for a period
// @Tags dashboard
// @Produce json
// @Param storeId path string true "Store ID"
// @Param from query string false "Start"
// @Param to query string false "End"
// @Param bucket query string false "day|week|month (default day)"
// @Success 200 {object} httpx.Envelope{data=[]SeriesPoint}
// @Router /api/v1/stores/{storeId}/dashboard/series [get]
// @Security BearerAuth
func (h *Handler) GetRevenueSeries(c *fiber.Ctx) error {
	storeID := c.Locals("store_id").(string)
	from, to, err := parseRange(c)
	if err != nil {
		return httpx.BadRequest(c, err.Error())
	}
	points, err := h.service.GetRevenueSeriesRange(c.Context(), storeID, from, to, c.Query("bucket", "day"))
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, points)
}
