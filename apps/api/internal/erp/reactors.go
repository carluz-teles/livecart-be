package erp

// Bloco B2c-2 — reactors ERP no padrão On<Fato> (docs/domain-map.md §8). O corpo
// destes reactors vive no pacote canônico internal/erp; integration.Service
// mantém as delegações React*ERP (assinaturas inalteradas) para NÃO tocar
// main.go — o rename dos call sites e o wiring direto do reactor são B2e.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// FinalizeOrConfirm é a entrada única do caminho pago: tenta o confirm do
// pedido-como-reserva (2 PUTs, zero estoque) e cai na finalização LEGADA quando
// o cart não foi convertido (ErrCartNotConverted).
func (s *Service) FinalizeOrConfirm(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	err := s.ConfirmERPOrderPayment(ctx, cartID, storeID, status)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCartNotConverted) {
		return s.FinalizeCartERPOrder(ctx, cartID, storeID, status)
	}
	return err
}

// OnOrderPaid finalises the ERP (Tiny) order in reaction to the order.paid fact
// (emitted transactionally by OnCartPaid once the immutable Order exists), NOT
// cart.paid directly — decoupling the ERP retry loop from the customer-facing
// fan-out. It needs the FRESH gateway snapshot (installments, fees, money-release
// date) frozen into the order.paid payload. Errors are returned so asynq retries
// + dead-letters (idempotent via the advisory lock + resumable markers). Stores
// without a Tiny integration no-op so a paid order never churns retries. A nil
// snapshot finalises without payment details — admin retry replays afterwards.
func (s *Service) OnOrderPaid(ctx context.Context, cartID, storeID string, snapshotJSON []byte) error {
	ctx = logger.WithStore(ctx, storeID, "")
	if _, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); err != nil {
		return nil // no active ERP integration — nothing to finalise
	}
	var status *providers.PaymentStatus
	if len(snapshotJSON) > 0 {
		var st providers.PaymentStatus
		if err := json.Unmarshal(snapshotJSON, &st); err == nil {
			status = &st
		} else {
			logger.From(ctx, s.logger).Warn("order.paid ERP reactor: bad payment snapshot, finalising without payment details",
				zap.String("cart_id", cartID), zap.Error(err))
		}
	}
	return s.FinalizeOrConfirm(ctx, cartID, storeID, status)
}

// OnOrderRefunded cancels the converted ERP (Tiny) order and returns its stock
// in reaction to the order.refunded fact (emitted transactionally by
// OnCartRefunded once the Order is flipped to 'refunded'), NOT cart.refunded
// directly. Idempotent by erp_order_state (once cancelled, a re-run no-ops), so
// returning the error for asynq retry + DLQ is safe — no payment snapshot needed.
func (s *Service) OnOrderRefunded(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	if err := s.RefundConvertedCartOrder(ctx, cartID, storeID); err != nil {
		return fmt.Errorf("erp refund on order.refunded: %w", err)
	}
	return nil
}

// OnCartExpired reverses the cart's ERP footprint in Tiny in reaction to the
// cart.expired fact, decoupled from ExpireCart's eligibility flip so it gets its
// own asynq retry + DLQ. Design-C converted carts get their order cancelled
// (idempotent by erp_order_state, so the error is returned for retry);
// non-converted carts have their saída-manual reservations reversed best-effort.
func (s *Service) OnCartExpired(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state on expiry: %w", err)
	}
	if st.State != OrderStateNone && st.State != OrderStateCancelled {
		// Design C: cancelling the converted order returns stock in Tiny.
		return s.CancelERPOrderForCart(ctx, cartID, storeID)
	}
	// Legacy (non-converted): reverse the saída-manual reservations per cart.
	// O erro sobe para o asynq poder repetir — antes era descartado aqui.
	return s.reverseCartReservationsInERP(ctx, cartID, storeID)
}
