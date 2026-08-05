package erp

// Bloco B2c-2 — a finalização LEGADA do cart→ERP (pós-pagamento) vive agora no
// pacote canônico internal/erp. A ORQUESTRAÇÃO migrou para cá; os colaboradores
// que dependem de CartRow (struct grande integration-owned) e da emissão de
// eventos ficaram em internal/integration e são chamados de volta via
// StockCollaborators (mesma abordagem "wired at boot" que quebra o ciclo de
// import). integration.Service mantém delegações finas (assinaturas públicas
// inalteradas); os call sites em main.go/order/checkout migram em B2e.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// FinalizeCartERPOrder is the post-payment ERP workflow: reverse the Tiny
// saída-manual reservations held during the live, then create a single sales
// order already marked as paid, with customer identity and delivery address.
//
// Resumable state machine (Fase 2): every step leaves a durable marker and
// re-entry only moves FORWARD — no compensation on retry:
//
//	[L]  advisory lock por cart — webhooks de gateway duplicados perdem o
//	     lock e retornam cedo; o vencedor termina o trabalho
//	[S1] snapshot do gateway + carimbo da tentativa persistidos ANTES de
//	     tocar o ERP (retry admin faz replay sem novo webhook)
//	[S0] external_order_id já gravado ⇒ RESUME: re-lança o estoque
//	     (tolerante a "Estoque já lançado.") e estorna só as reservas ainda
//	     'active'. Mata os Gaps A e B (carts zumbis presos em 'pending').
//	[S2] estornos per-row: cada reserva só é marcada 'reversed' após o Tiny
//	     confirmar a entrada E; falha aborta com 'failed' retomável.
//	[S3] createFinalERPOrder (grava external_order_id antes do launch)
//	[S4] done
//
// Failure recovery: if the order creation throws AFTER we have already
// reversed reservations, we re-create the saída-manual exits so the unit stays
// held against this cart — stock is never silently released against a paid cart.
func (s *Service) FinalizeCartERPOrder(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	logger.From(ctx, s.logger).Info("starting ERP finalisation for paid cart",
		zap.String("store_id", storeID),
		zap.String("cart_id", cartID),
	)

	// [L] Claim único por cart. Perder o lock significa que outra entrega do
	// mesmo webhook está finalizando AGORA — sem isso, duas goroutines listam
	// as mesmas reservas 'active' e cada uma aplica sua entrada E (saldo do
	// Tiny acima do real: a invariante central do fix quebraria).
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring finalisation lock: %w", lockErr)
	}
	if !acquired {
		logger.From(ctx, s.logger).Info("ERP finalisation already in flight for cart, skipping duplicate trigger",
			zap.String("cart_id", cartID),
		)
		return nil
	}
	defer release()

	// Idempotência dura: um trigger tardio (redelivery de horas depois) sobre
	// cart já finalizado não deve custar nem uma chamada ao Tiny.
	if stRow, stErr := s.repo.GetCartERPFinalisationStatus(ctx, cartID); stErr == nil && stRow.Status == "done" {
		logger.From(ctx, s.logger).Info("cart ERP finalisation already done, skipping",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		// Disambiguate "merchant never set up Tiny" (info, expected) from
		// "Tiny exists but is in error state" (warn, recoverable) so we don't
		// keep losing paid carts under a silent debug log.
		if any, _ := s.repo.GetByProvider(ctx, storeID, "erp", "tiny"); any != nil {
			logger.From(ctx, s.logger).Warn("Tiny integration is not active, skipping paid-order creation",
				zap.String("store_id", storeID),
				zap.String("cart_id", cartID),
				zap.String("integration_id", any.ID),
				zap.String("status", any.Status),
			)
		} else {
			logger.From(ctx, s.logger).Info("no Tiny integration configured, skipping paid-order creation",
				zap.String("store_id", storeID),
				zap.String("cart_id", cartID),
			)
		}
		return nil
	}

	// [S1] Snapshot + carimbo ANTES de agir. Best-effort: a finalização não
	// para por falha aqui, mas o retry perde o replay se o snapshot faltar.
	var snapshot []byte
	if status != nil {
		if b, encErr := json.Marshal(status); encErr == nil {
			snapshot = b
		} else {
			logger.From(ctx, s.logger).Warn("failed to encode payment status snapshot",
				zap.String("cart_id", cartID),
				zap.Error(encErr),
			)
		}
	}
	if markErr := s.repo.MarkCartERPFinalisationAttempt(ctx, cartID, snapshot); markErr != nil {
		logger.From(ctx, s.logger).Warn("failed to mark ERP finalisation attempt",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}

	erpProvider, err := s.collab.ResolveProvider(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// [S0] RESUME: o pedido já existe no Tiny — crash ou falha após o
	// CreateOrder de uma tentativa anterior (Gaps A/B). Nunca pular o launch:
	// ele é tolerante a "já lançado", e pulá-lo devolveria as reservas sem o
	// pedido ter baixado estoque (oversell contra cart pago). external_order_id
	// (reserva) segue no cart e é lido por GetCartERPOrderState.
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.ExternalOrderID != "" {
		return s.resumeCartERPFinalisation(ctx, erpProvider, cartID, storeID, st.ExternalOrderID, snapshot)
	}

	// Fase 3: lojas com a flag ligada finalizam em ordem invertida
	// (launch-first) — a perna de oferta do race morre na origem.
	if s.collab.FinalisationInverted(storeID) {
		return s.finalizeCartERPOrderInverted(ctx, erpProvider, erpIntegration, storeID, cartID, status, snapshot)
	}

	// [S2] Reverse all active saída-manual reservations for this cart — the
	// final order will decrement stock itself via LaunchOrderStock, so keeping
	// the reservations would double-count. Per-row: a row only flips to
	// 'reversed' after Tiny confirmed the entrada E; on failure we abort with
	// a RESUMABLE 'failed' instead of proceeding.
	reservations, err := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart reservations: %w", err)
	}
	logger.From(ctx, s.logger).Info("reversing ERP stock reservations before creating paid order",
		zap.String("cart_id", cartID),
		zap.Int("reservations_count", len(reservations)),
	)

	// Track which reservations actually made it through the Tiny reversal so
	// we know which ones to re-create in the failure path. A reservation that
	// failed to reverse is still "active" on Tiny's side — re-creating it
	// would double-deduct stock.
	reversedSnapshot := make([]StockReservationRow, 0, len(reservations))
	for _, r := range reservations {
		obs := fmt.Sprintf("Estorno reserva pós-pagamento - Cart %s", cartID)
		if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
			msg := fmt.Sprintf("estorno de reserva pendente (produto %s): %v", r.ExternalProductID, reverseErr)
			s.collab.MarkFinalisationFailed(ctx, cartID, msg)
			logger.From(ctx, s.logger).Warn("failed to reverse ERP reservation on paid cart, aborting for retry",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.String("external_product_id", r.ExternalProductID),
				zap.Int("quantity", r.Quantity),
				zap.Error(reverseErr),
			)
			return fmt.Errorf("reversing reservation %s: %w", r.ID, reverseErr)
		}
		if dbErr := s.repo.ReverseReservationByID(ctx, r.ID); dbErr != nil {
			// Tiny estornou mas a marcação local falhou: um retry re-estornaria
			// esta row e DUPLICARIA a entrada E. Loga alto com o movementID
			// para reconciliação manual e segue — a direção do erro é estoque
			// segurado a mais, nunca oferta falsa.
			logger.From(ctx, s.logger).Error("reservation reversed on Tiny but local mark failed — reconcile manually",
				zap.String("cart_id", cartID),
				zap.String("reservation_id", r.ID),
				zap.String("erp_movement_id", r.ERPMovementID),
				zap.Error(dbErr),
			)
		}
		reversedSnapshot = append(reversedSnapshot, r)
	}
	logger.From(ctx, s.logger).Info("ERP stock reservations reversed",
		zap.String("cart_id", cartID),
		zap.Int("requested", len(reservations)),
		zap.Int("succeeded", len(reversedSnapshot)),
	)

	// [S3] Create the paid sales order. On failure, re-reserve and surface the
	// error to the merchant via erp_finalisation_status='failed'. The collab
	// loads the CartRow (integration-owned) and reuses createFinalERPOrder.
	createErr := s.collab.CreateFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cartID, status, true)
	if createErr != nil {
		s.collab.ReReserveAfterFailedFinalisation(ctx, erpProvider, cartID, reversedSnapshot)
		s.collab.MarkFinalisationFailed(ctx, cartID, createErr.Error())
		return createErr
	}

	// [S4]
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		// The order is in Tiny — don't propagate. Just log so the cart shows up
		// in the admin "stuck pending" view if the column ever drifts.
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.collab.EmitERPOrderFinalized(ctx, storeID, cartID)
	return nil
}

