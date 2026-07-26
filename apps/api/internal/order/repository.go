package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	// Read from `orders` (immutable paid records, Fatia 7).
	// total_amount comes from o.total_cents — never re-sums cart_items.
	// is_first_purchase checks the orders table (all rows are paid+).
	baseQuery := `
		SELECT
			c.id,
			o.short_id,
			o.event_id,
			c.platform_user_id,
			c.platform_handle,
			c.token,
			c.status,
			c.payment_status,
			c.paid_at,
			c.created_at,
			c.expires_at,
			COALESCE(c.customer_name, '') as customer_name,
			COALESCE(c.customer_email, '') as customer_email,
			COALESCE(e.title, '') as live_title,
			COALESCE(e.free_shipping, false) as free_shipping,
			COALESCE(
				(SELECT lsp.platform FROM live_session_platforms lsp
				 JOIN live_sessions ls ON ls.id = lsp.session_id
				 WHERE ls.event_id = e.id
				 ORDER BY lsp.added_at LIMIT 1),
				'instagram'
			) as live_platform,
			COALESCE(o.total_cents, 0) as total_amount,
			COALESCE(
				(SELECT SUM(ci.quantity)::INT FROM cart_items ci WHERE ci.cart_id = c.id),
				0
			) as total_items,
			(
				c.platform_user_id <> ''
				AND NOT EXISTS (
					SELECT 1
					FROM orders o2
					JOIN carts c2 ON c2.id = o2.cart_id
					WHERE o2.store_id = o.store_id
					  AND c2.platform_user_id = c.platform_user_id
					  AND o2.id <> o.id
					  AND o2.created_at < o.created_at
				)
			) as is_first_purchase,
			COALESCE(
				(SELECT sh.status FROM shipments sh
				 WHERE sh.order_id = c.id
				 ORDER BY sh.created_at DESC LIMIT 1),
				''
			) as shipment_status,
			(
				c.shipping_service_id IS NOT NULL
				AND c.shipping_service_id <> ''
				AND c.payment_status NOT IN ('cancelled', 'refunded')
			) as has_shipping,
			c.erp_finalisation_status
		FROM orders o
		JOIN carts c ON c.id = o.cart_id
		JOIN live_events e ON e.id = o.event_id
		WHERE o.store_id = $1
	`

	countQuery := `
		SELECT COUNT(*)
		FROM orders o
		JOIN carts c ON c.id = o.cart_id
		JOIN live_events e ON e.id = o.event_id
		WHERE o.store_id = $1
	`

	conditions, args := buildOrderListConditions(params.StoreID, params.Search, params.Filters)

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
	sortColumn := "o.created_at"
	allowedSortColumns := map[string]string{
		"created_at":     "o.created_at",
		"status":         "c.status",
		"payment_status": "c.payment_status",
		"total_amount":   "total_amount",
		"short_id":       "o.short_id",
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
			&row.ShortID,
			&row.EventID,
			&row.PlatformUserID,
			&row.PlatformHandle,
			&row.Token,
			&row.Status,
			&row.PaymentStatus,
			&row.PaidAt,
			&row.CreatedAt,
			&row.ExpiresAt,
			&row.CustomerName,
			&row.CustomerEmail,
			&row.LiveTitle,
			&row.FreeShipping,
			&row.LivePlatform,
			&row.TotalAmount,
			&row.TotalItems,
			&row.IsFirstPurchase,
			&row.ShipmentStatus,
			&row.HasShipping,
			&row.ERPFinalisationStatus,
		)
		if err != nil {
			return result, fmt.Errorf("scanning order row: %w", err)
		}
		result.Orders = append(result.Orders, row)
	}

	return result, nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*OrderDetailRow, error) {
	// Fatia B1: detalhe do pedido lido do agregado Order (imutável), não mais de
	// carts.*. customer_* vêm de orders.customer_snapshot, shipping_* de
	// order_logistics, erp_*/payment_status de order_payments, status/paid_at de
	// orders. LEFT JOIN + COALESCE dá fallback seguro para carts antigos que
	// nunca viraram Order (order_* ausente → defaults vazios). Os campos de
	// identidade do carrinho (short_id, token, platform_*, created_at,
	// expires_at) e o is_first_purchase permanecem na fonte carts.
	query := `
		SELECT
			c.id,
			c.short_id,
			c.event_id,
			c.platform_user_id,
			c.platform_handle,
			c.token,
			COALESCE(o.status, c.status),
			COALESCE(op.payment_status, c.payment_status),
			COALESCE(o.paid_at, c.paid_at),
			c.created_at,
			c.expires_at,
			COALESCE(e.title, '') as live_title,
			COALESCE(
				(SELECT lsp.platform FROM live_session_platforms lsp
				 JOIN live_sessions ls ON ls.id = lsp.session_id
				 WHERE ls.event_id = e.id
				 ORDER BY lsp.added_at LIMIT 1),
				'instagram'
			) as live_platform,
			e.store_id,
			(
				c.payment_status = 'paid'
				AND c.platform_user_id <> ''
				AND NOT EXISTS (
					SELECT 1
					FROM carts c2
					JOIN live_events e2 ON e2.id = c2.event_id
					WHERE e2.store_id = e.store_id
					  AND c2.platform_user_id = c.platform_user_id
					  AND c2.payment_status = 'paid'
					  AND c2.id <> c.id
					  AND COALESCE(c2.paid_at, c2.created_at) < COALESCE(c.paid_at, c.created_at)
				)
			) as is_first_purchase,

			COALESCE(o.customer_snapshot->>'email', ''),
			COALESCE(o.customer_snapshot->>'name', ''),
			COALESCE(o.customer_snapshot->>'document', ''),
			COALESCE(o.customer_snapshot->>'phone', ''),

			ol.shipping_address,

			COALESCE(ol.shipping_provider, ''),
			COALESCE(ol.shipping_service_id, ''),
			COALESCE(ol.shipping_service_name, ''),
			COALESCE(ol.shipping_carrier, ''),
			COALESCE(ol.shipping_cost_cents, 0),
			COALESCE(ol.shipping_cost_real_cents, 0),
			COALESCE(ol.shipping_deadline_days, 0),
			COALESCE(e.free_shipping, false),

			s.name,
			s.logo_url,
			COALESCE(s.cnpj, ''),
			COALESCE(s.email_address, ''),
			COALESCE(s.whatsapp_number, ''),
			COALESCE(s.address_zip, ''),
			COALESCE(s.address_street, ''),
			COALESCE(s.address_number, ''),
			COALESCE(s.address_complement, ''),
			COALESCE(s.address_district, ''),
			COALESCE(s.address_city, ''),
			COALESCE(s.address_state, ''),
			COALESCE(s.default_package_weight_grams, 0),
			COALESCE(s.default_package_format, 'box'),

			COALESCE(op.erp_finalisation_status, ''),
			COALESCE(op.erp_last_error, ''),
			op.erp_last_attempt_at,
			COALESCE(op.erp_attempts_count, 0),

			COALESCE(op.invoice_id, ''),
			COALESCE(op.invoice_key, ''),
			COALESCE(op.invoice_status, ''),
			op.invoice_emitted_at
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		JOIN stores s      ON s.id = e.store_id
		LEFT JOIN orders o          ON o.cart_id  = c.id
		LEFT JOIN order_payments op ON op.order_id = o.id
		LEFT JOIN order_logistics ol ON ol.order_id = o.id
		WHERE c.id = $1
	`

	var (
		row             OrderDetailRow
		shippingAddress []byte // JSONB
	)
	err := r.db.QueryRow(ctx, query, id).Scan(
		&row.ID,
		&row.ShortID,
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
		&row.IsFirstPurchase,

		&row.CustomerEmail,
		&row.CustomerName,
		&row.CustomerDocument,
		&row.CustomerPhone,

		&shippingAddress,

		&row.ShippingProvider,
		&row.ShippingServiceID,
		&row.ShippingServiceName,
		&row.ShippingCarrier,
		&row.ShippingCostCents,
		&row.ShippingCostRealCents,
		&row.ShippingDeadlineDays,
		&row.EventFreeShipping,

		&row.StoreName,
		&row.StoreLogoURL,
		&row.StoreCNPJ,
		&row.StoreEmail,
		&row.StorePhone,
		&row.StoreAddressZip,
		&row.StoreAddressStreet,
		&row.StoreAddressNumber,
		&row.StoreAddressComplement,
		&row.StoreAddressDistrict,
		&row.StoreAddressCity,
		&row.StoreAddressState,
		&row.StoreDefaultPkgWeightGrams,
		&row.StoreDefaultPkgFormat,

		&row.ERPFinalisationStatus,
		&row.ERPLastError,
		&row.ERPLastAttemptAt,
		&row.ERPAttemptsCount,

		&row.ERPInvoiceID,
		&row.ERPInvoiceKey,
		&row.ERPInvoiceStatus,
		&row.ERPInvoiceEmittedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting order by id: %w", err)
	}

	// Decode the shipping_address JSONB into the flat projection fields. The
	// column accepts any shape the checkout produces — we only pull known
	// fields and silently tolerate missing ones.
	if len(shippingAddress) > 0 {
		var addr struct {
			ZipCode      string `json:"zipCode"`
			Street       string `json:"street"`
			Number       string `json:"number"`
			Complement   string `json:"complement"`
			Neighborhood string `json:"neighborhood"`
			City         string `json:"city"`
			State        string `json:"state"`
		}
		if jerr := json.Unmarshal(shippingAddress, &addr); jerr == nil {
			row.ShippingAddressZip = addr.ZipCode
			row.ShippingAddressStreet = addr.Street
			row.ShippingAddressNumber = addr.Number
			row.ShippingAddressComplement = addr.Complement
			row.ShippingAddressNeighborhood = addr.Neighborhood
			row.ShippingAddressCity = addr.City
			row.ShippingAddressState = addr.State
		}
	}

	return &row, nil
}

