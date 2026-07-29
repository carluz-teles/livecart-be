package erp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// StockCollaborators groups the integration-Service helpers the migrated ERP
// flow still calls back into. It is satisfied by integration.Service (wired when
// the delegating methods build the erp.Service), so erp stays free of the
// integration import. The interface shrinks slice by slice as the
// provider/contact/order-creation logic itself migrates (B2c-2+).
type StockCollaborators interface {
	// ResolveProvider builds the ERP provider client for an active integration,
	// honouring the finalisation tests' scripted-provider seam.
	ResolveProvider(ctx context.Context, integration *Integration) (providers.ERPProvider, error)
	// ResolveExternalProduct maps a local product to its ERP external id.
	// linked=false means there is nothing to move against the ERP — no product
	// syncer wired, the product is not linked, or the lookup failed — and the
	// caller MUST treat that as a silent no-op (the source semantics).
	ResolveExternalProduct(ctx context.Context, storeID, productID string) (externalID string, linked bool)

	// --- Order-as-reservation lifecycle collaborators (Bloco B2c, Design C) ---

	// OrderAtCheckoutEnabled reports whether the store runs design C (converting
	// the cart into an ERP order from payment initiation). Reads the
	// integration-owned per-store rollout flag.
	OrderAtCheckoutEnabled(storeID string) bool
	// ResolveERPContact finds/creates and enriches the ERP contact for a platform
	// user (the recurring-customer prewarm hits the same cache).
	ResolveERPContact(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, platformUserID, platformHandle, name, document, email, phone string) (string, error)
	// CreateFinalERPOrderForConversion creates the unpaid ERP order (situação
	// Aberta, no stock launch) for a cart conversion and persists its external id.
	CreateFinalERPOrderForConversion(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, cartID string) error
	// ReverseCartReservationsPerRow estorna the cart's active manual stock exits,
	// row by row, marking each only after the ERP confirms the entry.
	ReverseCartReservationsPerRow(ctx context.Context, provider providers.ERPProvider, cartID string) error
	// MarkFinalisationFailed records the 'failed' finalisation state with the
	// error and emits the group G erp.finalization_failed fact (best-effort).
	MarkFinalisationFailed(ctx context.Context, cartID, msg string)
	// MirrorToOrder projects the cart's current ERP state into the Order
	// aggregate (best-effort; no-op when the mirror is not wired).
	MirrorToOrder(ctx context.Context, cartID string)
	// EmitERPOrderFinalized publishes the group G erp.order_finalized fact for a
	// confirmed order-as-reservation (best-effort, dedup by cart).
	EmitERPOrderFinalized(ctx context.Context, storeID, cartID string)
	// EmitERPOrderCancelled publishes the group G erp.order_cancelled fact for a
	// cancelled/refunded order-as-reservation (best-effort, dedup by order id).
	EmitERPOrderCancelled(ctx context.Context, storeID, cartID, externalOrderID, reason string)
}

// =============================================================================
// CART → ERP STOCK RESERVATION (moved from internal/integration, Bloco B2b)
// =============================================================================

// ReserveStockInERP creates a manual stock exit (tipo S) in the ERP for a product
// added to a cart. The movement is tracked in stock_reservations for later reversal.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping stock reservation",
			zap.String("store_id", storeID),
		)
		return nil
	}

	// Cart convertido em pedido-como-reserva (design C): quem segura a peça é
	// o PEDIDO, não saídas manuais — a grade nova (o caller já gravou o item
	// no cart) entra pelo ciclo estornar→PUT→lançar. Cobre o live-add pós-pix
	// e a promoção de waitlist de cart convertido num único ponto.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != OrderStateNone && st.State != OrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			return fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return nil
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Get external product ID
	externalID, linked := s.collab.ResolveExternalProduct(ctx, storeID, productID)
	if !linked {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping stock reservation",
			zap.String("product_id", productID),
		)
		return nil
	}

	// Idempotency: check if an active reservation already exists for this cart+product
	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)
	if len(existing) > 0 {
		logger.From(ctx, s.logger).Debug("stock reservation already exists for cart+product, skipping",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
		)
		return nil
	}

	obs := fmt.Sprintf("Reserva LiveCart - @%s - Evento %s", platformHandle, eventID)
	movementID, err := erpProvider.ReserveStock(ctx, externalID, quantity, float64(unitPrice)/100, obs)
	if err != nil {
		return fmt.Errorf("reserving stock in ERP: %w", err)
	}

	_, err = s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
		EventID:           eventID,
		CartID:            cartID,
		ProductID:         productID,
		ExternalProductID: externalID,
		Quantity:          quantity,
		ERPMovementID:     movementID,
	})
	if err != nil {
		// ERP movement was created but we can't track it locally — attempt compensating reversal
		logger.From(ctx, s.logger).Error("failed to save stock reservation, attempting ERP reversal",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.String("erp_movement_id", movementID),
			zap.Error(err),
		)
		reverseObs := fmt.Sprintf("Estorno compensatório - falha DB - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, externalID, quantity, 0, reverseObs); reverseErr != nil {
			logger.From(ctx, s.logger).Error("CRITICAL: failed to compensate ERP stock after DB failure — manual reconciliation required",
				zap.String("external_product_id", externalID),
				zap.Int("quantity", quantity),
				zap.String("erp_movement_id", movementID),
				zap.Error(reverseErr),
			)
		}
		return fmt.Errorf("saving stock reservation: %w", err)
	}

	logger.From(ctx, s.logger).Info("stock reserved in ERP",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.String("external_product_id", externalID),
		zap.Int("quantity", quantity),
		zap.String("erp_movement_id", movementID),
	)

	return nil
}

