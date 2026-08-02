package inventory

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
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
}

// WaitlistCollaborators is the slice of integration-Service behaviour the
// migrated cancel flow still calls back into — mirrors erp.StockCollaborators.
// AdjustStockReservationDelta ultimately lives in erp, but is reached through the
// integration.Service that already owns the reservation orchestration;
// ProcessWaitlistForProduct is the concurrent queue-promotion core that stays in
// integration until B3b. Declared consumer-side so inventory does not import
// integration; it shrinks as more logic migrates.
type WaitlistCollaborators interface {
	// AdjustStockReservationDelta applies a (positive/negative) quantity delta to a
	// cart+product reservation, mutating both local stock and the ERP reservation.
	AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op erp.StockOp) (string, error)
	// ProcessWaitlistForProduct promotes the next queued entry for an event+product
	// (best-effort; the concurrent core, still integration-owned until B3b).
	ProcessWaitlistForProduct(ctx context.Context, eventID, productID, storeID string)
}

// Service owns the Inventory domain's waitlist/fila business logic. B3a lays the
// foundation (struct + ports) and moves the two low-blast-radius flows
// (ListActiveWaitlistByCart, CancelWaitlistItem); the concurrent core
// (ProcessWaitlistForProduct / ExpireCart) migrates in B3b. Mirrors erp.Service:
// repo (port) + collab (integration callbacks) + stock (local-stock releases) +
// logger.
type Service struct {
	repo   InventoryRepository
	collab WaitlistCollaborators
	stock  *erp.StockReservations
	logger *zap.Logger
}

// NewService creates a new Inventory service. stock is the same
// *erp.StockReservations the integration.Service holds (the release manager for
// LOCAL product stock); collab supplies the integration-Service helpers the
// migrated cancel flow still calls back into. Both shrink as more logic migrates.
func NewService(repo InventoryRepository, collab WaitlistCollaborators, stock *erp.StockReservations, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		collab: collab,
		stock:  stock,
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
			s.collab.ProcessWaitlistForProduct(ctx, item.EventID, item.ProductID, cart.StoreID)
		}
	}
	return true, nil
}