func (r *Repository) GetItems(ctx context.Context, cartID string) ([]OrderItemRow, error) {
	// Fatia B1: itens do detalhe vêm de order_items (snapshot imutável). O nome
	// do produto é o congelado em order_items.product_name — não o vivo de
	// products (que pode ter sido renomeado). Imagem/keyword/dimensões seguem de
	// products, pois são dados de apresentação/frete, não valores congelados.
	// cartID é o identificador público; resolve-se order_id via orders.cart_id.
	query := `
		SELECT
			oi.id,
			o.cart_id,
			oi.product_id,
			NULL::TEXT as size,
			oi.quantity,
			COALESCE(oi.unit_price, 0)::BIGINT as unit_price,
			oi.product_name,
			p.image_url as product_image,
			p.keyword as product_keyword,
			COALESCE(p.weight_grams, 0),
			COALESCE(p.height_cm, 0),
			COALESCE(p.width_cm, 0),
			COALESCE(p.length_cm, 0),
			COALESCE(p.package_format, 'box')
		FROM order_items oi
		JOIN orders o   ON o.id = oi.order_id
		JOIN products p ON p.id = oi.product_id
		WHERE o.cart_id = $1
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
			&item.WeightGrams,
			&item.HeightCm,
			&item.WidthCm,
			&item.LengthCm,
			&item.PackageFormat,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning order item row: %w", err)
		}
		items = append(items, item)
	}

	return items, nil
}

// GetItemsPreviewByCartIDs returns a small per-cart preview of items (name +
// image + quantity) so the list page can render an avatar stack without
// loading every column from cart_items. Bulk-fetched in one query and grouped
// by cart_id on the caller side.
func (r *Repository) GetItemsPreviewByCartIDs(ctx context.Context, cartIDs []string) (map[string][]OrderItemPreviewRow, error) {
	out := make(map[string][]OrderItemPreviewRow, len(cartIDs))
	if len(cartIDs) == 0 {
		return out, nil
	}

	uuids := make([]pgtype.UUID, 0, len(cartIDs))
	for _, id := range cartIDs {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parsing cart id: %w", err)
		}
		uuids = append(uuids, pgtype.UUID{Bytes: parsed, Valid: true})
	}

	const q = `
		SELECT ci.cart_id::text, p.name, p.image_url, ci.quantity
		FROM cart_items ci
		JOIN products p ON p.id = ci.product_id
		WHERE ci.cart_id = ANY($1::uuid[])
		ORDER BY ci.cart_id, ci.id
	`
	rows, err := r.db.Query(ctx, q, uuids)
	if err != nil {
		return nil, fmt.Errorf("listing item previews: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cartID string
			row    OrderItemPreviewRow
		)
		if err := rows.Scan(&cartID, &row.ProductName, &row.ProductImage, &row.Quantity); err != nil {
			return nil, fmt.Errorf("scanning item preview: %w", err)
		}
		out[cartID] = append(out[cartID], row)
	}
	return out, rows.Err()
}

// UpdateShippingAddress overwrites the order's shipping_address JSONB on
// order_logistics (Fatia B1 — antes era carts). The caller is responsible for
// the upstream invariants (cannot edit after shipment exists, cannot edit a
// paid order); this is a thin write. `id` is the public cart id; the row is
// resolved via orders.cart_id. No-op (0 rows) for carts without an Order.
func (r *Repository) UpdateShippingAddress(ctx context.Context, id string, address map[string]string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("invalid order id: %w", err)
	}
	payload, err := json.Marshal(address)
	if err != nil {
		return fmt.Errorf("encoding shipping address: %w", err)
	}
	const q = `
		UPDATE order_logistics ol
		SET shipping_address = $2
		FROM orders o
		WHERE ol.order_id = o.id AND o.cart_id = $1
	`
	if _, err := r.db.Exec(ctx, q, pgtype.UUID{Bytes: uid, Valid: true}, payload); err != nil {
		return fmt.Errorf("updating shipping address: %w", err)
	}
	return nil
}

// RegenerateCheckout pushes expires_at forward and resets the cart back to a
// state where the buyer can complete payment again. checkout_url is cleared
// so the next public-checkout call generates a fresh one (avoids reusing a
// stale Mercado Pago link).
func (r *Repository) RegenerateCheckout(ctx context.Context, id string, expiresAt time.Time) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("invalid order id: %w", err)
	}
	const q = `
		UPDATE carts
		SET expires_at         = $2,
		    status             = 'active',
		    payment_status     = 'pending',
		    checkout_url       = NULL,
		    checkout_id        = NULL,
		    checkout_expires_at = NULL
		WHERE id = $1
	`
	if _, err := r.db.Exec(ctx, q, pgtype.UUID{Bytes: uid, Valid: true}, expiresAt); err != nil {
		return fmt.Errorf("regenerating checkout: %w", err)
	}
	return nil
}

// GetStoreCartExpirationMinutes returns the merchant-configured cart TTL
// (used to compute new expires_at when regenerating). Falls back to 30 if
// the lookup fails — same default the checkout package uses.
func (r *Repository) GetStoreCartExpirationMinutes(ctx context.Context, storeID string) int {
	uid, err := uuid.Parse(storeID)
	if err != nil {
		return 30
	}
	var minutes int
	const q = `SELECT cart_expiration_minutes FROM stores WHERE id = $1`
	if err := r.db.QueryRow(ctx, q, pgtype.UUID{Bytes: uid, Valid: true}).Scan(&minutes); err != nil {
		return 30
	}
	if minutes <= 0 {
		return 30
	}
	return minutes
}

// UpdateStatus writes the order lifecycle status to orders.status (Fatia B1 —
// antes era carts.status). `id` is the public cart id; resolved via
// orders.cart_id. No-op (0 rows) for carts without an Order.
func (r *Repository) UpdateStatus(ctx context.Context, id string, status string) error {
	query := `UPDATE orders SET status = $2, updated_at = now() WHERE cart_id = $1`
	_, err := r.db.Exec(ctx, query, id, status)
	if err != nil {
		return fmt.Errorf("updating order status: %w", err)
	}
	return nil
}

// UpdatePaymentStatus writes the payment status to order_payments.payment_status
// and reflects the paid_at instant on orders (Fatia B1 — antes era UPDATE
// carts). Both writes run in one transaction so the detail never observes a
// half-applied update. `id` is the public cart id; resolved via orders.cart_id.
// No-op for carts without an Order.
func (r *Repository) UpdatePaymentStatus(ctx context.Context, id string, paymentStatus string) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("updating order payment status: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const payQ = `
		UPDATE order_payments op
		SET payment_status = $2::text
		FROM orders o
		WHERE op.order_id = o.id AND o.cart_id = $1
	`
	if _, err := tx.Exec(ctx, payQ, id, paymentStatus); err != nil {
		return fmt.Errorf("updating order payment status: %w", err)
	}

	const paidAtQ = `
		UPDATE orders
		SET paid_at = CASE WHEN $2::text = 'paid' THEN now() ELSE paid_at END,
		    updated_at = now()
		WHERE cart_id = $1
	`
	if _, err := tx.Exec(ctx, paidAtQ, id, paymentStatus); err != nil {
		return fmt.Errorf("updating order paid_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("updating order payment status: commit: %w", err)
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

func (r *Repository) GetStats(ctx context.Context, storeID string, search string, filters OrderFilters) (*OrderStatsOutput, error) {
	// Revenue and ticket use o.total_cents (immutable) — never re-sums cart_items.
	query := `
		SELECT
			COUNT(*)::INT as total_orders,
			COUNT(*) FILTER (WHERE c.status = 'active')::INT as pending_orders,
			COALESCE(SUM(o.total_cents), 0)::BIGINT as total_revenue,
			COALESCE(
				CASE
					WHEN COUNT(*) > 0 THEN SUM(o.total_cents) / COUNT(*)
					ELSE 0
				END,
				0
			)::BIGINT as avg_ticket
		FROM orders o
		JOIN carts c ON c.id = o.cart_id
		JOIN live_events e ON e.id = o.event_id
		WHERE o.store_id = $1
	`

	conditions, args := buildOrderListConditions(storeID, search, filters)
	if len(conditions) > 0 {
		query += " AND " + strings.Join(conditions, " AND ")
	}

	var stats OrderStatsOutput
	err := r.db.QueryRow(ctx, query, args...).Scan(
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

// buildOrderListConditions builds the WHERE conditions (without leading "AND")
// and the matching positional args for the order list queries. The returned
// args always start with storeID so callers can prepend the base WHERE clause
// `WHERE e.store_id = $1` and append " AND <conditions>" when non-empty.
func buildOrderListConditions(storeID string, search string, filters OrderFilters) ([]string, []interface{}) {
	args := []interface{}{storeID}
	argIndex := 2
	var conditions []string

	// Trim a leading "#" so merchants can paste "#1247" or "1247" interchangeably
	// — the UI renders the prefix decoratively, so it's not part of the data.
	trimmedSearch := strings.TrimSpace(search)
	trimmedSearch = strings.TrimPrefix(trimmedSearch, "#")
	if trimmedSearch != "" {
		conditions = append(conditions, fmt.Sprintf(
			"(c.platform_handle ILIKE $%d OR c.id::TEXT ILIKE $%d OR c.short_id::TEXT ILIKE $%d OR c.customer_name ILIKE $%d OR c.customer_email ILIKE $%d)",
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

	if len(filters.PaymentStatus) > 0 {
		placeholders := make([]string, len(filters.PaymentStatus))
		for i, ps := range filters.PaymentStatus {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, ps)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf("c.payment_status IN (%s)", strings.Join(placeholders, ",")))
	}

	if filters.LiveSessionID != nil && *filters.LiveSessionID != "" {
		conditions = append(conditions, fmt.Sprintf("c.event_id = $%d", argIndex))
		args = append(args, *filters.LiveSessionID)
		argIndex++
	}

	if filters.DateFrom != nil && *filters.DateFrom != "" {
		conditions = append(conditions, fmt.Sprintf("c.created_at >= $%d", argIndex))
		args = append(args, *filters.DateFrom)
		argIndex++
	}
	if filters.DateTo != nil && *filters.DateTo != "" {
		conditions = append(conditions, fmt.Sprintf("c.created_at <= $%d", argIndex))
		args = append(args, *filters.DateTo)
		argIndex++
	}

	// Shipment status filter implies HasShipment=true, so when both are set the
	// status filter wins. NOT EXISTS is the only way to ask for orders without
	// any shipment row.
	if len(filters.ShipmentStatus) > 0 {
		placeholders := make([]string, len(filters.ShipmentStatus))
		for i, st := range filters.ShipmentStatus {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, st)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM shipments sh WHERE sh.order_id = c.id AND sh.status IN (%s))",
			strings.Join(placeholders, ","),
		))
	} else if filters.HasShipment != nil {
		if *filters.HasShipment {
			conditions = append(conditions, "EXISTS (SELECT 1 FROM shipments sh WHERE sh.order_id = c.id)")
		} else {
			conditions = append(conditions, "NOT EXISTS (SELECT 1 FROM shipments sh WHERE sh.order_id = c.id)")
		}
	}

	if len(filters.ERPFinalisation) > 0 {
		placeholders := make([]string, len(filters.ERPFinalisation))
		for i, st := range filters.ERPFinalisation {
			placeholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, st)
			argIndex++
		}
		conditions = append(conditions, fmt.Sprintf(
			"c.erp_finalisation_status IN (%s)",
			strings.Join(placeholders, ","),
		))
	}

	// NeedsAttention is tri-state. true = only rows where any failure surface
	// is hit; false = exclude those rows so non-attention tabs ("Para
	// despachar", "Em trânsito", …) don't double-count problem orders that
	// also happen to match their primary intent (e.g. paid + no shipment +
	// ERP failed shows up in "Para despachar" without this).
	//
	// The OR covers:
	//   - Payment gateway failed or refunded the charge (cart left mid-flow).
	//   - Carrier rejected the shipment / NF-e blocked / package damaged /
	//     refused / not delivered.
	//   - ERP finalisation rejected the paid cart (any provider — Tiny today,
	//     Bling next; the cart column is provider-agnostic).
	if filters.NeedsAttention != nil {
		shipmentIssueStatuses := []string{
			"awaiting_invoice",
			"issue",
			"delivery_issue",
			"delivery_blocked",
			"shipment_blocked",
			"fiscal_issue",
			"damaged",
			"refused",
			"not_delivered",
		}
		shipmentPlaceholders := make([]string, len(shipmentIssueStatuses))
		for i, st := range shipmentIssueStatuses {
			shipmentPlaceholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, st)
			argIndex++
		}
		paymentIssueStatuses := []string{"failed", "refunded"}
		paymentPlaceholders := make([]string, len(paymentIssueStatuses))
		for i, st := range paymentIssueStatuses {
			paymentPlaceholders[i] = fmt.Sprintf("$%d", argIndex)
			args = append(args, st)
			argIndex++
		}
		matcher := fmt.Sprintf(
			"(c.erp_finalisation_status = 'failed' OR c.payment_status IN (%s) OR EXISTS (SELECT 1 FROM shipments sh WHERE sh.order_id = c.id AND sh.status IN (%s)))",
			strings.Join(paymentPlaceholders, ","),
			strings.Join(shipmentPlaceholders, ","),
		)
		if *filters.NeedsAttention {
			conditions = append(conditions, matcher)
		} else {
			conditions = append(conditions, "NOT "+matcher)
		}
	}

	return conditions, args
}

// GetShipmentForOrder returns the shipment attached to an order (at most one).
// Returns nil, nil when no shipment has been created yet — the order detail
// handler exposes that as `shipment: null`.
func (r *Repository) GetShipmentForOrder(ctx context.Context, orderID string) (*OrderShipmentRecord, error) {
	uid, err := uuid.Parse(orderID)
	if err != nil {
		return nil, fmt.Errorf("invalid order id: %w", err)
	}
	const q = `
		SELECT
			id::text,
			provider,
			provider_order_id,
			COALESCE(provider_order_number, ''),
			COALESCE(tracking_code, ''),
			COALESCE(public_tracking_url, ''),
			COALESCE(invoice_key, ''),
			COALESCE(invoice_kind, ''),
			COALESCE(label_url, ''),
			status,
			COALESCE(status_raw_code, 0),
			COALESCE(status_raw_name, ''),
			created_at,
			updated_at
		FROM shipments
		WHERE order_id = $1
		ORDER BY created_at DESC
		LIMIT 1
	`
	var rec OrderShipmentRecord
	err = r.db.QueryRow(ctx, q, pgtype.UUID{Bytes: uid, Valid: true}).Scan(
		&rec.ID,
		&rec.Provider,
		&rec.ProviderOrderID,
		&rec.ProviderOrderNumber,
		&rec.TrackingCode,
		&rec.PublicTrackingURL,
		&rec.InvoiceKey,
		&rec.InvoiceKind,
		&rec.LabelURL,
		&rec.Status,
		&rec.StatusRawCode,
		&rec.StatusRawName,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting shipment for order: %w", err)
	}
	return &rec, nil
}

// ListShipmentEvents returns the tracking timeline for a shipment, ascending
// by event_at so the UI can render it in chronological order (older first).
func (r *Repository) ListShipmentEvents(ctx context.Context, shipmentID string) ([]OrderShipmentEventRecord, error) {
	uid, err := uuid.Parse(shipmentID)
	if err != nil {
		return nil, fmt.Errorf("invalid shipment id: %w", err)
	}
	const q = `
		SELECT
			status,
			COALESCE(raw_code, 0),
			COALESCE(raw_name, ''),
			COALESCE(observation, ''),
			event_at,
			source
		FROM shipment_tracking_events
		WHERE shipment_id = $1
		ORDER BY event_at ASC, received_at ASC
	`
	rows, err := r.db.Query(ctx, q, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("listing shipment events: %w", err)
	}
	defer rows.Close()

	var out []OrderShipmentEventRecord
	for rows.Next() {
		var e OrderShipmentEventRecord
		if err := rows.Scan(&e.Status, &e.RawCode, &e.RawName, &e.Observation, &e.EventAt, &e.Source); err != nil {
			return nil, fmt.Errorf("scanning shipment event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// =============================================================================
// UPSELL / DOWNSELL — initial cart vs final paid cart
// =============================================================================

// GetUpsellSummary builds the per-order upsell card payload: initial subtotal
// (snapshot taken on first checkout view), final subtotal, the immutable
// initial item list, and the mutation log. Returns nil when the order has no
// snapshot at all (cart predates the feature) so the handler can render a
// neutral "no changes" state.
func (r *Repository) GetUpsellSummary(ctx context.Context, orderID string) (*OrderUpsellOutput, error) {
	uid, err := uuid.Parse(orderID)
	if err != nil {
		return nil, fmt.Errorf("invalid order id: %w", err)
	}
	cartUUID := pgtype.UUID{Bytes: uid, Valid: true}

	headerQ := `
		SELECT
			COALESCE(c.initial_subtotal_cents, 0)::bigint                                AS initial_subtotal_cents,
			c.initial_snapshot_taken_at,
			COALESCE((
				SELECT SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price)
				FROM cart_items ci
				WHERE ci.cart_id = c.id AND ci.quantity > ci.waitlisted_quantity
			), 0)::bigint                                                                AS final_subtotal_cents,
			COALESCE((
				SELECT COUNT(*) FROM cart_mutations m WHERE m.cart_id = c.id AND m.source = 'buyer_checkout'
			), 0)::int                                                                   AS mutation_count
		FROM carts c
		WHERE c.id = $1
	`
	var (
		initialCents pgtype.Int8
		snapAt       pgtype.Timestamptz
		finalCents   pgtype.Int8
		mutCount     int
	)
	if err := r.db.QueryRow(ctx, headerQ, cartUUID).Scan(&initialCents, &snapAt, &finalCents, &mutCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("loading upsell header: %w", err)
	}

	out := &OrderUpsellOutput{
		InitialSubtotalCents: initialCents.Int64,
		FinalSubtotalCents:   finalCents.Int64,
		DeltaCents:           finalCents.Int64 - initialCents.Int64,
		MutationCount:        mutCount,
		HasSnapshot:          snapAt.Valid,
	}
	if snapAt.Valid {
		t := snapAt.Time
		out.SnapshotTakenAt = &t
	}

	// Initial items list (immutable baseline).
	itemsQ := `
		SELECT cii.product_id, cii.quantity, cii.unit_price,
		       p.name, p.image_url, p.keyword
		FROM cart_initial_items cii
		JOIN products p ON p.id = cii.product_id
		WHERE cii.cart_id = $1
		ORDER BY p.name
	`
	rows, err := r.db.Query(ctx, itemsQ, cartUUID)
	if err != nil {
		return nil, fmt.Errorf("listing initial items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pid     pgtype.UUID
			qty     int
			unitP   int64
			name    string
			imgURL  pgtype.Text
			keyword string
		)
		if err := rows.Scan(&pid, &qty, &unitP, &name, &imgURL, &keyword); err != nil {
			return nil, fmt.Errorf("scanning initial item: %w", err)
		}
		item := OrderUpsellItem{
			ProductID: uuid.UUID(pid.Bytes).String(),
			Name:      name,
			Keyword:   keyword,
			Quantity:  qty,
			UnitPrice: unitP,
		}
		if imgURL.Valid {
			s := imgURL.String
			item.ImageURL = &s
		}
		out.InitialItems = append(out.InitialItems, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating initial items: %w", err)
	}

	// Mutation log (timeline) ordered chronologically.
	mutQ := `
		SELECT cm.id, cm.product_id, cm.mutation_type,
		       cm.quantity_before, cm.quantity_after,
		       cm.unit_price, cm.source, cm.created_at,
		       p.name, p.image_url, p.keyword
		FROM cart_mutations cm
		JOIN products p ON p.id = cm.product_id
		WHERE cm.cart_id = $1
		ORDER BY cm.created_at ASC
	`
	mrows, err := r.db.Query(ctx, mutQ, cartUUID)
	if err != nil {
		return nil, fmt.Errorf("listing mutations: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			id        pgtype.UUID
			pid       pgtype.UUID
			mtype     string
			qBefore   int
			qAfter    int
			unitP     int64
			source    string
			createdAt pgtype.Timestamptz
			name      string
			imgURL    pgtype.Text
			keyword   string
		)
		if err := mrows.Scan(&id, &pid, &mtype, &qBefore, &qAfter, &unitP, &source, &createdAt, &name, &imgURL, &keyword); err != nil {
			return nil, fmt.Errorf("scanning mutation: %w", err)
		}
		m := OrderUpsellMutation{
			ID:             uuid.UUID(id.Bytes).String(),
			ProductID:      uuid.UUID(pid.Bytes).String(),
			ProductName:    name,
			Keyword:        keyword,
			MutationType:   mtype,
			QuantityBefore: qBefore,
			QuantityAfter:  qAfter,
			UnitPrice:      unitP,
			Source:         source,
			CreatedAt:      createdAt.Time,
		}
		if imgURL.Valid {
			s := imgURL.String
			m.ImageURL = &s
		}
		out.Mutations = append(out.Mutations, m)
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("iterating mutations: %w", err)
	}

	return out, nil
}
