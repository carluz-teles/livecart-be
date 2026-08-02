package erp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// =============================================================================
// CART NFe SYNC (ERP → carts.erp_invoice_*) — moved from internal/integration
// (Bloco B2d). integration keeps a one-liner delegation; the call sites migrate
// in B2e.
// =============================================================================

// CartInvoiceState is the normalised view of a cart's NFe used by the order
// detail handler and the manual-sync response. It mirrors providers.ERPInvoice
// without exposing provider-specific quirks. Canonical home is this package
// (Bloco B2d); internal/integration aliases it so the delegation signature and
// its call sites keep compiling unchanged.
type CartInvoiceState struct {
	InvoiceID  string
	InvoiceKey string
	Status     string // pending | authorized | cancelled | rejected | "" (none)
	EmittedAt  *time.Time
}

// UpsertCartERPInvoiceParams carries the NFe fields written to the Order's
// payment row. Canonical home is this package (Bloco B2d — the invoice sync that
// writes it lives here); internal/integration aliases it so the Repository (which
// owns the SQL) keeps compiling unchanged.
type UpsertCartERPInvoiceParams struct {
	CartID        string
	InvoiceID     string
	InvoiceKey    string
	InvoiceStatus string
	EmittedAt     *time.Time
}

// ShipmentInvoiceRef is the slim view of an existing shipment the NFe sync needs
// to mirror the chave de acesso onto: only the id and the currently stored
// invoice key. It deliberately does NOT drag the full integration.ShipmentRow
// (shipment/logistics domain) into erp — the port stays enxuto, the same way the
// cart is reached via GetCartInvoiceAnchor instead of the whole CartRow.
type ShipmentInvoiceRef struct {
	ID         string
	InvoiceKey string
}

// SyncCartInvoiceFromERP pulls the NFe state for a paid cart from the active
// ERP integration (today: Tiny) and persists it on the Order's payment row.
//
// invoiceID is optional: when the caller already knows the ERP-side notafiscal
// id (e.g. from a webhook payload) we fetch by id; otherwise we ask the ERP
// for whatever NFe is attached to the order. Returns nil error when the
// merchant hasn't emitted the NFe yet — that's the "Aguardando NFe" branch
// on the frontend, surfaced via the absence of erp_invoice_* fields.
//
// Idempotent: re-running the same sync produces the same row state. Callers
// can hit it from the Tiny webhook handler, the manual "Verificar NFe" button,
// or a future poller without coordination.
//
// Implements order.CartInvoiceSyncer.
func (s *Service) SyncCartInvoiceFromERP(ctx context.Context, storeID, cartID, invoiceID string) error {
	_, err := s.fetchAndPersistCartInvoice(ctx, storeID, cartID, invoiceID)
	return err
}