// resumeCartERPFinalisation finishes a finalisation that was interrupted AFTER
// the Tiny order already existed (Gaps A/B): re-launches the order stock
// (LaunchOrderStock treats "Estoque já lançado." as success) then reverses only
// the reservations still 'active', and marks the cart done. Monotonic: running
// it twice is a no-op beyond one tolerated launch call.
func (s *Service) resumeCartERPFinalisation(ctx context.Context, erpProvider providers.ERPProvider, cartID, storeID, externalOrderID string, snapshot []byte) error {
	logger.From(ctx, s.logger).Info("resuming ERP finalisation for cart with existing order",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", externalOrderID),
	)

	if err := erpProvider.LaunchOrderStock(ctx, externalOrderID); err != nil {
		msg := fmt.Sprintf("relançamento de estoque do pedido %s falhou: %v", externalOrderID, err)
		s.collab.MarkFinalisationFailed(ctx, cartID, msg)
		return fmt.Errorf("re-launching stock for order %s: %w", externalOrderID, err)
	}

	if err := s.collab.ReverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
		s.collab.MarkFinalisationFailed(ctx, cartID, err.Error())
		return fmt.Errorf("reversing reservations on resume: %w", err)
	}

	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done after resume",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	// Group G fact (best-effort): terminal 'done' reached via the resume path.
	s.collab.EmitERPOrderFinalized(ctx, storeID, cartID)
	logger.From(ctx, s.logger).Info("ERP finalisation resumed to done",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", externalOrderID),
	)
	return nil
}

