package inventory

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/lib/logger"
)

// InventoryRepository owns atomic queue mutations. Integration provides the
// persistence adapter without introducing an inventory -> integration cycle.
type InventoryRepository interface {
	ListActiveByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error)
	PromoteNextWaitlistEntry(ctx context.Context, storeID, productID string) (*WaitlistPromotion, error)
	CancelWaitingRequest(ctx context.Context, id, cartID string) (bool, error)
	AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error)
	ExpireCartAndReleaseStock(ctx context.Context, cartID, storeID string) (ExpireCartResult, error)
	GetProductIDByExternalID(ctx context.Context, storeID, externalSource, externalID string) (string, error)
	HasInFlightFinalisationForProduct(ctx context.Context, productID string) (bool, error)
}

// WaitlistCollaborators is the slice of integration-Service behaviour the
// migrated waitlist flows still call back into — mirrors erp.StockCollaborators.
// All three of the ERP/notification/scheduler helpers are reached through the
// integration.Service that already owns those wirings, so inventory does not
// import integration and — crucially — reads the notification/scheduler
// dependencies LAZILY at call time (they are wired via setters AFTER the eager
// inventory build, so capturing them concretely would capture nil). The interface
// shrinks as more logic migrates.
type WaitlistCollaborators interface {
	// AdjustStockReservationDelta applies a (positive/negative) quantity delta to a
	// cart+product reservation, mutating both local stock and the ERP reservation.
	AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op erp.StockOp) (string, error)
	// ReserveStockInERP creates the ERP saída-manual reservation paired to the units
	// just released to a promoted cart. No-op when the store has no ERP integration.
	// Reached through integration.Service (touches provider/contact/order of the erp
	// domain via the integration StockCollaborators — injecting *erp.StockReservations
	// alone would not cover it).
	ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error
	// ScheduleExpiry (re-)arms the cart.expire ETA task for a cart's current
	// expires_at. Best-effort; no-op when the scheduler is not wired. Read lazily by
	// integration.Service so the setter-wired scheduler is picked up at call time.
	ScheduleExpiry(ctx context.Context, cartID string) error
	// NotifyWaitlistPromoted sends the "produto liberou" DM to a promoted buyer.
	// No-op when the notification service is not wired; read lazily by
	// integration.Service (the impl, sendWaitlistNotifiedDM, stays integration-owned).
	NotifyWaitlistPromoted(ctx context.Context, in WaitlistNotifiedInput)
}

// Service owns the Inventory domain's waitlist/fila business logic. B3a laid the
// foundation (struct + ports) and moved the two low-blast-radius flows
// (ListActiveWaitlistByCart, CancelWaitlistItem); B3b brings in the concurrent
// core (ProcessWaitlistForProduct / ExpireCart / notified-expiry sweep / stock-
// webhook backstop), closing the domain. Mirrors erp.Service: repo (port) + collab
// (integration callbacks) + stock (local-stock releases) + live (direct injection
// for the promotion cart-item fallback) + logger.
type Service struct {
	repo   InventoryRepository
	collab WaitlistCollaborators
	stock  *erp.StockReservations
	live   *live.Service
	logger *zap.Logger
}

// NewService creates a new Inventory service. stock is the same
// *erp.StockReservations the integration.Service holds (the release manager for
// LOCAL product stock); live is the same *live.Service (the promotion fallback
// re-creates a deleted cart item through AddToCart); collab supplies the
// integration-Service helpers the migrated flows still call back into (lazily, so
// setter-wired notification/scheduler are picked up at call time). All shrink as
// more logic migrates.
func NewService(repo InventoryRepository, collab WaitlistCollaborators, stock *erp.StockReservations, live *live.Service, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		collab: collab,
		stock:  stock,
		live:   live,
		logger: logger,
	}
}

// ListActiveWaitlistByCart é a leitura usada pelo checkout para popular a
// seção "produtos em fila". Retorna apenas waiting/notified.
func (s *Service) ListActiveWaitlistByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error) {
	return s.repo.ListActiveByCart(ctx, cartID)
}

// CancelWaitlistItem removes only an unfulfilled request, atomically with its
// cart quantities. Promoted products use the ordinary cart item editing flow.
func (s *Service) CancelWaitlistItem(ctx context.Context, waitlistItemID, cartID string) (bool, error) {
	return s.repo.CancelWaitingRequest(ctx, waitlistItemID, cartID)
}

// =============================================================================
// LAZY EXPIRATION & WAITLIST PROCESSING (Bloco B3b)
// =============================================================================

// cartExpiryTerminal reports whether a cart is already in a state where expiry
// (or promotion) must not run (paid/refunded or already expired/cancelled). A
// copy of the same predicate lives in integration (over the aliased snapshot),
// consumed by the ScheduleExpiry/RunScheduledExpiry bridge that stays there; the
// two coexist.
func cartExpiryTerminal(s *CartExpirySnapshot) bool {
	return s.Protected || s.Status == "expired" || s.Status == "cancelled" ||
		s.PaymentStatus == "paid" || s.PaymentStatus == "refunded"
}