// AdjustStockReservationDelta applies a quantity delta (positive or negative)
// to a (cart, product) reservation. It mutates both the local products.stock
// counter and the ERP reservation in a single call so the two stay in sync.
//
// Local stock is the source-of-truth gate for waitlist promotion
// (ProcessWaitlistForProduct's atomic DecrementProductStock) and live-add
// availability (processLiveAdd reads product.Stock). It is mutated FIRST so
// that even stores without an ERP integration get correct waitlist behavior.
// A failure in the optional ERP sync rolls the local mutation back.
//
// On delta > 0, an insufficient-stock condition returns httpx 422 instead of
// silently over-allocating (the previous behavior caused buyers reducing then
// re-increasing their cart to over-allocate against the original stock).
//
// Returns the ERP movement_id (empty when no ERP integration is configured or
// the product is not linked — both treated as no-ops for the ERP side; local
// stock is still updated).
// op labels the emitted stock event; pass StockOpUnspecified to use the default
// sign-based label (qty_increase / qty_decrease), or a specific op (e.g.
// waitlist_cancel / waitlist_expire) when the delta represents a domain action
// other than a buyer quantity edit.
func (s *Service) AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op StockOp) (string, error) {
	if delta == 0 {
		return "", nil
	}

	// 1. Local stock mutation — atomic gate for delta>0, mirror of the ERP
	//    reversal for delta<0. Runs unconditionally so waitlist promotion sees
	//    freed units immediately, even when the store has no ERP integration.
	if delta > 0 {
		if err := s.repo.DecrementProductStock(ctx, productID, delta); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", httpx.ErrUnprocessable("estoque insuficiente para esse aumento")
			}
			return "", fmt.Errorf("decrementing local stock: %w", err)
		}
	} else {
		if err := s.repo.IncrementProductStock(ctx, productID, -delta); err != nil {
			return "", fmt.Errorf("releasing local stock: %w", err)
		}
	}

	// localCommitted tracks whether the local stock mutation above stands. It is
	// cleared by rollbackLocal so the deferred emit below fires ONLY on the
	// definitive success of this operation (every rollback path returns an error).
	localCommitted := true

	// Rollback helper used when ERP sync fails after local stock already moved.
	rollbackLocal := func() {
		localCommitted = false
		if delta > 0 {
			if err := s.repo.IncrementProductStock(ctx, productID, delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock decrement after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		} else {
			if err := s.repo.DecrementProductStock(ctx, productID, -delta); err != nil {
				logger.From(ctx, s.logger).Error("failed to rollback local stock increment after ERP failure",
					zap.String("product_id", productID),
					zap.Int("delta", delta),
					zap.Error(err),
				)
			}
		}
	}

	// stock.reserved / stock.released — the single emit point for this operation.
	// Deferred + guarded so every nil-error return below funnels through here
	// without instrumenting each one, and rollbacks (which clear localCommitted)
	// stay silent.
	defer func() {
		if !localCommitted {
			return
		}
		if delta > 0 {
			reserveOp := op
			if reserveOp == StockOpUnspecified {
				reserveOp = StockOpQtyIncrease
			}
			s.stock.NoteReserved(ctx, ReserveParams{Op: reserveOp, ProductID: productID, Quantity: delta, CartID: cartID, EventID: eventID})
		} else {
			releaseOp := op
			if releaseOp == StockOpUnspecified {
				releaseOp = StockOpQtyDecrease
			}
			s.stock.NoteReleased(ctx, ReleaseParams{Op: releaseOp, ProductID: productID, Quantity: -delta, CartID: cartID, EventID: eventID})
		}
	}()

	// Cart convertido (design C): a mutação vai para o PEDIDO via ciclo
	// estornar→PUT→lançar — a grade final já está no banco (o checkout grava
	// o cart_item ANTES de chamar este método). Sem movimentação manual e sem
	// movementID; falha desfaz o estoque local para o comprador ver o erro.
	if st, stErr := s.repo.GetCartERPOrderState(ctx, cartID); stErr == nil &&
		st.State != OrderStateNone && st.State != OrderStateCancelled {
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			rollbackLocal()
			return "", fmt.Errorf("applying grid to converted cart order: %w", mutErr)
		}
		return "", nil
	}

	// 2. ERP sync — optional. Anything below is best-effort against the ERP;
	//    any failure rolls back the local change above.
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping reservation delta",
			zap.String("store_id", storeID),
		)
		return "", nil
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, integration)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("creating ERP provider: %w", err)
	}

	externalID, linked := s.collab.ResolveExternalProduct(ctx, storeID, productID)
	if !linked {
		logger.From(ctx, s.logger).Debug("product not linked to ERP, skipping reservation delta",
			zap.String("product_id", productID),
		)
		return "", nil
	}

	existing, _ := s.repo.ListActiveReservationsByCartAndProduct(ctx, cartID, productID)

	if delta > 0 {
		obs := fmt.Sprintf("Ajuste reserva LiveCart (+%d) - @%s - Cart %s", delta, platformHandle, cartID)
		movementID, err := erpProvider.ReserveStock(ctx, externalID, delta, float64(unitPrice)/100, obs)
		if err != nil {
			rollbackLocal()
			return "", fmt.Errorf("reserving stock delta in ERP: %w", err)
		}

		if len(existing) == 0 {
			if _, err := s.repo.CreateStockReservation(ctx, CreateStockReservationParams{
				EventID:           eventID,
				CartID:            cartID,
				ProductID:         productID,
				ExternalProductID: externalID,
				Quantity:          delta,
				ERPMovementID:     movementID,
			}); err != nil {
				return movementID, fmt.Errorf("recording new reservation: %w", err)
			}
		} else if _, err := s.repo.AdjustActiveReservationQuantity(ctx, cartID, productID, delta, movementID); err != nil {
			return movementID, fmt.Errorf("bumping reservation quantity: %w", err)
		}

		logger.From(ctx, s.logger).Info("ERP reservation increased",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
			zap.String("erp_movement_id", movementID),
		)
		return movementID, nil
	}

	// delta < 0
	if len(existing) == 0 {
		logger.From(ctx, s.logger).Warn("no active reservation to decrease for cart+product, skipping ERP call",
			zap.String("cart_id", cartID),
			zap.String("product_id", productID),
			zap.Int("delta", delta),
		)
		return "", nil
	}

	// Sum across all active rows (in practice the unique index keeps it to 1).
	currentQty := 0
	for _, r := range existing {
		currentQty += r.Quantity
	}
	newQty := currentQty + delta

	obs := fmt.Sprintf("Ajuste reserva LiveCart (%d) - @%s - Cart %s", delta, platformHandle, cartID)
	movementID, err := erpProvider.ReverseStockReservation(ctx, externalID, -delta, 0, obs)
	if err != nil {
		rollbackLocal()
		return "", fmt.Errorf("reversing stock delta in ERP: %w", err)
	}

	// Full reversal: skip the UPDATE — stock_reservations.quantity has a
	// CHECK (quantity > 0) constraint, so we cannot zero the row in place.
	// Mark it reversed instead.
	if newQty <= 0 {
		if err := s.repo.ReverseReservationsByCartAndProduct(ctx, cartID, productID); err != nil {
			return movementID, fmt.Errorf("marking reservation reversed: %w", err)
		}
	} else if _, err := s.repo.AdjustActiveReservationQuantity(ctx, cartID, productID, delta, movementID); err != nil {
		return movementID, fmt.Errorf("decreasing reservation quantity: %w", err)
	}

	logger.From(ctx, s.logger).Info("ERP reservation decreased",
		zap.String("cart_id", cartID),
		zap.String("product_id", productID),
		zap.Int("delta", delta),
		zap.Int("new_qty", newQty),
		zap.String("erp_movement_id", movementID),
	)
	return movementID, nil
}
