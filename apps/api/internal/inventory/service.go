package inventory

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/live"
	"livecart/apps/api/lib/logger"
)

// InventoryRepository is the persistence port consumed by the Inventory service.
// It is declared here (the consumer side, per Go idiom) and satisfied by
// internal/integration.Repository via a boot-wired adapter — inventory MUST NOT
// import integration (cycle). It starts with just the methods the two B3a flows
// need and grows one method per slice as the waitlist core migrates (Bloco B3b).
//
// It is DISTINCT from inventory.Repository (Fatia A2): that one is the thin sqlc
// wrapper the OnCartPaid reactor delegates to (MarkWaitlistFulfilledByCart). This
// port is satisfied by the integration.Repository instead, because the migrated
// flows still read the same waitlist/cart rows that repository owns (and, in
// B3b, the pgxpool advisory lock that must NOT move onto the inventory sqlc
// wrapper). The two coexist.
type InventoryRepository interface {
	// ListActiveByCart returns the cart's active (waiting/notified) waitlist rows
	// — the read the checkout uses to populate the "produtos em fila" section.
	ListActiveByCart(ctx context.Context, cartID string) ([]ListActiveByCartRow, error)
	// GetWaitlistItemForCart fetches a single waitlist row scoped to a cart (nil
	// when absent), so the cancel flow can tell a 'notified' row (needs stock
	// release + queue advancement) from a plain 'waiting' one before cancelling.
	GetWaitlistItemForCart(ctx context.Context, id, cartID string) (*WaitlistItemRow, error)
	// CancelWaitlistItem marks the row cancelled. Ownership is enforced by cart_id
	// in the query's WHERE.
	CancelWaitlistItem(ctx context.Context, id, cartID string) error
	// DecrementCartItem lowers the (cart, product) quantity by delta, deleting the
	// row when it reaches zero. Returns the resulting quantity.
	DecrementCartItem(ctx context.Context, cartID, productID string, delta int) (int, error)
	// GetCartByID returns the slim cart ref (store + handle), or nil when the cart
	// is gone (the release-fallback branch of the cancel flow).
	GetCartByID(ctx context.Context, cartID string) (*CartRef, error)

	// --- Concurrent waitlist core (Bloco B3b) ---
	// Every method below is already implemented verbatim on integration.Repository;
	// it satisfies this port directly (the DTO types are aliases of the inventory
	// ones), so only GetProductByID needs an adapter for its enxuto return type.

	// GetWaitlistNotifiedTTL reads the event's waitlist_notified_ttl_minutes — the
	// "gordura" window a promoted buyer gets to finalise.
	GetWaitlistNotifiedTTL(ctx context.Context, eventID string) (time.Duration, error)
	// ClaimNextWaitlistItem atomically reserves the next queued entry for an
	// event+product (FOR UPDATE SKIP LOCKED) and stamps its notify window, so
	// concurrent callers claim DISTINCT buyers. Returns nil when the queue is empty.
	ClaimNextWaitlistItem(ctx context.Context, eventID, productID string, expiresAt time.Time) (*WaitlistItemRow, error)
	// DecrementProductStockUpTo takes UP TO want units atomically (partial
	// promotion), returning how many it actually took (0 = no stock — the gate).
	DecrementProductStockUpTo(ctx context.Context, productID string, want int) (int, error)
	// RevertWaitlistToWaiting rolls a claimed row back to 'waiting' (every early
	// return in the promotion reverts the claim so the buyer keeps their place).
	RevertWaitlistToWaiting(ctx context.Context, id string) error
	// IncrementProductStock returns units to local stock (the rollback partner of
	// DecrementProductStockUpTo on every promotion early-return).
	IncrementProductStock(ctx context.Context, productID string, quantity int) error
	// AcquireCartFinalisationLock takes the session-scoped advisory lock keyed on
	// the cart id (raw pgxpool, integration-owned — NOT the inventory sqlc wrapper).
	// acquired=false means a payment finalisation of the SAME cart holds it. The
	// caller MUST call release() when acquired.
	AcquireCartFinalisationLock(ctx context.Context, cartID string) (release func(), acquired bool, err error)
	// GetCartExpirySnapshot loads the expiry-relevant fields for a cart (nil when
	// gone) — the terminal-cart guard under the advisory lock.
	GetCartExpirySnapshot(ctx context.Context, cartID string) (*CartExpirySnapshot, error)
	// GetProductByID returns the enxuto product ref (price/name/keyword) for the
	// promoted cart item + DM, or nil when the product is gone.
	GetProductByID(ctx context.Context, storeID, productID string) (*ProductRef, error)
	// DecrementCartItemWaitlistedQuantity flips `delta` of a cart item's waitlisted
	// units to available in place. found=false means the row was gone (fallback to
	// AddToCart).
	DecrementCartItemWaitlistedQuantity(ctx context.Context, cartID, productID string, delta int) (bool, error)
	// RequeueWaitlistItemPartial re-queues the remainder of a partial promotion so
	// the buyer keeps waiting for the rest.
	RequeueWaitlistItemPartial(ctx context.Context, id string, remainingQty int) error
	// GetCartTokenByID reads the cart's public token for the checkout link in the DM.
	GetCartTokenByID(ctx context.Context, cartID string) (string, error)
	// ExtendCartExpiration pushes the cart's expires_at out to `until` (GREATEST —
	// never shrinks), giving the promoted buyer the notify TTL to finalise.
	ExtendCartExpiration(ctx context.Context, cartID string, until time.Time) error
	// EmitWaitlistNotified publishes waitlist.notified best-effort at the definitive
	// success point of the promotion.
	EmitWaitlistNotified(ctx context.Context, p EmitWaitlistNotifiedParams) error
	// ExpireCartAndReleaseStock does the guard-first 'expired' flip + local stock
	// release of every item in one transaction (the atomic anti-double-release core).
	ExpireCartAndReleaseStock(ctx context.Context, cartID, storeID string) (ExpireCartResult, error)
	// UpdateWaitlistItemStatus sets a row's status (+ optional notified/fulfilled/
	// expired timestamps) — the idempotency gate of the notified-expiry sweep.
	UpdateWaitlistItemStatus(ctx context.Context, id, status string, notifiedAt, fulfilledAt, expiresAt *time.Time) error
	// EmitWaitlistExpired publishes waitlist.expired best-effort after the
	// idempotency-gate flip to 'expired'.
	EmitWaitlistExpired(ctx context.Context, waitlistItemID, eventID, productID, cartID string) error
	// ListExpiredNotifiedWaitlist returns the 'notified' rows whose TTL lapsed —
	// input to the sweep.
	ListExpiredNotifiedWaitlist(ctx context.Context) ([]WaitlistItemRow, error)
	// GetProductIDByExternalID resolves a local product from an ERP external id
	// (empty when not imported) — the entry point of the stock-webhook backstop.
	GetProductIDByExternalID(ctx context.Context, storeID, externalSource, externalID string) (string, error)
	// HasInFlightFinalisationForProduct reports whether a paid cart holding the
	// product is mid ERP finalisation — the backstop defers promotion when true
	// (avoids notifying against transiently-inflated stock).
	HasInFlightFinalisationForProduct(ctx context.Context, productID string) (bool, error)
	// ListEventsWithWaitingByProduct lists the active events with waiting entries
	// for a product — the backstop fans promotion out across them.
	ListEventsWithWaitingByProduct(ctx context.Context, productID string) ([]string, error)
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

// CancelWaitlistItem é a operação pública "sair da fila": cliente desiste
// de uma entry. Quando estava 'notified' (já promovido para o cart), o
// stock volta para o próximo da fila — mesmo fluxo do worker de expiração.
// Quando estava 'waiting', apenas marca como 'cancelled'.
//
// Ownership é validada pela query (cart_id no WHERE de CancelWaitlistItem).
// Retorna (true) se algo foi alterado, (false) se a row não existia ou já
// estava em estado terminal.
func (s *Service) CancelWaitlistItem(ctx context.Context, waitlistItemID, cartID string) (bool, error) {
	// Carrega antes do UPDATE para saber se precisamos disparar a
	// devolução de estoque (status='notified').
	item, err := s.repo.GetWaitlistItemForCart(ctx, waitlistItemID, cartID)
	if err != nil {
		return false, fmt.Errorf("loading waitlist item: %w", err)
	}
	if item == nil {
		return false, nil
	}
	if item.Status != "waiting" && item.Status != "notified" {
		// Já fulfilled / expired / cancelled — no-op.
		return false, nil
	}
	if err := s.repo.CancelWaitlistItem(ctx, waitlistItemID, cartID); err != nil {
		return false, fmt.Errorf("cancelling waitlist item: %w", err)
	}

	if item.Status == "notified" {
		// O cliente já tinha o item reservado no cart + ERP. Devolve
		// tudo via mesmo fluxo do worker de expiração.
		if _, err := s.repo.DecrementCartItem(ctx, cartID, item.ProductID, item.Quantity); err != nil {
			logger.From(ctx, s.logger).Warn("failed to decrement cart item on waitlist cancel",
				zap.String("waitlist_item_id", waitlistItemID),
				zap.Error(err),
			)
		}
		cart, _ := s.repo.GetCartByID(ctx, cartID)
		if cart != nil {
			// AdjustStockReservationDelta also bumps products.stock for delta<0,
			// so no separate IncrementProductStock call is needed here.
			if _, err := s.collab.AdjustStockReservationDelta(ctx, cart.StoreID, cartID, item.EventID, item.ProductID, -item.Quantity, 0, cart.PlatformHandle, erp.StockOpWaitlistCancel); err != nil {
				logger.From(ctx, s.logger).Warn("failed to reverse reservation on waitlist cancel",
					zap.String("waitlist_item_id", waitlistItemID),
					zap.Error(err),
				)
			}
		} else {
			// Cart desapareceu: ainda precisamos devolver o estoque local que
			// o promote consumiu, para a próxima entry da fila ser promovível.
			if err := s.stock.Release(ctx, erp.ReleaseParams{Op: erp.StockOpWaitlistCancel, ProductID: item.ProductID, Quantity: item.Quantity, CartID: cartID, EventID: item.EventID}); err != nil {
				logger.From(ctx, s.logger).Warn("failed to increment local stock on waitlist cancel (cart missing)",
					zap.String("waitlist_item_id", waitlistItemID),
					zap.Error(err),
				)
			}
		}
		// Promove o próximo da fila — best-effort.
		if cart != nil {
			s.ProcessWaitlistForProduct(ctx, item.EventID, item.ProductID, cart.StoreID)
		}
	}
	return true, nil
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
	return s.Status == "expired" || s.Status == "cancelled" ||
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
func (s *Service) ExpireCart(ctx context.Context, cartID, storeID string) {
	ctx = logger.WithStore(ctx, storeID, "")
	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("expiry: failed to acquire cart lock", zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	if !acquired {
		// Webhook de pagamento está finalizando este mesmo cart. Ele decide.
		logger.From(ctx, s.logger).Info("expiry: skip, finalisation in progress", zap.String("cart_id", cartID))
		return
	}
	defer release()

	res, err := s.repo.ExpireCartAndReleaseStock(ctx, cartID, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Error("expiry: failed to expire cart", zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	if !res.Eligible {
		// Pago ou já expirado/cancelado entre a seleção e o flip. Nada a fazer.
		logger.From(ctx, s.logger).Info("expiry: cart no longer eligible (paid/terminal in gap)", zap.String("cart_id", cartID))
		return
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
}

// acquireCartFinalisationLockRetry tenta o try-lock de finalização do cart com
// algumas tentativas curtas. Um lock momentaneamente retido por uma expiração
// concorrente (que agora se abstém e o solta rápido, graças ao guard de
// ExpireCart) é liberado entre as tentativas, então o promotor quase sempre o
// adquire sem perder a vez do cliente. Respeita o cancelamento do ctx no backoff.
func (s *Service) acquireCartFinalisationLockRetry(ctx context.Context, cartID string) (release func(), acquired bool, err error) {
	const attempts = 3
	const backoff = 25 * time.Millisecond
	for i := 0; i < attempts; i++ {
		release, acquired, err = s.repo.AcquireCartFinalisationLock(ctx, cartID)
		if err != nil || acquired {
			return release, acquired, err
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, false, nil
}

// ProcessWaitlistForProduct promotes the next waiting customer to "notified"
// when stock is available. Called after any stock release (cart expired,
// cart paid, item removed, ERP webhook). Idempotent: if no one is waiting,
// or DecrementProductStock fails (race with another caller), it no-ops.
//
// Flow:
//  1. Decrement local stock (this is the gate — atomic, prevents double-promote)
//  2. Promote item to the next person's cart (WaitlistedQuantity=0)
//  3. Push the cart's expires_at by event.waitlist_notified_ttl_minutes (the
//     "gordura" the customer asked for)
//  4. Mark waitlist row as 'notified' with expires_at
//  5. Reserve stock in the ERP (Tiny saída)
//  6. Fire-and-forget DM via the notification service
//
// If anything in steps 2-3 fails we roll back the local stock decrement so
// nobody else loses the slot. Steps 4-6 are best-effort post-promotion.
func (s *Service) ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string) {
	// TTL primeiro: a reivindicação atômica já grava a janela de notificação
	// na row da fila, então precisamos de notifiedUntil antes do claim.
	ttl, ttlErr := s.repo.GetWaitlistNotifiedTTL(ctx, eventID)
	if ttlErr != nil {
		logger.From(ctx, s.logger).Warn("failed to read waitlist TTL, defaulting to 30min",
			zap.String("event_id", eventID),
			zap.Error(ttlErr),
		)
		ttl = 30 * time.Minute
	}
	notifiedUntil := time.Now().Add(ttl)

	// Reivindica ATOMICAMENTE o próximo da fila (FOR UPDATE SKIP LOCKED):
	// callers concorrentes pegam clientes DISTINTOS. Sem isso, duas
	// liberações simultâneas selecionavam o MESMO W1 e consumiam 2 unidades
	// avançando a fila só 1 — o próximo ficava preso com uma unidade perdida.
	next, err := s.repo.ClaimNextWaitlistItem(ctx, eventID, productID, notifiedUntil)
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to claim next waitlist item", zap.Error(err))
		return
	}
	if next == nil {
		logger.From(ctx, s.logger).Info(TracePrefix+"promote: nobody in the queue for this product",
			zap.String("event_id", eventID), zap.String("product_id", productID))
		return // fila vazia
	}

	// Gate de estoque PRIMEIRO (gate atômico no contador do produto), ANTES do
	// advisory-lock. Callers concorrentes reivindicam carts DISTINTOS (SKIP
	// LOCKED), então cada um pegaria o lock do SEU cart e o seguraria (via defer)
	// enquanto abre uma 2ª conexão para as queries seguintes — sob uma multidão
	// de promoções simultâneas isso é hold-and-wait e esgota o pool (deadlock).
	// Passando o gate antes, só o(s) goroutine(s) que REALMENTE tomam unidade
	// seguem para o lock; os demais revertem para 'waiting' sem nunca prendê-lo.
	// Promoção PARCIAL: toma ATÉ a quantidade pedida — o cliente recebe o que
	// houver e continua na fila pelo restante. Take provisório: revertido +
	// estoque devolvido em qualquer falha abaixo, então stock.reserved só sai no
	// ponto de sucesso definitivo.
	taken, err := s.repo.DecrementProductStockUpTo(ctx, productID, next.Quantity)
	if err != nil {
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to take stock for waitlist promotion",
			zap.String("product_id", productID),
			zap.String("waitlist_item_id", next.ID),
			zap.Error(err),
		)
		return
	}
	if taken <= 0 {
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Error("failed to revert waitlist claim after stock gate miss",
				zap.String("waitlist_item_id", next.ID),
				zap.Error(revErr),
			)
		}
		logger.From(ctx, s.logger).Debug("waitlist promote skipped: stock not available",
			zap.String("product_id", productID),
			zap.String("waitlist_item_id", next.ID),
		)
		return
	}

	// Tomou a unidade → serializa a MUTAÇÃO do cart contra uma finalização de
	// PAGAMENTO concorrente do MESMO cart sob o advisory-lock que ExpireCart
	// também respeita. O guard de ExpireCart já abstém-se de um cart de
	// waitlister enquanto o item está 'waiting' ou 'notified' dentro da janela
	// (a reivindicação atômica grava wi.expires_at no futuro no MESMO passo em
	// que vira 'notified'), então a corrida com a EXPIRAÇÃO está fechada sem o
	// lock — ele é a 2ª linha, só contra o pagamento. Uma expiração concorrente
	// que porventura o segure abstém-se e o solta rápido, então um RETRY curto
	// quase sempre o adquire. Falha após as tentativas OU cart terminal (pago) →
	// DEVOLVE a unidade ao estoque e REVERTE a reivindicação para 'waiting' (NÃO
	// cancela): o cliente não perde a vez e é promovido no próximo ciclo.
	release, acquired, lockErr := s.acquireCartFinalisationLockRetry(ctx, next.CartID)
	if lockErr != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Warn("waitlist promote: failed to acquire cart lock",
			zap.String("cart_id", next.CartID), zap.Error(lockErr))
		return
	}
	if !acquired {
		// Pagamento/finalização do cart segurou o lock além do retry. Devolve a
		// unidade ao estoque e o cliente ao TOPO da fila (não cancela) — ele é
		// promovido no próximo release. Garante que ninguém perde a vez.
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Warn("waitlist promote: failed to revert claim after lock contention",
				zap.String("waitlist_item_id", next.ID), zap.Error(revErr))
		}
		logger.From(ctx, s.logger).Info("waitlist promote deferred: cart lock held, buyer kept in queue",
			zap.String("cart_id", next.CartID), zap.String("waitlist_item_id", next.ID))
		return
	}
	defer release()

	// Sob o lock, relê o cart: se ele foi PAGO/cancelado logo antes de pegarmos o
	// lock, está terminal — não promover nele. Devolve a unidade e reverte a
	// reivindicação para 'waiting' (mantém o cliente na fila para a próxima).
	if snap, snapErr := s.repo.GetCartExpirySnapshot(ctx, next.CartID); snapErr != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Warn("waitlist promote: failed to read cart snapshot",
			zap.String("cart_id", next.CartID), zap.Error(snapErr))
		return
	} else if snap == nil || cartExpiryTerminal(snap) {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		if revErr := s.repo.RevertWaitlistToWaiting(ctx, next.ID); revErr != nil {
			logger.From(ctx, s.logger).Warn("waitlist promote: failed to revert claim on terminal cart",
				zap.String("waitlist_item_id", next.ID), zap.Error(revErr))
		}
		logger.From(ctx, s.logger).Info("waitlist promote deferred: target cart terminal, buyer kept in queue",
			zap.String("cart_id", next.CartID))
		return
	}

	product, err := s.repo.GetProductByID(ctx, storeID, productID)
	if err != nil || product == nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to get product for waitlist promotion",
			zap.String("product_id", productID),
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return
	}

	// Promote: the cart_items row was created at waitlist signup with
	// quantity == waitlisted_quantity. Flip it to "available" by decrementing
	// waitlisted_quantity in place — re-running AddToCart here would hit
	// UpsertCartItem's ON CONFLICT branch and add both columns again, ending
	// up with quantity=2N, waitlisted=N (the shipping query then computes
	// available=N but the cart visibly carries phantom items, and a later
	// quantity edit on the inflated row leaves available <= 0, which makes
	// /shipping-quote return "nenhum item no carrinho para cotar" on a cart
	// that the FE still renders as non-empty).
	cartID := next.CartID
	found, err := s.repo.DecrementCartItemWaitlistedQuantity(ctx, cartID, productID, taken)
	if err != nil {
		_ = s.repo.IncrementProductStock(ctx, productID, taken)
		_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
		logger.From(ctx, s.logger).Error("failed to decrement waitlisted quantity on promotion",
			zap.String("waitlist_item_id", next.ID),
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	if !found {
		// Edge case: the cart_items row was deleted between waitlist signup
		// and promotion (manual remove from the dashboard, expiration sweep,
		// etc.). Recreate it via the standard AddToCart path — no existing
		// row, no upsert inflation.
		if s.live == nil {
			// Defensive: the live service is always injected in production
			// (integration passes s.liveService before the eager build). A nil
			// here means a misconfigured test wiring — revert so the buyer keeps
			// their place instead of silently losing the promotion.
			_ = s.repo.IncrementProductStock(ctx, productID, taken)
			_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
			logger.From(ctx, s.logger).Error("cannot recreate cart item on promotion: live service not wired",
				zap.String("waitlist_item_id", next.ID),
				zap.String("cart_id", cartID),
			)
			return
		}
		fbResult, fbErr := s.live.AddToCart(ctx, live.AddToCartInput{
			StoreID:            storeID,
			EventID:            eventID,
			PlatformUserID:     next.PlatformUserID,
			PlatformHandle:     next.PlatformHandle,
			ProductID:          productID,
			ProductPrice:       product.Price,
			Quantity:           taken,
			WaitlistedQuantity: 0,
		})
		if fbErr != nil {
			_ = s.repo.IncrementProductStock(ctx, productID, taken)
			_ = s.repo.RevertWaitlistToWaiting(ctx, next.ID)
			logger.From(ctx, s.logger).Error("failed to recreate cart item on promotion",
				zap.String("waitlist_item_id", next.ID),
				zap.Error(fbErr),
			)
			return
		}
		cartID = fbResult.CartID
	}

	// Promoção PARCIAL: se demos menos que o pedido, re-enfileira o restante
	// (o cliente ganhou `taken` no carrinho e continua na fila pelo resto). Se
	// atendemos tudo, a row já está 'notified' pela reivindicação atômica.
	remaining := next.Quantity - taken
	if remaining > 0 {
		if err := s.repo.RequeueWaitlistItemPartial(ctx, next.ID, remaining); err != nil {
			logger.From(ctx, s.logger).Warn("failed to requeue partial waitlist remainder",
				zap.String("waitlist_item_id", next.ID),
				zap.Int("remaining", remaining),
				zap.Error(err),
			)
		}
	}

	cartToken, tokenErr := s.repo.GetCartTokenByID(ctx, cartID)
	if tokenErr != nil {
		logger.From(ctx, s.logger).Warn("failed to read cart token after waitlist promotion",
			zap.String("cart_id", cartID),
			zap.Error(tokenErr),
		)
	}

	// "Gordura": empurra o cart.expires_at para garantir que o cliente tem
	// o TTL configurado para finalizar. Usa GREATEST no banco — não encolhe
	// um cart que já tinha um expires_at maior. (A row da fila já foi marcada
	// 'notified' com notifiedUntil pela reivindicação atômica no topo.)
	if extendErr := s.repo.ExtendCartExpiration(ctx, cartID, notifiedUntil); extendErr != nil {
		logger.From(ctx, s.logger).Warn("failed to extend cart expiration for notified waitlist",
			zap.String("cart_id", cartID),
			zap.Error(extendErr),
		)
	}

	// Re-arma a task asynq cart.expire para a NOVA janela estendida. A task
	// original (armada no finalize) já disparou — ou expirou o cart, ou pulou
	// por causa do lock acima — então, sem o sweep, nada mais expiraria este
	// cart quando o prazo estendido vencer. ScheduleExpiry lê o expires_at atual
	// (já estendido) e agenda no horário novo. Best-effort + idempotente.
	if armErr := s.collab.ScheduleExpiry(ctx, cartID); armErr != nil {
		logger.From(ctx, s.logger).Warn("failed to re-arm expiry for promoted cart",
			zap.String("cart_id", cartID), zap.Error(armErr))
	}

	// ERP saída — reserva pareada às `taken` unidades recém-liberadas para o
	// cart. Falha aqui não bloqueia: o worker de reconciliação pega depois.
	if syncErr := s.collab.ReserveStockInERP(ctx, storeID, cartID, eventID, productID, taken, product.Price, next.PlatformHandle); syncErr != nil {
		logger.From(ctx, s.logger).Warn("failed to reserve stock in ERP for promoted waitlist item",
			zap.String("cart_id", cartID),
			zap.Error(syncErr),
		)
	}

	// Fire-and-forget DM. Email é desnecessário aqui — o cliente foi
	// adicionado via comentário no Instagram, então não temos email
	// confirmado a essa altura. (Quando ele abrir o checkout e preencher
	// o email, o cart já vai estar com o item disponível.)
	s.collab.NotifyWaitlistPromoted(ctx, WaitlistNotifiedInput{
		StoreID:        storeID,
		EventID:        eventID,
		EventTitle:     "", // resolved by the collaborator if needed
		CartID:         cartID,
		CartToken:      cartToken,
		PlatformUserID: next.PlatformUserID,
		PlatformHandle: next.PlatformHandle,
		ProductName:    product.Name,
		ProductKeyword: product.Keyword,
		Quantity:       taken,
		TTL:            ttl,
	})

	// Definitive success point: every revert path above returns early, so
	// reaching this line means the buyer was actually promoted. Emit the
	// provisional stock.reserved (op waitlist_promote) here, keyed by the cart.
	s.stock.NoteReserved(ctx, erp.ReserveParams{Op: erp.StockOpWaitlistPromote, ProductID: productID, Quantity: taken, CartID: cartID, EventID: eventID})

	// waitlist.notified — emitted only here, at the definitive success point:
	// every revert path above returns early, so reaching this line means the
	// buyer was actually promoted and notified. Best-effort (the promotion state
	// is already committed across several steps).
	if emitErr := s.repo.EmitWaitlistNotified(ctx, EmitWaitlistNotifiedParams{
		WaitlistItemID: next.ID,
		EventID:        eventID,
		ProductID:      productID,
		CartID:         cartID,
		Quantity:       taken,
		Remaining:      remaining,
	}); emitErr != nil {
		logger.From(ctx, s.logger).Warn("failed to emit waitlist.notified",
			zap.String("waitlist_item_id", next.ID),
			zap.Error(emitErr),
		)
	}

	logger.From(ctx, s.logger).Info("waitlist promoted",
		zap.String("user", next.PlatformHandle),
		zap.String("waitlist_item_id", next.ID),
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.Int("requested", next.Quantity),
		zap.Int("promoted", taken),
		zap.Int("still_waiting", remaining),
		zap.Bool("partial", remaining > 0),
		zap.Duration("ttl", ttl),
		zap.Time("notified_until", notifiedUntil),
	)
}

// ExpireNotifiedWaitlistItem expira uma entrada 'notified' cujo TTL passou:
// devolve o item ao estoque, reverte a reserva no ERP, marca como
// 'expired' e tenta promover o próximo da fila. Best-effort em todos os
// passos secundários — só falha se a marcação de status falhar (que é o
// gate de idempotência: enquanto status='notified' a row reaparece no
// próximo sweep).
func (s *Service) ExpireNotifiedWaitlistItem(ctx context.Context, item WaitlistItemRow) error {
	cartID := item.CartID
	productID := item.ProductID
	eventID := item.EventID

	if cartID != "" {
		// Devolve o item do carrinho. DecrementCartItem deleta a row se
		// zerar — mantém a invariante quantity>0.
		if _, err := s.repo.DecrementCartItem(ctx, cartID, productID, item.Quantity); err != nil {
			logger.From(ctx, s.logger).Warn("failed to decrement cart item on waitlist expire",
				zap.String("waitlist_item_id", item.ID),
				zap.String("cart_id", cartID),
				zap.String("product_id", productID),
				zap.Error(err),
			)
		}

		// Reverte a reserva no ERP e devolve o estoque local em uma única
		// chamada (AdjustStockReservationDelta lida com ambos para delta<0).
		// Falha aqui não bloqueia — o sweep tenta de novo no próximo tick.
		cart, _ := s.repo.GetCartByID(ctx, cartID)
		if cart != nil {
			if _, err := s.collab.AdjustStockReservationDelta(ctx, cart.StoreID, cartID, eventID, productID, -item.Quantity, 0, cart.PlatformHandle, erp.StockOpWaitlistExpire); err != nil {
				logger.From(ctx, s.logger).Warn("failed to reverse reservation on waitlist expire",
					zap.String("waitlist_item_id", item.ID),
					zap.String("cart_id", cartID),
					zap.Error(err),
				)
			}
		} else {
			// Cart desapareceu (raro): nada para reverter no ERP, mas ainda
			// precisamos devolver o estoque local que o promote consumiu.
			if err := s.stock.Release(ctx, erp.ReleaseParams{Op: erp.StockOpWaitlistExpire, ProductID: productID, Quantity: item.Quantity, CartID: cartID, EventID: eventID}); err != nil {
				logger.From(ctx, s.logger).Warn("failed to increment local stock on waitlist expire (cart missing)",
					zap.String("waitlist_item_id", item.ID),
					zap.Error(err),
				)
			}
		}
	}

	// Marca como expired — esse é o gate de idempotência. Se isso falhar,
	// a próxima varredura tenta de novo.
	now := time.Now()
	if err := s.repo.UpdateWaitlistItemStatus(ctx, item.ID, "expired", nil, nil, &now); err != nil {
		return fmt.Errorf("marking waitlist item expired: %w", err)
	}

	// waitlist.expired — emitido best-effort logo após o gate de idempotência
	// (o flip para 'expired'). Só chega aqui uma vez por item graças ao gate.
	if emitErr := s.repo.EmitWaitlistExpired(ctx, item.ID, eventID, productID, cartID); emitErr != nil {
		logger.From(ctx, s.logger).Warn("failed to emit waitlist.expired",
			zap.String("waitlist_item_id", item.ID),
			zap.Error(emitErr),
		)
	}

	// Tenta promover o próximo da fila para esse evento+produto.
	storeID := ""
	if item.CartID != "" {
		if cart, _ := s.repo.GetCartByID(ctx, item.CartID); cart != nil {
			storeID = cart.StoreID
		}
	}
	if storeID == "" {
		// Fallback: resolve via evento (se cart sumiu por algum motivo).
		// ProcessWaitlistForProduct tolera storeID vazio na lookup do
		// produto? Não — então só pulamos a promoção. O Tiny webhook
		// pega depois.
		logger.From(ctx, s.logger).Info("waitlist expired but storeID unresolved, skipping next-promotion",
			zap.String("waitlist_item_id", item.ID),
			zap.String("event_id", eventID),
		)
		return nil
	}
	s.ProcessWaitlistForProduct(ctx, eventID, productID, storeID)
	return nil
}

// ExpireNotifiedWaitlistSweep busca todos os 'notified' vencidos e chama
// ExpireNotifiedWaitlistItem em cada um. Retorna a contagem de processados
// e o primeiro erro encontrado (não interrompe — best-effort).
func (s *Service) ExpireNotifiedWaitlistSweep(ctx context.Context) (int, error) {
	items, err := s.repo.ListExpiredNotifiedWaitlist(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing expired notified waitlist: %w", err)
	}
	processed := 0
	var firstErr error
	for _, item := range items {
		if err := s.ExpireNotifiedWaitlistItem(ctx, item); err != nil {
			logger.From(ctx, s.logger).Warn("failed to expire notified waitlist item",
				zap.String("waitlist_item_id", item.ID),
				zap.Error(err),
			)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		processed++
	}
	return processed, firstErr
}

// ProcessWaitlistAfterStockWebhook é o backstop para mudanças de estoque
// vindas do ERP que não foram disparadas por uma operação local (ex.:
// merchant ajusta saldo manualmente no Tiny, devolução, importação).
// Resolve o produto local pelo external_id e tenta promover o próximo da
// fila em cada evento ativo. ProcessWaitlistForProduct é idempotente e
// gateado por DecrementProductStock — chamadas concorrentes não causam
// over-promotion.
func (s *Service) ProcessWaitlistAfterStockWebhook(ctx context.Context, storeID, externalSource, externalProductID string) error {
	productID, err := s.repo.GetProductIDByExternalID(ctx, storeID, externalSource, externalProductID)
	if err != nil {
		return fmt.Errorf("resolving product by external id: %w", err)
	}
	if productID == "" {
		// Produto não cadastrado no LiveCart — não temos fila para ele.
		return nil
	}

	// Defesa em profundidade: com finalização ERP em voo para algum cart pago
	// contendo o produto, o saldo reportado pelo webhook pode ser a inflação
	// transitória da reversão de reservas — promover agora enviaria a DM
	// (irreversível) contra unidades já vendidas. A promoção não é perdida:
	// o LaunchOrderStock/estornos subsequentes disparam novos webhooks e o
	// backstop roda de novo com o guard já desarmado. Fail-safe: em erro de
	// DB, adia a promoção (direção conservadora).
	inFlight, guardErr := s.repo.HasInFlightFinalisationForProduct(ctx, productID)
	if guardErr != nil {
		logger.From(ctx, s.logger).Warn("failed to check in-flight finalisation, deferring waitlist backstop",
			zap.String("product_id", productID),
			zap.Error(guardErr),
		)
		return nil
	}
	if inFlight {
		logger.From(ctx, s.logger).Info("deferring waitlist backstop: ERP finalisation in flight for product",
			zap.String("product_id", productID),
			zap.String("store_id", storeID),
		)
		return nil
	}

	eventIDs, err := s.repo.ListEventsWithWaitingByProduct(ctx, productID)
	if err != nil {
		return fmt.Errorf("listing events with waiting waitlist: %w", err)
	}
	for _, eventID := range eventIDs {
		s.ProcessWaitlistForProduct(ctx, eventID, productID, storeID)
	}
	return nil
}

// TracePrefix marca as linhas da investigação de expiração/fila.
// TODO REMOVER junto com o restante do [IGTRACE].
const TracePrefix = "[IGTRACE] "