// finalizeCartERPOrderInverted é a Fase 3 do fix: cria o pedido e LANÇA o
// estoque ANTES de estornar as reservas. O saldo do Tiny nunca sobe acima do
// valor real durante a finalização — o perfil vira um mergulho transitório, e
// mergulho não gera oferta falsa. Bônus estrutural: falha de CreateOrder não
// compensa NADA (as reservas nunca foram tocadas).
//
// Fallback: se o launch falhar (conta com "estoque negativo = Não", saldo preso
// nas próprias saídas manuais das reservas — live esgotada), estorna as reservas
// PRIMEIRO e re-tenta o launch uma vez, degradando para a ordem legada. Não há
// matcher confiável de "saldo insuficiente", então o fallback dispara para
// qualquer erro de launch — inofensivo se transiente, e o resume cobre se o
// re-launch também falhar.
func (s *Service) finalizeCartERPOrderInverted(ctx context.Context, erpProvider providers.ERPProvider, erpIntegration *Integration, storeID, cartID string, status *providers.PaymentStatus, snapshot []byte) error {
	logger.From(ctx, s.logger).Info("finalising cart with inverted order (launch-first)",
		zap.String("cart_id", cartID),
		zap.String("store_id", storeID),
	)

	// [I1] Cria o pedido SEM lançar — o launch é orquestrado aqui fora para
	// permitir o fallback.
	if createErr := s.collab.CreateFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cartID, status, false); createErr != nil {
		s.collab.MarkFinalisationFailed(ctx, cartID, createErr.Error())
		return createErr
	}

	// createFinalERPOrder grava external_order_id no sucesso; recarrega para
	// obtê-lo. Vazio = cart sem itens vinculados ao ERP (create pulou): não há
	// pedido para lançar — só devolve as reservas e encerra.
	fresh, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart after order creation: %w", err)
	}
	orderID := fresh.ExternalOrderID
	if orderID == "" {
		if err := s.collab.ReverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
			s.collab.MarkFinalisationFailed(ctx, cartID, err.Error())
			return err
		}
		if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		return nil
	}

	// [I2] Launch-first, com fallback reverse-first.
	if launchErr := erpProvider.LaunchOrderStock(ctx, orderID); launchErr != nil {
		logger.From(ctx, s.logger).Warn("launch-first failed, falling back to reverse-first order",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", orderID),
			zap.Bool("insufficient_balance", IsTinyInsufficientBalanceErr(launchErr)),
			zap.Error(launchErr),
		)
		if err := s.collab.ReverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
			s.collab.MarkFinalisationFailed(ctx, cartID, err.Error())
			return err
		}
		if retryErr := erpProvider.LaunchOrderStock(ctx, orderID); retryErr != nil {
			msg := fmt.Sprintf("lançamento de estoque do pedido %s falhou após fallback: %v", orderID, retryErr)
			s.collab.MarkFinalisationFailed(ctx, cartID, msg)
			// O pedido existe e external_order_id está gravado: o retry entra
			// pelo RESUME (launch tolerante) e termina o trabalho.
			return fmt.Errorf("launching stock for order %s after fallback: %w", orderID, retryErr)
		}
		if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
			logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
				zap.String("cart_id", cartID),
				zap.Error(markErr),
			)
		}
		s.collab.EmitERPOrderFinalized(ctx, storeID, cartID)
		return nil
	}

	// [I3] Estornos per-row: o saldo do Tiny volta ao valor real. Falha aqui é
	// retomável — o resume re-lança (no-op tolerado) e estorna o restante.
	if err := s.collab.ReverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
		s.collab.MarkFinalisationFailed(ctx, cartID, err.Error())
		return err
	}

	// [I4]
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		logger.From(ctx, s.logger).Error("failed to mark cart ERP finalisation done",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.collab.EmitERPOrderFinalized(ctx, storeID, cartID)
	logger.From(ctx, s.logger).Info("inverted ERP finalisation completed",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
	)
	return nil
}

