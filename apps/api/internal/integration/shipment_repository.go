package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/lib/httpx"
)

// =============================================================================
// SHIPMENTS REPOSITORY
// =============================================================================
//
// Persistence for provider-created freight orders. Lives in the integration
// module because the creation/mutation is driven by the shipping handlers —
// but the Row shape is intentionally self-contained so the order module can
// query it via a simple join helper (see GetShipmentByOrderID) without
// depending on anything else in this package.

// ShipmentRow mirrors the `shipments` table.
type ShipmentRow struct {
	ID                  string
	OrderID             string
	StoreID             string
	Provider            string
	ProviderOrderID     string
	ProviderOrderNumber string
	TrackingCode        string
	PublicTrackingURL   string
	InvoiceKey          string
	InvoiceKind         string
	InvoiceID           string
	LabelURL            string
	Status              string
	StatusRawCode       int
	StatusRawName       string
	ProviderMeta        json.RawMessage
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// ShipmentEventRow mirrors the `shipment_tracking_events` table.
type ShipmentEventRow struct {
	ID          string
	ShipmentID  string
	Status      string
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
	ReceivedAt  time.Time
	Source      string
}

// CreateShipmentParams is the input for ShipmentRepository.CreateShipment.
type CreateShipmentParams struct {
	OrderID             string
	StoreID             string
	Provider            string
	ProviderOrderID     string
	ProviderOrderNumber string
	TrackingCode        string
	InvoiceKey          string
	InvoiceKind         string
	InvoiceID           string
	Status              string
	StatusRawCode       int
	StatusRawName       string
	ProviderMeta        map[string]any
}

// CreateShipment inserts a row. If a shipment already exists for this
// (provider, provider_order_id) tuple — for example because the admin
// retried — the row is updated in place and returned.
func (r *Repository) CreateShipment(ctx context.Context, p CreateShipmentParams) (*ShipmentRow, error) {
	orderUID, err := uuid.Parse(p.OrderID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid order id")
	}
	storeUID, err := uuid.Parse(p.StoreID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid store id")
	}

	metaRaw := json.RawMessage("{}")
	if len(p.ProviderMeta) > 0 {
		b, err := json.Marshal(p.ProviderMeta)
		if err != nil {
			return nil, fmt.Errorf("marshaling provider_meta: %w", err)
		}
		metaRaw = b
	}

	const q = `
		INSERT INTO shipments (
			order_id, store_id, provider, provider_order_id, provider_order_number,
			tracking_code, invoice_key, invoice_kind, invoice_id,
			status, status_raw_code, status_raw_name, provider_meta
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13
		)
		ON CONFLICT (provider, provider_order_id) DO UPDATE SET
			tracking_code         = COALESCE(NULLIF(EXCLUDED.tracking_code,  ''), shipments.tracking_code),
			provider_order_number = COALESCE(NULLIF(EXCLUDED.provider_order_number, ''), shipments.provider_order_number),
			invoice_key           = COALESCE(NULLIF(EXCLUDED.invoice_key,    ''), shipments.invoice_key),
			invoice_kind          = COALESCE(NULLIF(EXCLUDED.invoice_kind,   ''), shipments.invoice_kind),
			invoice_id            = COALESCE(NULLIF(EXCLUDED.invoice_id,     ''), shipments.invoice_id),
			status                = EXCLUDED.status,
			status_raw_code       = EXCLUDED.status_raw_code,
			status_raw_name       = EXCLUDED.status_raw_name,
			provider_meta         = EXCLUDED.provider_meta,
			updated_at            = now()
		RETURNING ` + shipmentColumns + `
	`
	row := r.pool.QueryRow(ctx, q,
		pgtype.UUID{Bytes: orderUID, Valid: true},
		pgtype.UUID{Bytes: storeUID, Valid: true},
		p.Provider,
		p.ProviderOrderID,
		nullableText(p.ProviderOrderNumber),
		nullableText(p.TrackingCode),
		nullableText(p.InvoiceKey),
		nullableText(p.InvoiceKind),
		nullableText(p.InvoiceID),
		nonEmptyOr(p.Status, "pending"),
		nullableInt4(p.StatusRawCode),
		nullableText(p.StatusRawName),
		metaRaw,
	)
	return scanShipmentRow(row)
}

// GetShipmentByOrderID returns the (at most one) shipment attached to an order.
// Returns nil, nil when no shipment exists yet.
func (r *Repository) GetShipmentByOrderID(ctx context.Context, orderID string) (*ShipmentRow, error) {
	uid, err := uuid.Parse(orderID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid order id")
	}
	const q = `SELECT ` + shipmentColumns + ` FROM shipments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`
	row := r.pool.QueryRow(ctx, q, pgtype.UUID{Bytes: uid, Valid: true})
	sh, err := scanShipmentRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return sh, nil
}

// getShipmentByIDForStore resolves a shipment by its LiveCart id, scoped to a
// store so admin endpoints cannot accidentally operate on another tenant's
// shipment. Returns httpx.ErrNotFound when absent.
func (r *Repository) getShipmentByIDForStore(ctx context.Context, id, storeID string) (*ShipmentRow, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid shipment id")
	}
	storeUID, err := uuid.Parse(storeID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid store id")
	}
	const q = `SELECT ` + shipmentColumns + ` FROM shipments WHERE id = $1 AND store_id = $2`
	row := r.pool.QueryRow(ctx, q, pgtype.UUID{Bytes: uid, Valid: true}, pgtype.UUID{Bytes: storeUID, Valid: true})
	sh, err := scanShipmentRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("shipment not found")
		}
		return nil, err
	}
	return sh, nil
}