// ExpireCart expira UM carrinho, com segurança contra a corrida do pagamento.
// Ordem (crítica):
//  1. Advisory lock por cart — serializa contra o confirm/finalize do webhook
//     de pagamento (ConfirmERPOrderPayment/finalizeCartERPOrder tomam o mesmo
//     lock). !acquired = pagamento finalizando; sai.
//  2. Flip 'expired' + devolução de estoque local de TODOS os itens numa única
//     transação (ExpireCartAndReleaseStock). O flip é guard-first: se o cart
//     foi pago/expirado no intervalo, 0 rows → NÃO elegível → aborta sem tocar
//     ERP (a ação irreversível de cancelar pedido só roda com o cart já 'expired').
//  3. ERP (best-effort, fora do tx): agora roda no reactor cart.expired.
//  4. Promove a waitlist de cada produto liberado (pós-commit, fire-and-forget).
func (s *Service) ExpireCart(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return fmt.Errorf("acquiring expiry lock for cart %s: %w", cartID, err)
	}
	if !acquired {
		return fmt.Errorf("expiry deferred for cart %s: %w", cartID, erp.ErrCartBusy)
	}
	defer release()

	res, err := s.repo.ExpireCartAndReleaseStock(ctx, cartID, storeID)
	if err != nil {
		return fmt.Errorf("expiring cart %s: %w", cartID, err)
	}
	if !res.Eligible {
		// Pago ou já expirado/cancelado entre a seleção e o flip. Nada a fazer.
		logger.From(ctx, s.logger).Info("expiry: cart no longer eligible (paid/terminal in gap)", zap.String("cart_id", cartID))
		return nil
	}

	// ERP reversal (Tiny cancel/estorno) now runs in the cart.expired reactor
	// (ReactCartExpiredERP), decoupled from this eligibility flip so it gets its
	// own asynq retry + DLQ. The cart.expired fact was emitted transactionally
	// inside ExpireCartAndReleaseStock above.

	logger.From(ctx, s.logger).Info("expired cart processed",
		zap.String("cart_id", cartID),
		zap.Int("items_released", len(res.FreedProductIDs)),
	)

	// [IGTRACE] TODO remover — investigação da expiração em lote.
	//
	// Quando o EVENTO fecha, TODOS os carrinhos abertos recebem o MESMO
	// expires_at (um UPDATE, um now()), então eles vencem no mesmo instante e a
	// promoção abaixo tenta promover carrinhos que estão expirando junto. O
	// campo que faltava era saber QUAIS produtos foram liberados e para quem a
	// promoção foi tentada.
	logger.From(ctx, s.logger).Info(TracePrefix+"expiry: released stock, will try to promote",
		zap.String("cart_id", cartID),
		zap.String("event_id", res.EventID),
		zap.Strings("freed_product_ids", res.FreedProductIDs),
	)

	// Promove o próximo da fila para cada produto liberado. Idempotente.
	for _, productID := range res.FreedProductIDs {
		s.ProcessWaitlistForProduct(ctx, res.EventID, productID, storeID)
	}
	return nil
}

// ErrWaitlistPromotionDeferred means the oldest buyer is temporarily locked.
// A stock event retries it instead of giving their units to a later request.
var ErrWaitlistPromotionDeferred = errors.New("waitlist head temporarily unavailable")

// ProcessWaitlistForProduct drains stock in store-wide FIFO order. eventID is
// retained for existing callers; event membership never grants priority.
func (s *Service) ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string) {
	if err := s.ProcessWaitlistProduct(ctx, storeID, productID); err != nil {
		logger.From(ctx, s.logger).Warn("waitlist allocation deferred", zap.String("store_id", storeID),
			zap.String("product_id", productID), zap.Error(err))
	}
}

// ProcessWaitlistProduct exposes errors to durable event consumers for retry.
func (s *Service) ProcessWaitlistProduct(ctx context.Context, storeID, productID string) error {
	inFlight, err := s.repo.HasInFlightFinalisationForProduct(ctx, productID)
	if err != nil {
		return err
	}
	if inFlight {
		return ErrWaitlistPromotionDeferred
	}
	for {
		next, err := s.repo.PromoteNextWaitlistEntry(ctx, storeID, productID)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		if next.Quantity == 0 {
			continue
		}
		// Pending ERP state and the allocation fact were persisted atomically.
		// A process interruption is recovered by the pending-grid reconciler.
		if err = s.collab.ReserveStockInERP(ctx, storeID, next.CartID, next.EventID, productID,
			next.Quantity, next.UnitPrice, next.PlatformHandle); err != nil {
			logger.From(ctx, s.logger).Warn("waitlist item reserved locally; ERP synchronization pending",
				zap.String("cart_id", next.CartID), zap.String("waitlist_item_id", next.WaitlistItemID), zap.Error(err))
		}
		logger.From(ctx, s.logger).Info("waitlist promoted",
			zap.String("waitlist_item_id", next.WaitlistItemID), zap.String("cart_id", next.CartID),
			zap.String("product_id", productID), zap.Int("promoted", next.Quantity),
			zap.Int("still_waiting", next.Remaining), zap.Int64("unit_price", next.UnitPrice))
	}
}

// Legacy jobs are harmless after upgrade. Only the cart itself may expire a
// promoted product; its former notification timestamp grants no separate TTL.
func (s *Service) ExpireNotifiedWaitlistItem(context.Context, WaitlistItemRow) error { return nil }
func (s *Service) ExpireNotifiedWaitlistSweep(context.Context) (int, error)          { return 0, nil }

// ProcessWaitlistAfterStockWebhook resolves the imported product and delegates
// to the global allocator. Retry errors stay visible to the durable consumer.
func (s *Service) ProcessWaitlistAfterStockWebhook(ctx context.Context, storeID, externalSource, externalProductID string) error {
	productID, err := s.repo.GetProductIDByExternalID(ctx, storeID, externalSource, externalProductID)
	if err != nil {
		return fmt.Errorf("resolving product by external id: %w", err)
	}
	if productID == "" {
		// Produto não cadastrado no LiveCart — não temos fila para ele.
		return nil
	}

	return s.ProcessWaitlistProduct(ctx, storeID, productID)
}

// TracePrefix marca as linhas da investigação de expiração/fila.
// TODO REMOVER junto com o restante do [IGTRACE].
const TracePrefix = "[IGTRACE] "