// RetryERPFinalisation is the admin-triggered retry of the post-payment ERP
// flow. Runs on 'failed' carts (replaying the persisted gateway snapshot) and
// — Fase 2 — also on 'pending' zombies: carts whose finalisation crashed
// mid-flight (Gap A/B). A pending cart is retryable when the Tiny order already
// exists (resume path) or when the last attempt is old/absent; a FRESH pending
// still gets a 422 because the initial flow is running right now.
func (s *Service) RetryERPFinalisation(ctx context.Context, cartID, storeID string) error {
	row, err := s.repo.GetCartERPFinalisationStatus(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP status: %w", err)
	}
	switch row.Status {
	case "done":
		return nil
	case "pending":
		stale := row.LastAttemptAt == nil || time.Since(*row.LastAttemptAt) > 15*time.Minute
		if row.ExternalOrderID == "" && !stale {
			return httpx.DomainError(422, httpx.CodeErpFinalisationInProgress, "aguarde a finalização inicial concluir antes de tentar de novo")
		}
	case "failed":
		// proceed
	default:
		return httpx.DomainError(422, httpx.CodeErpRetryInvalidState, "estado inválido para retry: "+row.Status)
	}

	// Replay the original gateway PaymentStatus snapshot captured on the first
	// attempt (S1). Re-fetching from the gateway would work too but adds an
	// external dependency to a manual action.
	if len(row.PaymentSnapshot) == 0 {
		if row.ExternalOrderID != "" {
			// Resume puro: o pedido já existe no Tiny, e launch + estornos não
			// usam dados de pagamento — dá para terminar sem snapshot.
			return s.FinalizeCartERPOrder(ctx, cartID, storeID, nil)
		}
		return httpx.DomainError(422, httpx.CodeErpRetryNoSnapshot, "snapshot de pagamento ausente — retry não disponível")
	}
	var status providers.PaymentStatus
	if err := json.Unmarshal(row.PaymentSnapshot, &status); err != nil {
		return fmt.Errorf("decoding payment snapshot: %w", err)
	}

	return s.FinalizeCartERPOrder(ctx, cartID, storeID, &status)
}