// getShipmentByTrackingCode is a fallback lookup used when the caller tracked
// a shipment by tracking_code and we need to persist events against the local
// row. Returns nil, nil when absent.
func (r *Repository) getShipmentByTrackingCode(ctx context.Context, provider, trackingCode string) (*ShipmentRow, error) {
	if trackingCode == "" {
		return nil, nil
	}
	const q = `SELECT ` + shipmentColumns + ` FROM shipments WHERE provider = $1 AND tracking_code = $2`
	row := r.pool.QueryRow(ctx, q, provider, trackingCode)
	sh, err := scanShipmentRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return sh, nil
}

// GetShipmentByProviderOrderID looks up a shipment by provider + provider_order_id.
// Used when handlers need to find the row before updating it.
func (r *Repository) GetShipmentByProviderOrderID(ctx context.Context, provider, providerOrderID string) (*ShipmentRow, error) {
	const q = `SELECT ` + shipmentColumns + ` FROM shipments WHERE provider = $1 AND provider_order_id = $2`
	row := r.pool.QueryRow(ctx, q, provider, providerOrderID)
	sh, err := scanShipmentRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return sh, nil
}

// UpdateShipmentInvoice sets invoice_key + invoice_kind on an existing shipment.
// Use after a successful AttachInvoice / UploadInvoiceXML call.
func (r *Repository) UpdateShipmentInvoice(ctx context.Context, id, invoiceKey, invoiceKind string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return httpx.ErrBadRequest("invalid shipment id")
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE shipments
		SET invoice_key  = $2,
		    invoice_kind = COALESCE(NULLIF($3, ''), invoice_kind),
		    updated_at   = now()
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}, invoiceKey, invoiceKind)
	return err
}

// UpdateShipmentLabels persists the downloadable label URL + per-ticket
// `public_tracking` (best effort: if the tickets array has multiple, we keep
// the first since LiveCart creates one shipment per cart today).
func (r *Repository) UpdateShipmentLabels(ctx context.Context, id, labelURL, publicTrackingURL, trackingCode string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return httpx.ErrBadRequest("invalid shipment id")
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE shipments
		SET label_url           = COALESCE(NULLIF($2, ''), label_url),
		    public_tracking_url = COALESCE(NULLIF($3, ''), public_tracking_url),
		    tracking_code       = COALESCE(NULLIF($4, ''), tracking_code),
		    updated_at          = now()
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}, labelURL, publicTrackingURL, trackingCode)
	return err
}