// fetchAndPersistCartInvoice is the workhorse used by both the public
// SyncCartInvoiceFromERP entry point and SyncCartInvoiceByExternalOrder. The
// state it returns is consumed by the webhook path for logging, but not
// surfaced past the ERP package boundary.
func (s *Service) fetchAndPersistCartInvoice(ctx context.Context, storeID, cartID, invoiceID string) (*CartInvoiceState, error) {
	// Only the store and the ERP order id anchor the fetch — the full CartRow
	// stays integration-owned to keep this package free of the integration import
	// (cycle guard). GetCartInvoiceAnchor returns exactly those two fields.
	cartStoreID, externalOrderID, err := s.repo.GetCartInvoiceAnchor(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart for invoice sync: %w", err)
	}
	if cartStoreID != storeID {
		return nil, httpx.ErrNotFound("cart not found")
	}
	// Without an ERP order id we have no anchor on the Tiny side and nothing
	// to fetch. Returning nil lets the caller render "Aguardando criação no
	// ERP" without surfacing a generic error.
	if externalOrderID == "" && invoiceID == "" {
		return nil, nil
	}

	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil, httpx.DomainError(422, httpx.CodeErpNotActive, "ERP integration not active for store")
	}

	provider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		return nil, fmt.Errorf("creating ERP provider: %w", err)
	}
	invoiceProvider, ok := provider.(providers.ERPInvoiceProvider)
	if !ok {
		return nil, httpx.DomainError(422, httpx.CodeErpNoInvoiceSupport, "ERP provider does not expose invoice operations")
	}

	var (
		invoice  *providers.ERPInvoice
		fetchErr error
	)
	if invoiceID != "" {
		invoice, fetchErr = invoiceProvider.GetInvoiceByID(ctx, invoiceID)
	} else {
		invoice, fetchErr = invoiceProvider.GetInvoiceByOrder(ctx, externalOrderID)
	}
	if errors.Is(fetchErr, providers.ErrInvoiceNotFound) {
		// Tiny knows the order but no NFe is attached yet — merchant still
		// has to emit it in the ERP. Persist nothing and let the frontend
		// surface "Aguardando NFe".
		return nil, nil
	}
	if fetchErr != nil {
		s.collab.HandleProviderError(ctx, integration.ID, "sync_cart_invoice", fetchErr)
		return nil, httpx.InfrastructureError(fetchErr, "sync_cart_invoice")
	}

	// Fatia 11b: the NFe is written authoritatively to order_payments (resolved
	// from cart_id). 0 rows = no Order for this cart yet — a benign skip (NF is
	// always post-confirmation, so the Order should already exist; we log rather
	// than error so a stray webhook never dead-letters).
	rows, err := s.repo.UpsertCartERPInvoice(ctx, UpsertCartERPInvoiceParams{
		CartID:        cartID,
		InvoiceID:     invoice.InvoiceID,
		InvoiceKey:    invoice.AccessKey,
		InvoiceStatus: string(invoice.Status),
		EmittedAt:     invoiceTimePtr(invoice.IssuedAt),
	})
	if err != nil {
		return nil, fmt.Errorf("persisting order NFe: %w", err)
	}
	if rows == 0 {
		logger.From(ctx, s.logger).Warn("nota fiscal received for cart without a materialised order, skipping invoice persist",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", externalOrderID),
			zap.String("invoice_id", invoice.InvoiceID),
		)
	}

	// Mirror the chave on any existing shipment so the carrier provider can
	// pick it up the next time the merchant clicks "Anexar NFe" / generates a
	// label. We don't auto-call AttachInvoice on the carrier here because the
	// merchant-driven flow is explicit.
	if invoice.AccessKey != "" {
		if sh, _ := s.repo.GetShipmentByOrderID(ctx, cartID); sh != nil && sh.InvoiceKey == "" {
			if err := s.repo.UpdateShipmentInvoice(ctx, sh.ID, invoice.AccessKey, "nfe"); err != nil {
				logger.From(ctx, s.logger).Warn("failed to mirror NFe key onto existing shipment",
					zap.String("cart_id", cartID),
					zap.String("shipment_id", sh.ID),
					zap.Error(err),
				)
			}
		}
	}

	return &CartInvoiceState{
		InvoiceID:  invoice.InvoiceID,
		InvoiceKey: invoice.AccessKey,
		Status:     string(invoice.Status),
		EmittedAt:  invoiceTimePtr(invoice.IssuedAt),
	}, nil
}

// SyncCartInvoiceByExternalOrder is the webhook entry point: Tiny only sends
// the pedido id (and sometimes the notafiscal id) in nota_fiscal events, so
// we resolve the cart by external_order_id first, then delegate to the
// regular sync.
func (s *Service) SyncCartInvoiceByExternalOrder(ctx context.Context, storeID, externalOrderID, invoiceID string) (*CartInvoiceState, error) {
	cartID, err := s.repo.FindCartByExternalOrderID(ctx, externalOrderID, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Debug("nota_fiscal webhook for unknown pedido — skipping",
			zap.String("store_id", storeID),
			zap.String("external_order_id", externalOrderID),
			zap.Error(err),
		)
		return nil, nil
	}
	return s.fetchAndPersistCartInvoice(ctx, storeID, cartID, invoiceID)
}

func invoiceTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
