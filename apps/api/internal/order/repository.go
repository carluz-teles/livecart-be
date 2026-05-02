package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

	// Build dynamic query.
	//
	// `is_first_purchase` is true only for paid carts that have no earlier
	// paid cart from the same buyer in the same store. Anonymous carts
	// (empty platform_user_id) always evaluate to false because the EXISTS
	// subquery would match every other anon cart on the store.
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
			) as total_items,
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
			) as is_first_purchase
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
			&row.IsFirstPurchase,
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

			COALESCE(c.customer_email, ''),
			COALESCE(c.customer_name, ''),
			COALESCE(c.customer_document, ''),
			COALESCE(c.customer_phone, ''),

			c.shipping_address,

			COALESCE(c.shipping_provider, ''),
			COALESCE(c.shipping_service_id, ''),
			COALESCE(c.shipping_service_name, ''),
			COALESCE(c.shipping_carrier, ''),
			COALESCE(c.shipping_cost_cents, 0),
			COALESCE(c.shipping_cost_real_cents, 0),
			COALESCE(c.shipping_deadline_days, 0),
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
			COALESCE(s.default_package_format, 'box')
		FROM carts c
		JOIN live_events e ON e.id = c.event_id
		JOIN stores s      ON s.id = e.store_id
		WHERE c.id = $1
	`

	var (
		row             OrderDetailRow
		shippingAddress []byte // JSONB
	)
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
	query := `
		SELECT
			ci.id,
			ci.cart_id,
			ci.product_id,
			NULL::TEXT as size,
			ci.quantity,
			COALESCE(ci.unit_price, 0)::BIGINT as unit_price,
			p.name as product_name,
			p.image_url as product_image,
			p.keyword as product_keyword,
			COALESCE(p.weight_grams, 0),
			COALESCE(p.height_cm, 0),
			COALESCE(p.width_cm, 0),
			COALESCE(p.length_cm, 0),
			COALESCE(p.package_format, 'box')
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
		InitialSubtotalCents:   initialCents.Int64,
		FinalSubtotalCents:     finalCents.Int64,
		DeltaCents:             finalCents.Int64 - initialCents.Int64,
		MutationCount:          mutCount,
		HasSnapshot:            snapAt.Valid,
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