// UpdateShipmentStatus updates the normalized status plus its raw mirror.
// The raw fields are the last event's code/name, for admin-UI debugging.
func (r *Repository) UpdateShipmentStatus(ctx context.Context, id, status string, rawCode int, rawName, trackingCode string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return httpx.ErrBadRequest("invalid shipment id")
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE shipments
		SET status          = $2,
		    status_raw_code = $3,
		    status_raw_name = COALESCE(NULLIF($4, ''), status_raw_name),
		    tracking_code   = COALESCE(NULLIF($5, ''), tracking_code),
		    updated_at      = now()
		WHERE id = $1
	`, pgtype.UUID{Bytes: uid, Valid: true}, status, nullableInt4(rawCode), rawName, trackingCode)
	return err
}

// TrackingEventInput is one row to be appended to shipment_tracking_events.
type TrackingEventInput struct {
	Status      string
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
	Source      string // 'poll' | 'webhook'
}

// InsertTrackingEvents bulk-inserts events, ignoring duplicates on the
// (shipment_id, event_at, raw_code) unique constraint. Silently skips events
// with a zero EventAt (provider returned unparsable date).
func (r *Repository) InsertTrackingEvents(ctx context.Context, shipmentID string, events []TrackingEventInput) error {
	if len(events) == 0 {
		return nil
	}
	uid, err := uuid.Parse(shipmentID)
	if err != nil {
		return httpx.ErrBadRequest("invalid shipment id")
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		if e.EventAt.IsZero() {
			continue
		}
		src := e.Source
		if src == "" {
			src = "poll"
		}
		batch.Queue(`
			INSERT INTO shipment_tracking_events (shipment_id, status, raw_code, raw_name, observation, event_at, source)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (shipment_id, event_at, raw_code) DO NOTHING
		`, pgtype.UUID{Bytes: uid, Valid: true}, e.Status, nullableInt4(e.RawCode), e.RawName, e.Observation, e.EventAt, src)
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range events {
		if _, err := results.Exec(); err != nil {
			// Allow the sentinel "no rows affected" case; any other error stops us.
			if err.Error() != "no rows in result set" {
				return fmt.Errorf("inserting tracking event: %w", err)
			}
		}
	}
	return nil
}

// ListTrackingEvents returns events for a shipment in ascending chronological
// order (older first) — the UI can reverse it if it wants latest-first.
func (r *Repository) ListTrackingEvents(ctx context.Context, shipmentID string) ([]ShipmentEventRow, error) {
	uid, err := uuid.Parse(shipmentID)
	if err != nil {
		return nil, httpx.ErrBadRequest("invalid shipment id")
	}
	const q = `
		SELECT id::text, shipment_id::text, status, COALESCE(raw_code, 0), COALESCE(raw_name, ''),
		       COALESCE(observation, ''), event_at, received_at, source
		FROM shipment_tracking_events
		WHERE shipment_id = $1
		ORDER BY event_at ASC, received_at ASC
	`
	rows, err := r.pool.Query(ctx, q, pgtype.UUID{Bytes: uid, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("listing tracking events: %w", err)
	}
	defer rows.Close()
	out := make([]ShipmentEventRow, 0)
	for rows.Next() {
		var e ShipmentEventRow
		if err := rows.Scan(&e.ID, &e.ShipmentID, &e.Status, &e.RawCode, &e.RawName,
			&e.Observation, &e.EventAt, &e.ReceivedAt, &e.Source); err != nil {
			return nil, fmt.Errorf("scanning tracking event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// =============================================================================
// HELPERS
// =============================================================================

const shipmentColumns = `
	id::text, order_id::text, store_id::text,
	provider, provider_order_id,
	COALESCE(provider_order_number, ''),
	COALESCE(tracking_code, ''),
	COALESCE(public_tracking_url, ''),
	COALESCE(invoice_key, ''),
	COALESCE(invoice_kind, ''),
	COALESCE(invoice_id, ''),
	COALESCE(label_url, ''),
	status,
	COALESCE(status_raw_code, 0),
	COALESCE(status_raw_name, ''),
	provider_meta,
	created_at, updated_at
`

func scanShipmentRow(row pgx.Row) (*ShipmentRow, error) {
	var s ShipmentRow
	if err := row.Scan(
		&s.ID, &s.OrderID, &s.StoreID,
		&s.Provider, &s.ProviderOrderID,
		&s.ProviderOrderNumber,
		&s.TrackingCode,
		&s.PublicTrackingURL,
		&s.InvoiceKey,
		&s.InvoiceKind,
		&s.InvoiceID,
		&s.LabelURL,
		&s.Status,
		&s.StatusRawCode,
		&s.StatusRawName,
		&s.ProviderMeta,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

func nullableText(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: v, Valid: true}
}

func nullableInt4(v int) pgtype.Int4 {
	if v == 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

func nonEmptyOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
