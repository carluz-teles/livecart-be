package erp

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// ERPRepository is the persistence port consumed by the ERP service. It is
// declared here (the consumer side, per Go idiom) and satisfied by
// internal/integration.Repository via a boot-wired adapter — erp MUST NOT import
// integration (cycle). It starts with the two methods the ERP order flow already
// needs and grows one method per slice as logic migrates (Bloco B2b+).
type ERPRepository interface {
	// GetActiveByProvider resolves the active ERP integration for a store.
	// Signature mirrors integration.Repository.GetActiveByProvider, but returns
	// the neutral erp.Integration instead of the integration-owned IntegrationRow
	// so this package stays free of the integration import.
	GetActiveByProvider(ctx context.Context, storeID, integrationType, provider string) (*Integration, error)

	// GetByProvider resolves ANY integration for a store (active or not) so the
	// legacy finalisation can disambiguate "merchant never set up Tiny" (info)
	// from "Tiny exists but is in error state" (warn). Returns the neutral
	// erp.Integration, mapped by the boot-wired adapter (same cycle guard as
	// GetActiveByProvider).
	GetByProvider(ctx context.Context, storeID, integrationType, provider string) (*Integration, error)

	// AcquireCartFinalisationLock takes a session-scoped Postgres advisory lock
	// keyed on the cart id. The lock lives on integration.Repository (which holds
	// the raw pgxpool — advisory locks are per-connection); erp only consumes it
	// through this port. acquired=false means another finalisation of the SAME
	// cart is running right now, so the loser bails. The caller MUST call
	// release() when acquired.
	AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error)

	// --- Stock reservation persistence (Bloco B2b) ---
	// Every method below is already implemented verbatim on integration.Repository;
	// it satisfies this port directly (the DTO types are aliases of the erp ones),
	// so only GetActiveByProvider needs an adapter for its return type.

	// GetCartERPOrderState reads the cart's order-as-reservation lifecycle state.
	GetCartERPOrderState(ctx context.Context, cartID string) (*CartERPOrderState, error)
	// ListActiveReservationsByCartAndProduct returns the active reservations for a
	// cart+product (in practice the unique index keeps it to one row).
	ListActiveReservationsByCartAndProduct(ctx context.Context, cartID, productID string) ([]StockReservationRow, error)
	// CreateStockReservation persists a new stock reservation row.
	CreateStockReservation(ctx context.Context, params CreateStockReservationParams) (*StockReservationRow, error)
	// AdjustActiveReservationQuantity bumps an active reservation's quantity by delta.
	AdjustActiveReservationQuantity(ctx context.Context, cartID, productID string, delta int, erpMovementID string) (*StockReservationRow, error)
	// UpsertActiveReservationQuantity soma unidades à reserva ativa, criando a
	// linha se não existir. Uma chamada, sem leitura prévia — o par
	// "listar / decidir entre criar e ajustar" é uma corrida.
	UpsertActiveReservationQuantity(ctx context.Context, p UpsertReservationParams) (*StockReservationRow, error)
	// DecrementActiveReservationQuantity baixa dec unidades da reserva ativa e diz
	// o que aconteceu. É o que substitui "ler, decidir, chamar o ERP, gravar":
	// aqui quem decide é o banco, e a decisão já vem aplicada.
	DecrementActiveReservationQuantity(ctx context.Context, cartID, productID string, dec int) (ReservationDecrement, error)
	// RestoreReservationQuantityByID desfaz o decremento acima. Compensação
	// obrigatória quando o ERP recusa depois do banco já ter baixado.
	RestoreReservationQuantityByID(ctx context.Context, reservationID string, inc int) error
	// ReverseReservationsByCartAndProduct marks a cart+product's reservations reversed.
	ReverseReservationsByCartAndProduct(ctx context.Context, cartID, productID string) error
	// DecrementProductStock atomically lowers local stock; ErrNoRows means the
	// decrement would go negative (insufficient stock).
	DecrementProductStock(ctx context.Context, productID string, quantity int) error
	// IncrementProductStock raises local stock (also satisfies stockReservationRepo).
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	// EmitStockReserved / EmitStockReleased publish the stock events best-effort
	// (also satisfy stockReservationRepo, so NewStockReservations can take repo).
	EmitStockReserved(ctx context.Context, p StockEventParams) error
	EmitStockReleased(ctx context.Context, p StockEventParams) error

	// --- Order-as-reservation lifecycle persistence (Bloco B2c, Design C) ---
	// Each method below is already implemented verbatim on integration.Repository
	// and promoted by the erpRepoAdapter (the DTO types are aliases of the erp
	// ones), so no new adapter code is needed.

	// TransitionCartERPOrderState is the state-machine CAS: it advances the cart
	// from `from` to `to` and returns false when the current state differs from
	// `from` (the single-flight primitive of conversion/mutation/confirm).
	TransitionCartERPOrderState(ctx context.Context, cartID, from, to string) (bool, error)
	// UpdateCartExternalOrderID persists the ERP order id on the cart (marker
	// adoption in confirm/sweep).
	UpdateCartExternalOrderID(ctx context.Context, cartID, externalOrderID string) error
	// SetCartERPStockLaunched flips the durable "order stock launched" marker.
	SetCartERPStockLaunched(ctx context.Context, cartID string, launched bool) error
	// MarkCartERPFinalisationDone stamps the terminal finalisation marker so the
	// existing telemetry/guards stay coherent (confirmed = done).
	MarkCartERPFinalisationDone(ctx context.Context, cartID string) error
	// ListNonWaitlistedCartItems returns the ERP-linked cart grid — input for the
	// order mutation cycle and the confirm payment total.
	ListNonWaitlistedCartItems(ctx context.Context, cartID string) ([]NonWaitlistedCartItem, error)
	// ListStuckERPOrderOps lists converting/mutating carts older than the
	// threshold — input for the reconciliation sweep.
	ListStuckERPOrderOps(ctx context.Context, olderThan time.Duration) ([]StuckERPOrderOp, error)
	// ListTinyIntegrationsWithStaleStockWebhook lists active Tiny integrations
	// with no stock webhook events in the window (URL likely removed by Tiny).
	ListTinyIntegrationsWithStaleStockWebhook(ctx context.Context, staleAfter time.Duration) ([]StaleStockWebhookIntegration, error)
	// StampIntegrationStockWebhookAlert dedupes the stale-webhook alert (24h).
	StampIntegrationStockWebhookAlert(ctx context.Context, integrationID string) error

	// --- Legacy post-payment finalisation persistence (Bloco B2c-2) ---
	// Each method below is already implemented verbatim on integration.Repository
	// (the DTO types are aliases of the erp ones), so it satisfies this port
	// directly — no new adapter code beyond GetByProvider.

	// GetCartERPFinalisationStatus reads the Order payment row's ERP finalisation
	// lifecycle (status/attempts/snapshot) plus the cart's reserve
	// external_order_id. Returns pgx.ErrNoRows when the Order isn't materialised.
	GetCartERPFinalisationStatus(ctx context.Context, cartID string) (*CartFinalisationStatus, error)
	// MarkCartERPFinalisationAttempt stamps the attempt (bumps count, COALESCEs
	// the gateway snapshot) BEFORE the ERP is touched, so an admin retry replays.
	MarkCartERPFinalisationAttempt(ctx context.Context, cartID string, paymentSnapshot []byte) error
	// ListActiveReservationsByCart returns the cart's still-active saída-manual
	// reservations — the input to the post-payment reversal.
	ListActiveReservationsByCart(ctx context.Context, cartID string) ([]StockReservationRow, error)
	// ReverseReservationByID marks a single reservation reversed, only after the
	// ERP confirmed the corresponding entrada E (per-row, resumable).
	ReverseReservationByID(ctx context.Context, reservationID string) error
	// ClaimReservationForReversal reivindica a reserva ANTES de falar com o ERP
	// e devolve true só para quem ganhou a corrida. É o que torna o estorno
	// duplo impossível quando a asynq retenta.
	ClaimReservationForReversal(ctx context.Context, reservationID string) (bool, error)
	// RestoreReservationToActive desfaz a reivindicação quando o ERP recusa o
	// estorno, para a próxima tentativa voltar a enxergar a reserva.
	RestoreReservationToActive(ctx context.Context, reservationID string) error
	// ReverseReservationsByCart mass-marks a cart's active reservations reversed
	// (the legacy expiry path, which marks locally regardless of the ERP result).
	ReverseReservationsByCart(ctx context.Context, cartID string) error

	// --- Cart NFe sync persistence (Bloco B2d) ---
	// UpsertCartERPInvoice, FindCartByExternalOrderID and UpdateShipmentInvoice are
	// already implemented verbatim on integration.Repository (DTO types aliased),
	// so they satisfy this port directly. GetShipmentByOrderID is bridged to the
	// slim erp.ShipmentInvoiceRef by the erpRepoAdapter (same cycle guard as
	// GetActiveByProvider), and GetCartInvoiceAnchor is the new enxuto cart reader.

	// GetCartInvoiceAnchor returns just the two cart fields the NFe sync needs —
	// the owning store and the ERP order id — without dragging the full CartRow
	// (which stays integration-owned to avoid the import cycle).
	GetCartInvoiceAnchor(ctx context.Context, cartID string) (storeID, externalOrderID string, err error)
	// UpsertCartERPInvoice persists the NFe onto the Order's payment row (resolved
	// from cart_id). Idempotent; returns 0 rows when no Order exists yet (benign
	// skip — the NF is always post-confirmation).
	UpsertCartERPInvoice(ctx context.Context, params UpsertCartERPInvoiceParams) (int64, error)
	// FindCartByExternalOrderID locates the cart linked to an ERP pedido id for a
	// store — the bridge the Tiny nota_fiscal webhook needs (Tiny only sends the
	// pedido id).
	FindCartByExternalOrderID(ctx context.Context, externalOrderID, storeID string) (string, error)
	// GetShipmentByOrderID returns the slim invoice ref of the (at most one)
	// shipment attached to the cart, or nil when none exists yet.
	GetShipmentByOrderID(ctx context.Context, cartID string) (*ShipmentInvoiceRef, error)
	// UpdateShipmentInvoice mirrors the NFe chave/kind onto an existing shipment.
	UpdateShipmentInvoice(ctx context.Context, shipmentID, invoiceKey, invoiceKind string) error
}

// Service handles ERP-domain business logic. B2a laid the foundation (struct +
// ports); B2b moves in the cart→ERP stock reservation flow (ReserveStockInERP /
// AdjustStockReservationDelta). More order/finalisation logic follows in B2c+.
type Service struct {
	repo   ERPRepository
	collab StockCollaborators
	stock  *StockReservations
	logger *zap.Logger
}

// NewService creates a new ERP service. collab supplies the integration-Service
// helpers the migrated stock flow still calls back into (provider resolution,
// product linking, converted-cart mutation); it shrinks as more logic migrates.
func NewService(repo ERPRepository, collab StockCollaborators, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		collab: collab,
		stock:  NewStockReservations(repo, logger),
		logger: logger,
	}
}
