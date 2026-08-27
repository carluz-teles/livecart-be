package integration

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Motivos de morte de um cart (coluna carts.cancelled_reason). São o
// discriminador entre os produtores de cart.cancelled: só o cancelamento do
// LOJISTA estorna estoque/ERP por reactor; o bloqueio de handle já faz seu
// próprio estorno inline e o cancelamento de cobrança não mexe em estoque.
const (
	CancelReasonStore   = "store_cancelled"
	CancelReasonBlocked = "customer_blocked"
	CancelReasonExpired = "expired"
	CancelReasonPayment = "payment_cancelled"
)

// CancelCart cancela UM carrinho por decisão do lojista, com a mesma proteção de
// corrida da expiração — e uma regra de negócio extra: **pagamento vence**.
//
// Ordem (crítica):
//  1. Advisory lock por cart — serializa contra o confirm/finalize do webhook de
//     pagamento (que toma o mesmo lock). !acquired = pagamento finalizando AGORA;
//     recusamos o cancelamento em vez de correr com ele.
//  2. Flip 'cancelled' + devolução do estoque local de TODOS os itens + morte da
//     fila do cart, numa única transação (CancelCartAndReleaseStock). O flip é
//     guard-first: se o cart foi pago no intervalo, 0 rows → não elegível →
//     aborta sem tocar em estoque nem ERP, e o lojista recebe 409.
//  3. ERP (Tiny) roda FORA daqui, no reactor de cart.cancelled, com retry + DLQ.
//  4. Promove a waitlist de cada produto liberado (pós-commit).
//
// O caminho inverso da corrida (pagamento confirmado DEPOIS do cancelamento) é
// tratado no webhook: RestoreCancelledCartAsPaid traz o pedido de volta como
// pago. Aqui e lá, o dinheiro manda.
func (s *Service) CancelCart(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")

	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Error("cancel: failed to acquire cart lock",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	if !acquired {
		logger.From(ctx, s.logger).Info("cancel: refused, finalisation in progress",
			zap.String("cart_id", cartID))
		return httpx.ErrConflict("pagamento em processamento para este carrinho — tente novamente em instantes")
	}
	defer release()

	res, err := s.repo.CancelCartAndReleaseStock(ctx, cartID, storeID)
	if err != nil {
		logger.From(ctx, s.logger).Error("cancel: failed to cancel cart",
			zap.String("cart_id", cartID), zap.Error(err))
		return err
	}
	if !res.Eligible {
		logger.From(ctx, s.logger).Info("cancel: cart not eligible (paid/terminal)",
			zap.String("cart_id", cartID))
		return httpx.ErrConflict("carrinho não pode ser cancelado: já foi pago, expirou ou já estava cancelado")
	}

	logger.From(ctx, s.logger).Info("cart cancelled by store",
		zap.String("cart_id", cartID),
		zap.Int("items_released", len(res.FreedProductIDs)),
	)

	// Estoque devolvido = alguém da fila pode ser atendido. Idempotente.
	for _, productID := range res.FreedProductIDs {
		s.ProcessWaitlistForProduct(ctx, res.EventID, productID, storeID)
	}
	return nil
}

// ReactCartCancelledERP é o reactor de cart.cancelled para o cancelamento do
// lojista (L3): estorna a pegada do cart no Tiny. Mesma lógica do
// ReactCartExpiredERP — um cart cancelado e um expirado deixam exatamente a
// mesma pegada pré-pagamento — então os dois compartilham reverseCartERPFootprint
// e ganham retry + DLQ do asynq.
func (s *Service) ReactCartCancelledERP(ctx context.Context, cartID, storeID string) error {
	ctx = logger.WithStore(ctx, storeID, "")

	// Relê o estado antes de tocar o ERP. Entre a emissão do fato e a entrega
	// (backlog/retry do asynq) o pagamento pode ter vencido o cancelamento e
	// restaurado o cart — estornar aqui cancelaria o pedido de uma venda paga.
	// Guard-first, mesma filosofia do flip.
	snap, err := s.repo.GetCartExpirySnapshot(ctx, cartID)
	if err != nil {
		return err
	}
	if snap == nil || snap.Status != "cancelled" {
		logger.From(ctx, s.logger).Info("cancel: skipping ERP reversal, cart is no longer cancelled",
			zap.String("cart_id", cartID))
		return nil
	}

	return s.reverseCartERPFootprint(ctx, cartID, storeID)
}

// reverseCartERPFootprint desfaz no ERP o que um cart não pago segurava:
// carts convertidos (design C) têm o PEDIDO cancelado — é o cancelamento que
// devolve o estoque no Tiny — e carts não convertidos têm as reservas
// saída-manual estornadas. Idempotente por erp_order_state.
//
// Um cart cancelado pelo lojista e um expirado deixam EXATAMENTE a mesma pegada
// pré-pagamento, então reusamos o reactor canônico do pacote erp (OnCartExpired)
// — DRY, um único ponto de verdade para a reversão (Regra nº1). A extração do
// ERP (Bloco B2) tornou reverseCartReservationsInERP privado ao erp; OnCartExpired
// é a superfície pública equivalente.
func (s *Service) reverseCartERPFootprint(ctx context.Context, cartID, storeID string) error {
	return s.ERP().OnCartExpired(ctx, cartID, storeID)
}

// =============================================================================
// O CANCELAMENTO QUE O ERP DESFEZ
// =============================================================================

// ReopenCartResult conta o que a ressurreição conseguiu recuperar.
type ReopenCartResult struct {
	Reopened bool
	CartID   string
	EventID  string
	// Recuperadas é quantas unidades voltaram para o carrinho com estoque.
	Recuperadas int
	// EmFila é quantas não couberam e foram para a lista de espera. É o número
	// que o lojista precisa ver: o carrinho voltou, mas não inteiro.
	EmFila int
}

// ReopenCartFromERP traz de volta o carrinho que o lojista reabriu no ERP.
//
// O gesto do outro lado é claro: ele cancelou, se arrependeu, e reabriu o pedido
// no Tiny à mão. Seguir isso é o que o LiveCart deve fazer — o pedido de lá já
// está vivo e reservando peça, e deixar o carrinho morto aqui produz justamente
// a unidade presa sem dono que a detecção denunciava.
//
// ═══ O QUE MUDA ENTRE CANCELAR E RESSUSCITAR ═══
//
// Cancelar devolveu o estoque. Entre o cancelamento e a reabertura, outra
// compradora pode ter levado a peça — e é por isso que a ressurreição aceita
// voltar INCOMPLETA: leva o que houver e manda o resto para a fila de espera.
// Recusar tudo por causa de uma unidade jogaria fora o carrinho inteiro, que é o
// oposto do que o lojista pediu ao reabrir.
//
// O pedido do ERP não é tocado aqui: ele já está vivo, foi o lojista quem o
// reabriu, e é isso que estamos seguindo. O que volta é o estado da máquina para
// 'open', para o próximo comentário mutar a grade em vez de criar pedido novo.
func (s *Service) ReopenCartFromERP(ctx context.Context, cartID, storeID string) (ReopenCartResult, error) {
	var out ReopenCartResult
	ctx = logger.WithStore(ctx, storeID, "")

	release, acquired, err := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if err != nil {
		return out, err
	}
	if !acquired {
		// Pagamento em voo neste carrinho. O pagamento vence sempre, e ele tem
		// o seu próprio caminho de restauração — sair daqui é o certo.
		logger.From(ctx, s.logger).Info("reopen: refused, finalisation in progress",
			zap.String("cart_id", cartID))
		return out, nil
	}
	defer release()

	out, err = s.repo.ReopenCancelledCartFromERP(ctx, cartID, storeID)
	if err != nil {
		return out, fmt.Errorf("reopening cancelled cart: %w", err)
	}
	if !out.Reopened {
		logger.From(ctx, s.logger).Info("reopen: cart not eligible (expired, paid or not store-cancelled)",
			zap.String("cart_id", cartID))
		return out, nil
	}

	logger.From(ctx, s.logger).Info("cart reopened because the merchant reopened the order in the ERP",
		zap.String("cart_id", cartID),
		zap.Int("units_recovered", out.Recuperadas),
		zap.Int("units_waitlisted", out.EmFila),
	)
	return out, nil
}
