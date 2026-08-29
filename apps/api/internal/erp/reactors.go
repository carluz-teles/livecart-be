package erp

// Reactors ERP no padrão On<Fato> (docs/domain-map.md §8).

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// FinalizeOrConfirm é a entrada única do caminho pago.
//
// Era um par: tentava o confirm do pedido e, quando o carrinho não tinha pedido,
// caía numa finalização legada que criava o pedido do zero, lançava estoque e
// estornava as reservas manuais. Esse segundo caminho não existe mais — o
// carrinho sempre tem pedido desde o primeiro comentário, e quando não tem o
// próprio confirm cria um. Restou a chamada direta; o nome fica porque é o que
// os reactors e as delegações chamam.
func (s *Service) FinalizeOrConfirm(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	return s.ConfirmERPOrderPayment(ctx, cartID, storeID, status)
}

// OnOrderPaid finalises the ERP order in reaction to the order.paid fact
// (emitted transactionally by OnCartPaid once the immutable Order exists), NOT
// cart.paid directly — decoupling the ERP retry loop from the customer-facing
// fan-out. It needs the FRESH gateway snapshot (installments, fees,
// money-release date) frozen into the order.paid payload. Errors are returned so
// asynq retries + dead-letters (idempotent via the advisory lock + resumable
// markers). Stores without an ERP integration no-op so a paid order never churns
// retries. A nil snapshot finalises without payment details — admin retry
// replays afterwards.
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

// OnOrderRefunded cancels the ERP order in reaction to the order.refunded fact
// (emitted transactionally by OnCartRefunded once the Order is flipped to
// 'refunded'), NOT cart.refunded directly. Idempotent by erp_order_state (once
// cancelled, a re-run no-ops), so returning the error for asynq retry + DLQ is
// safe — no payment snapshot needed.
func (s *Service) OnOrderRefunded(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	if err := s.RefundConvertedCartOrder(ctx, cartID, storeID); err != nil {
		return fmt.Errorf("erp refund on order.refunded: %w", err)
	}
	return nil
}

// OnCartExpired devolve o estoque do carrinho que venceu, cancelando o pedido.
// Uma chamada, idempotente pelo erp_order_state — o erro sobe para o asynq
// repetir. Carrinho sem pedido não tem nada preso no ERP e sai por aqui mesmo.
func (s *Service) OnCartExpired(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")
	return s.CancelERPOrderForCart(ctx, cartID, storeID)
}