// reverseCartReservationsInERP estorna todas as reservas saída-manual ativas de
// um cart NÃO convertido na expiração (design C converte → cancela o pedido).
// Best-effort no ERP; as rows locais são marcadas 'reversed' de qualquer jeito
// (a expiração é terminal), com um WARN quando o ERP não confirmou tudo.
func (s *Service) reverseCartReservationsInERP(ctx context.Context, cartID, storeID string) error {
	reservations, resErr := s.repo.ListActiveReservationsByCart(ctx, cartID)
	if resErr != nil {
		logger.From(ctx, s.logger).Error("expiry: failed to list reservations", zap.String("cart_id", cartID), zap.Error(resErr))
		return fmt.Errorf("listing reservations to reverse: %w", resErr)
	}
	if len(reservations) == 0 {
		return nil
	}

	// A marcação é POR RESERVA, logo após o Tiny confirmar aquela entrada.
	//
	// Era uma marcação só, no fim do laço, com o MESMO ctx. O handler tem 15s de
	// orçamento e cada chamada ao Tiny leva ~1s pelo limitador: um carrinho com
	// muitos itens estoura, e o UPDATE final recebe um contexto já morto. Foi o
	// que aconteceu em 05/08 às 13:52:46 — "failed to mark reservations
	// reversed: context deadline exceeded".
	//
	// O estrago não é perder a marcação: é perder o PROGRESSO. As reservas que o
	// Tiny já tinha estornado continuaram 'active', então a retentativa
	// estornou tudo de novo e criou unidade que não existe. Marcando uma a uma,
	// a retentativa vira idempotente por construção — ela só vê as que sobraram.
	//
	// Este é o padrão que o caminho design-C já usa (ReverseReservationByID,
	// stock_reservation.sql:83). Só o caminho legado ficou para trás.
	//
	// markCtx sobrevive ao cancelamento do handler: o movimento no Tiny JÁ
	// aconteceu, e não registrar isso é o que dispara o estorno duplicado.
	markCtx, cancelMark := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelMark()

	erpReversed := true
	if integration, intErr := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); intErr == nil {
		if erpProvider, provErr := s.collab.ResolveProvider(ctx, integration); provErr == nil {
			for _, r := range reservations {
				obs := fmt.Sprintf("Estorno expiração carrinho LiveCart - Cart %s - Reserva %s", cartID, r.ID)
				if _, reverseErr := erpProvider.ReverseStockReservation(ctx, r.ExternalProductID, r.Quantity, 0, obs); reverseErr != nil {
					erpReversed = false
					logger.From(ctx, s.logger).Warn("expiry: failed to reverse ERP reservation",
						zap.String("cart_id", cartID),
						zap.String("reservation_id", r.ID),
						zap.String("external_product_id", r.ExternalProductID),
						zap.Error(reverseErr))
					continue
				}
				// Confirmado no Tiny: registra ANTES de seguir para a próxima.
				if markErr := s.repo.ReverseReservationByID(markCtx, r.ID); markErr != nil {
					erpReversed = false
					logger.From(ctx, s.logger).Error("expiry: ERP reversed but marking failed — retry would double-reverse",
						zap.String("cart_id", cartID),
						zap.String("reservation_id", r.ID),
						zap.Error(markErr))
				}
			}
		} else {
			erpReversed = false
			logger.From(ctx, s.logger).Error("expiry: failed to build ERP provider", zap.String("cart_id", cartID), zap.Error(provErr))
		}
	} else {
		erpReversed = false
		logger.From(ctx, s.logger).Warn("expiry: no active ERP integration, marking reservations reversed locally only",
			zap.String("store_id", storeID))
		// Sem ERP não há movimento a duplicar: marcar em bloco é seguro e é o
		// que evita deixar a reserva presa numa loja sem integração.
		if markErr := s.repo.ReverseReservationsByCart(markCtx, cartID); markErr != nil {
			logger.From(ctx, s.logger).Error("expiry: failed to mark reservations reversed", zap.String("cart_id", cartID), zap.Error(markErr))
		}
	}
	if !erpReversed {
		logger.From(ctx, s.logger).Warn("expiry: ERP reservations NOT fully reversed — manual reconciliation may be needed",
			zap.String("cart_id", cartID))
		// O erro SOBE. Era engolido: o reactor devolvia nil e o asynq
		// considerava a task um sucesso, então a retentativa que salvou o
		// carrinho em 05/08 aconteceu por acidente — por outro listener
		// herdando o mesmo contexto morto. Com a marcação per-row acima, o
		// retry agora é seguro: ele só vê as reservas que sobraram.
		return fmt.Errorf("cart %s: ERP reservations not fully reversed", cartID)
	}
	return nil
}
