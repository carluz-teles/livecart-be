package integration

// Design C — pedido Tiny como reserva a partir da INICIAÇÃO DO PAGAMENTO.
//
// A fase live continua barata (saída manual, 1 escrita por mutação). Quando o
// comprador clica em pagar — o último instante em que grade, endereço e frete
// estão congelados, para cliente novo E recorrente — o cart converte:
//
//	POST /pedidos (Aberta, sem pagamento, com endereço+frete+numeroOrdemCompra)
//	→ marcador lc-cart-<id> (âncora de idempotência, busca em ~300ms)
//	→ lancar-estoque (launch-first: o saldo MERGULHA, nunca faz pico)
//	→ estornos per-row das saídas manuais (saldo volta ao real)
//	→ erp_order_state = 'open'
//
// Pagamento confirmado vira 2 PUTs sem NENHUMA movimentação de estoque
// (parcelas reais + situação Aprovada) — o gap da finalização morre
// estruturalmente para carts convertidos. Edições no checkout usam o ciclo
// estornar → PUT /itens → lançar (imposto pela própria API: "estoque
// lançado" bloqueia o PUT — sandbox 11/07). Expiração/refund: cancelar →
// estornar (cancelamento NÃO devolve estoque sozinho — sandbox T7/C4).
//
// Máquina de estados (coluna carts.erp_order_state):
//
//	none → converting → open ⇄ mutating → confirmed
//	                      └──────────────→ cancelled
//
// Regra sagrada: 'converting' NUNCA volta para 'none' — a chamada em voo pode
// ter sucedido server-side; resetar reabriria o caminho legado e duplicaria a
// baixa. Quem resolve converting sem pedido é a adoção por marcador (confirm)
// e o sweep (ListStuckERPOrderOps).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
)

const (
	erpOrderStateNone       = "none"
	erpOrderStateConverting = "converting"
	erpOrderStateOpen       = "open"
	erpOrderStateMutating   = "mutating"
	erpOrderStateConfirmed  = "confirmed"
	erpOrderStateCancelled  = "cancelled"
)

// ErrCartNotConverted sinaliza ao caller (paid-webhook) que o cart não tem
// pedido-como-reserva — o fluxo deve cair na finalização legada.
var ErrCartNotConverted = errors.New("cart não convertido em pedido ERP")

func erpOrderMarker(cartID string) string { return "lc-cart-" + cartID }

// isTinyInsufficientBalanceErr reconhece a rejeição de lançamento por saldo
// insuficiente sob "estoque negativo = Não". Mensagem real capturada na conta
// de teste em 11/07: "Não é possível integrar o estoque deste pedido pois o
// saldo em estoque de um ou mais produtos é insuficiente." Usado só para
// classificar logs/métricas — o fallback dispara para qualquer erro de launch.
func isTinyInsufficientBalanceErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "insuficiente")
}

// erpOrderModeEnabled diz se a loja opera no modo pedido-como-reserva.
// Reusa o mecanismo de flag por loja da Fase 3 (env própria) para permitir
// rollout independente: ERP_ORDER_AT_CHECKOUT_STORE_IDS ("*" = todas).
func (s *Service) erpOrderModeEnabled(storeID string) bool {
	return s.orderAtCheckoutAll || s.orderAtCheckoutStoreIDs[storeID]
}

// PrepareCartForPayment é o gancho da INICIAÇÃO de pagamento (card/pix/link):
// converte o cart em pedido-como-reserva quando a loja está no modo C.
// Chamado em goroutine pelo checkout (fire-and-forget): o pagamento nunca
// espera o Tiny — se a conversão ainda estiver em voo quando o pago chegar,
// o confirm retoma/adota; se nunca aconteceu, cai no legado.
func (s *Service) PrepareCartForPayment(ctx context.Context, cartID, storeID string) {
	if !s.erpOrderModeEnabled(storeID) {
		return
	}
	if err := s.EnsureERPOrderForCart(ctx, cartID, storeID); err != nil {
		s.logger.Warn("cart conversion on payment initiation failed (legacy fallback remains)",
			zap.String("cart_id", cartID),
			zap.String("store_id", storeID),
			zap.Error(err),
		)
	}
}

// finalizeOrConfirmCartERP é a entrada única do caminho pago: tenta o confirm
// do pedido-como-reserva (2 PUTs, zero estoque) e cai na finalização legada
// quando o cart não foi convertido.
func (s *Service) finalizeOrConfirmCartERP(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	err := s.ConfirmERPOrderPayment(ctx, cartID, storeID, status)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCartNotConverted) {
		return s.finalizeCartERPOrder(ctx, cartID, storeID, status)
	}
	return err
}

// PrewarmERPContact resolve/enriquece o contato Tiny em background quando um
// cliente RECORRENTE abre o checkout (dados já conhecidos pelo @handle) — a
// conversão na iniciação encontra o cache quente e fica ~2 escritas. Erros
// são irrelevantes: o caminho síncrono resolve o contato de qualquer forma.
func (s *Service) PrewarmERPContact(ctx context.Context, storeID, platformUserID, platformHandle, name, document, email, phone string) {
	if !s.erpOrderModeEnabled(storeID) {
		return
	}
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return
	}
	if _, err := s.resolveERPContact(ctx, erpProvider, erpIntegration, storeID, platformUserID, platformHandle, name, document, email, phone); err != nil {
		s.logger.Debug("ERP contact prewarm failed",
			zap.String("store_id", storeID),
			zap.String("platform_user_id", platformUserID),
			zap.Error(err),
		)
	}
}

// RefundConvertedCartOrder devolve o estoque de um pedido CONFIRMADO cujo
// pagamento foi estornado no gateway: situação 2 → estorno (ordem validada;
// estorno funciona pós-cancelamento) → estado 'cancelled'. Resolve o TODO
// antigo do refund para carts convertidos.
func (s *Service) RefundConvertedCartOrder(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != erpOrderStateConfirmed || st.ExternalOrderID == "" {
		return nil // não convertido/não confirmado — fluxo legado de refund (TODO histórico)
	}
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}
	if err := erpProvider.SetOrderSituacao(ctx, st.ExternalOrderID, 2); err != nil {
		return fmt.Errorf("cancelling refunded order: %w", err)
	}
	if err := erpProvider.ReverseOrderStock(ctx, st.ExternalOrderID); err != nil {
		return fmt.Errorf("reversing refunded order stock: %w", err)
	}
	if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateConfirmed, erpOrderStateCancelled); err != nil {
		s.logger.Error("failed to transition refunded cart to cancelled",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
	s.logger.Info("refunded ERP order cancelled and stock returned",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
	)
	return nil
}

// CheckTinyStockWebhookDelivery é o health-check de ENTREGA de webhook:
// integração Tiny ativa sem NENHUM evento 'estoque' na janela = quase certeza
// de URL removida pelo Tiny (eles deletam o cadastro após falhas consecutivas
// e param de entregar em silêncio — aprendizado de campo de 11/07). Loga em
// ERROR (visível no Railway/alertas de log) com dedupe de 24h por integração.
// Follow-up registrado: notificação in-app ao lojista (o inbox atual é
// idea-cêntrico; exige migration própria).
func (s *Service) CheckTinyStockWebhookDelivery(ctx context.Context, staleAfter time.Duration) {
	stale, err := s.repo.ListTinyIntegrationsWithStaleStockWebhook(ctx, staleAfter)
	if err != nil {
		s.logger.Error("stock webhook delivery check failed to list", zap.Error(err))
		return
	}
	for _, integ := range stale {
		fields := []zap.Field{
			zap.String("integration_id", integ.IntegrationID),
			zap.String("store_id", integ.StoreID),
			zap.Duration("stale_after", staleAfter),
		}
		if integ.LastStockEventAt != nil {
			fields = append(fields, zap.Time("last_stock_event_at", *integ.LastStockEventAt))
		} else {
			fields = append(fields, zap.String("last_stock_event_at", "nunca"))
		}
		s.logger.Error("TINY WEBHOOK POSSIVELMENTE REMOVIDO: sem eventos de estoque na janela — recadastrar a URL no painel do Tiny", fields...)
		if stampErr := s.repo.StampIntegrationStockWebhookAlert(ctx, integ.IntegrationID); stampErr != nil {
			s.logger.Warn("failed to stamp stock webhook alert",
				zap.String("integration_id", integ.IntegrationID),
				zap.Error(stampErr),
			)
		}
	}
}

// RunERPOrderOpsSweep reconcilia conversões/mutações presas (processo morreu
// no meio): converting com pedido → termina a conversão; converting sem
// pedido → tenta adotar por marcador; mutating → re-aplica a grade do banco.
// Chamado por ticker no main. NUNCA regride estado para 'none'.
func (s *Service) RunERPOrderOpsSweep(ctx context.Context) {
	stuck, err := s.repo.ListStuckERPOrderOps(ctx, 10*time.Minute)
	if err != nil {
		s.logger.Error("ERP order ops sweep failed to list", zap.Error(err))
		return
	}
	for _, op := range stuck {
		switch {
		case op.State == erpOrderStateConverting && op.ExternalOrderID != "":
			if err := s.finishERPOrderConversion(ctx, op.CartID, op.StoreID, op.ExternalOrderID); err != nil {
				s.logger.Warn("sweep failed to finish conversion",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
		case op.State == erpOrderStateConverting:
			erpIntegration, intErr := s.repo.GetActiveByProvider(ctx, op.StoreID, "erp", "tiny")
			if intErr != nil {
				continue
			}
			erpProvider, provErr := s.erpProviderFor(ctx, erpIntegration)
			if provErr != nil {
				continue
			}
			foundID, findErr := erpProvider.FindOrderIDByMarker(ctx, erpOrderMarker(op.CartID))
			if findErr != nil || foundID == "" {
				// Sem pedido rastreável: o confirm cairá no legado; o estado
				// converting vazio é cosmético e fica para auditoria.
				continue
			}
			if updErr := s.repo.UpdateCartExternalOrderID(ctx, op.CartID, foundID); updErr != nil {
				s.logger.Error("sweep failed to adopt orphan order",
					zap.String("cart_id", op.CartID), zap.Error(updErr))
				continue
			}
			s.logger.Info("sweep adopted orphan ERP order via marker",
				zap.String("cart_id", op.CartID),
				zap.String("external_order_id", foundID),
			)
			if err := s.finishERPOrderConversion(ctx, op.CartID, op.StoreID, foundID); err != nil {
				s.logger.Warn("sweep failed to finish adopted conversion",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
		case op.State == erpOrderStateMutating && op.ExternalOrderID != "":
			if err := s.applyCartGridToOrder(ctx, op.CartID, op.StoreID, op.ExternalOrderID); err != nil {
				s.logger.Warn("sweep failed to reconcile mutating cart",
					zap.String("cart_id", op.CartID), zap.Error(err))
				continue
			}
			if _, err := s.repo.TransitionCartERPOrderState(ctx, op.CartID, erpOrderStateMutating, erpOrderStateOpen); err != nil {
				s.logger.Error("sweep failed to return cart to open",
					zap.String("cart_id", op.CartID), zap.Error(err))
			}
		}
	}
}

// EnsureERPOrderForCart converte o cart em pedido Tiny na iniciação do
// pagamento. Single-flight via CAS none→converting; idempotente (estados pós-
// conversão retornam nil sem tocar o ERP). Erros deixam o estado 'converting'
// com external_order_id como marcador de progresso — a retomada acontece na
// próxima iniciação, no confirm (adoção) ou no sweep; NUNCA regride a 'none'.
func (s *Service) EnsureERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}

	switch st.State {
	case erpOrderStateOpen, erpOrderStateMutating, erpOrderStateConfirmed:
		return nil // já convertido
	case erpOrderStateCancelled:
		return nil // não ressuscita cart cancelado
	case erpOrderStateConverting:
		if st.ExternalOrderID != "" {
			// Conversão anterior morreu entre o create e o open — retoma.
			return s.finishERPOrderConversion(ctx, cartID, storeID, st.ExternalOrderID)
		}
		// Em voo agora (outra iniciação) ou crash pré-create: não bloqueia o
		// pagamento; adoção por marcador acontece no confirm/sweep.
		s.logger.Info("ERP order conversion already in flight for cart",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return nil // loja sem Tiny — modo legado segue valendo
	}

	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateNone, erpOrderStateConverting)
	if err != nil {
		return fmt.Errorf("claiming cart conversion: %w", err)
	}
	if !won {
		return nil // outra iniciação converteu/está convertendo
	}

	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart for conversion: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Pedido SEM pagamento (nasce Aberta, sem contas a receber — validado em
	// sandbox), SEM launch (orquestrado abaixo, launch-first com fallback).
	// createFinalERPOrder resolve o contato (cache quente para recorrentes),
	// monta endereço+frete e grava external_order_id.
	if createErr := s.createFinalERPOrder(ctx, erpProvider, erpIntegration, storeID, cart.EventID, *cart, nil, false); createErr != nil {
		// Estado permanece 'converting' de propósito (ver regra no topo).
		return fmt.Errorf("creating ERP order for cart conversion: %w", createErr)
	}

	fresh, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart after conversion create: %w", err)
	}
	if fresh.ExternalOrderID == "" {
		// Cart sem itens vinculados ao ERP: nada a converter. Sem pedido, o
		// único caminho seguro é seguir 'converting' vazio — o confirm cai no
		// legado via ErrCartNotConverted e o cart segue o fluxo antigo.
		s.logger.Info("cart has no ERP-linked items, conversion skipped",
			zap.String("cart_id", cartID),
		)
		return nil
	}

	// Âncora de idempotência ANTES de mexer em estoque. Best-effort: com o
	// numeroOrdemCompra gravado no POST, o sweep reconcilia mesmo sem marcador.
	if markErr := erpProvider.AddOrderMarker(ctx, fresh.ExternalOrderID, erpOrderMarker(cartID)); markErr != nil {
		s.logger.Warn("failed to tag ERP order with cart marker",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", fresh.ExternalOrderID),
			zap.Error(markErr),
		)
	}

	return s.finishERPOrderConversion(ctx, cartID, storeID, fresh.ExternalOrderID)
}

// finishERPOrderConversion completa a conversão a partir de "pedido criado":
// launch-first (com fallback reverse-first para contas que bloqueiam saldo
// negativo), estornos per-row das saídas manuais e CAS converting→open.
// Retomável: launch tolera "já lançado", estornos pulam rows já 'reversed'.
func (s *Service) finishERPOrderConversion(ctx context.Context, cartID, storeID, orderID string) error {
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	if launchErr := erpProvider.LaunchOrderStock(ctx, orderID); launchErr != nil {
		s.logger.Warn("conversion launch-first failed, falling back to reverse-first",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", orderID),
			zap.Bool("insufficient_balance", isTinyInsufficientBalanceErr(launchErr)),
			zap.Error(launchErr),
		)
		if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
			return fmt.Errorf("reversing reservations on conversion fallback: %w", err)
		}
		if retryErr := erpProvider.LaunchOrderStock(ctx, orderID); retryErr != nil {
			// Estado fica converting+order — retomável (confirm/sweep/retry).
			return fmt.Errorf("launching order stock after conversion fallback: %w", retryErr)
		}
	}
	if err := s.repo.SetCartERPStockLaunched(ctx, cartID, true); err != nil {
		s.logger.Warn("failed to persist erp_stock_launched",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	if err := s.reverseCartReservationsPerRow(ctx, erpProvider, cartID); err != nil {
		return fmt.Errorf("reversing reservations after conversion launch: %w", err)
	}

	moved, err := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateConverting, erpOrderStateOpen)
	if err != nil {
		return fmt.Errorf("transitioning cart to open: %w", err)
	}
	if !moved {
		s.logger.Info("cart left converting state concurrently, skipping open transition",
			zap.String("cart_id", cartID),
		)
	}
	s.logger.Info("cart converted to ERP order-as-reservation",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
	)
	return nil
}

// MutateERPOrderItems aplica a grade ATUAL do cart ao pedido via o ciclo
// obrigatório estornar-estoque → PUT /itens → lancar-estoque (a API bloqueia
// PUT com estoque lançado). Single-flight via CAS open→mutating. A grade é
// SEMPRE reconstruída do banco — chamadas concorrentes convergem para o
// estado final do cart, não para deltas individuais.
func (s *Service) MutateERPOrderItems(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != erpOrderStateOpen || st.ExternalOrderID == "" {
		return fmt.Errorf("cart %s não está em 'open' (estado %s): %w", cartID, st.State, ErrCartNotConverted)
	}

	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateOpen, erpOrderStateMutating)
	if err != nil {
		return fmt.Errorf("claiming order mutation: %w", err)
	}
	if !won {
		// Outra mutação em voo: ela reconstrói a grade do banco, que já
		// contém a mudança deste caller — convergência garantida.
		s.logger.Info("order mutation already in flight, latest grid will win",
			zap.String("cart_id", cartID),
		)
		return nil
	}
	defer func() {
		// Melhor esforço: devolve para open mesmo em erro — o pedido pode ter
		// ficado estornado (grade divergente), e o confirm SEMPRE reconcilia a
		// grade antes das parcelas exatamente por isso.
		if _, backErr := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateMutating, erpOrderStateOpen); backErr != nil {
			s.logger.Error("failed to return cart to open after mutation",
				zap.String("cart_id", cartID),
				zap.Error(backErr),
			)
		}
	}()

	return s.applyCartGridToOrder(ctx, cartID, storeID, st.ExternalOrderID)
}

// applyCartGridToOrder roda o ciclo estornar → PUT /itens (grade do banco) →
// lançar no pedido do cart. Usado pela mutação e pela reconciliação do confirm.
func (s *Service) applyCartGridToOrder(ctx context.Context, cartID, storeID, orderID string) error {
	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		return fmt.Errorf("listing cart items for order grid: %w", err)
	}
	grid := make([]providers.ERPOrderItem, 0, len(items))
	for _, item := range items {
		if item.ProductExternalID == "" {
			continue
		}
		grid = append(grid, providers.ERPOrderItem{
			ProductID: item.ProductExternalID,
			Name:      item.ProductName,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		})
	}
	if len(grid) == 0 {
		// Grade vazia é ACEITA pela API (sandbox T6) mas nunca é o que o
		// comprador quer — trate como erro de chamada.
		return fmt.Errorf("cart %s sem itens vinculados ao ERP para aplicar no pedido", cartID)
	}

	if err := erpProvider.ReverseOrderStock(ctx, orderID); err != nil {
		return fmt.Errorf("reversing order stock for mutation: %w", err)
	}
	if err := erpProvider.UpdateOrderItems(ctx, orderID, grid); err != nil {
		// Pedido estornado com grade antiga — estado seguro (saldo correto no
		// Tiny, sem baixa dupla); o confirm reconcilia e relança.
		return fmt.Errorf("updating order items: %w", err)
	}
	if err := erpProvider.LaunchOrderStock(ctx, orderID); err != nil {
		return fmt.Errorf("re-launching order stock after mutation: %w", err)
	}
	s.logger.Info("order grid updated via estornar→PUT→lançar cycle",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
		zap.Int("items", len(grid)),
	)
	return nil
}

// ConfirmERPOrderPayment é o caminho pago do pedido-como-reserva: grava as
// parcelas reais do gateway e aprova o pedido — DUAS escritas, zero
// movimentação de estoque, zero webhook adversário. Retorna
// ErrCartNotConverted quando o cart deve seguir a finalização legada.
func (s *Service) ConfirmERPOrderPayment(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	// Mesmo claim por cart da finalização legada: webhooks de gateway chegam
	// duplicados em goroutines concorrentes; a adoção/retomada dentro do
	// confirm mexe em estoque e não pode correr em dupla. O perdedor sai — a
	// redelivery seguinte encontra 'confirmed' e no-opa.
	release, acquired, lockErr := s.repo.AcquireCartFinalisationLock(ctx, cartID)
	if lockErr != nil {
		return fmt.Errorf("acquiring confirm lock: %w", lockErr)
	}
	if !acquired {
		s.logger.Info("ERP confirm already in flight for cart, skipping duplicate trigger",
			zap.String("cart_id", cartID),
		)
		return nil
	}
	defer release()

	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}

	switch st.State {
	case erpOrderStateNone:
		return ErrCartNotConverted
	case erpOrderStateConfirmed:
		return nil // redelivery de webhook — idempotente
	case erpOrderStateCancelled:
		return fmt.Errorf("cart %s pago após cancelamento do pedido ERP — reconciliação manual", cartID)
	case erpOrderStateConverting:
		orderID := st.ExternalOrderID
		if orderID == "" {
			// Adoção por marcador: a conversão pode ter sucedido server-side
			// antes do crash. Sem pedido encontrado → finalização legada.
			erpIntegration, intErr := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
			if intErr != nil {
				return ErrCartNotConverted
			}
			erpProvider, provErr := s.erpProviderFor(ctx, erpIntegration)
			if provErr != nil {
				return fmt.Errorf("creating ERP provider: %w", provErr)
			}
			foundID, findErr := erpProvider.FindOrderIDByMarker(ctx, erpOrderMarker(cartID))
			if findErr != nil {
				return fmt.Errorf("searching order by marker for adoption: %w", findErr)
			}
			if foundID == "" {
				return ErrCartNotConverted
			}
			if updErr := s.repo.UpdateCartExternalOrderID(ctx, cartID, foundID); updErr != nil {
				return fmt.Errorf("adopting ERP order %s: %w", foundID, updErr)
			}
			orderID = foundID
			s.logger.Info("adopted orphan ERP order via marker",
				zap.String("cart_id", cartID),
				zap.String("external_order_id", orderID),
			)
		}
		if err := s.finishERPOrderConversion(ctx, cartID, storeID, orderID); err != nil {
			return fmt.Errorf("finishing conversion before confirm: %w", err)
		}
	case erpOrderStateMutating:
		// Mutação presa (processo morreu no meio do ciclo): reconcilia a
		// grade — o pedido pode estar estornado e/ou com grade velha.
		if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, erpOrderStateMutating, erpOrderStateOpen); err != nil {
			return fmt.Errorf("unsticking mutating cart: %w", err)
		}
		if err := s.applyCartGridToOrder(ctx, cartID, storeID, st.ExternalOrderID); err != nil {
			return fmt.Errorf("reconciling order grid before confirm: %w", err)
		}
	case erpOrderStateOpen:
		// caminho normal
	default:
		return fmt.Errorf("estado ERP inesperado %q para cart %s", st.State, cartID)
	}

	fresh, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("reloading cart ERP order state: %w", err)
	}
	if fresh.ExternalOrderID == "" {
		return ErrCartNotConverted
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	// Parcelas reais do gateway (mesma montagem do legado: valores em cima do
	// total dos itens; MoneyReleaseDate/installments vindos do provider).
	if status != nil {
		items, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
		if err != nil {
			return fmt.Errorf("listing cart items for payment total: %w", err)
		}
		var totalAmount int64
		for _, item := range items {
			if item.ProductExternalID == "" {
				continue
			}
			totalAmount += item.UnitPrice * int64(item.Quantity)
		}
		paidAt := time.Now()
		if status.PaidAt != nil {
			paidAt = *status.PaidAt
		}
		payment := &providers.ERPOrderPayment{
			Method:           status.PaymentMethod,
			PaymentID:        status.PaymentID,
			Installments:     status.Installments,
			PaidAt:           paidAt,
			Amount:           totalAmount,
			MoneyReleaseDate: status.MoneyReleaseDate,
			FeeAmountCents:   status.FeeAmountCents,
			NetAmountCents:   status.NetAmountCents,
		}
		if err := erpProvider.UpdateOrderPayment(ctx, fresh.ExternalOrderID, payment); err != nil {
			s.markFinalisationFailed(ctx, cartID, "gravação das parcelas falhou: "+err.Error(), nil)
			return fmt.Errorf("updating order payment: %w", err)
		}
	}

	if err := erpProvider.SetOrderSituacao(ctx, fresh.ExternalOrderID, 3); err != nil {
		s.markFinalisationFailed(ctx, cartID, "aprovação do pedido falhou: "+err.Error(), nil)
		return fmt.Errorf("approving order: %w", err)
	}

	if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, fresh.State, erpOrderStateConfirmed); err != nil {
		s.logger.Error("failed to transition cart to confirmed",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
	// Mantém a telemetria/guards existentes coerentes: confirmado = done.
	if markErr := s.repo.MarkCartERPFinalisationDone(ctx, cartID); markErr != nil {
		s.logger.Error("failed to mark cart ERP finalisation done after confirm",
			zap.String("cart_id", cartID),
			zap.Error(markErr),
		)
	}
	s.logger.Info("ERP order payment confirmed — two PUTs, zero stock movement",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", fresh.ExternalOrderID),
	)
	return nil
}

// CancelERPOrderForCart cancela o pedido-como-reserva devolvendo o estoque:
// situação 2 e SÓ ENTÃO estornar (cancelamento não devolve sozinho, e o
// estorno funciona em pedido cancelado — sandbox T7; a ordem cancel→estornar
// também fecha a corrida cancelar∥lançar do T11/C4). Idempotente.
func (s *Service) CancelERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart ERP order state: %w", err)
	}
	switch st.State {
	case erpOrderStateNone, erpOrderStateCancelled:
		return nil
	case erpOrderStateConfirmed:
		return fmt.Errorf("cart %s confirmado — cancelamento pós-pago é fluxo de refund", cartID)
	}
	if st.ExternalOrderID == "" {
		// converting sem pedido: nada a cancelar no Tiny; marca cancelado
		// para o sweep não insistir.
		_, _ = s.repo.TransitionCartERPOrderState(ctx, cartID, st.State, erpOrderStateCancelled)
		return nil
	}

	erpIntegration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return fmt.Errorf("loading ERP integration: %w", err)
	}
	erpProvider, err := s.erpProviderFor(ctx, erpIntegration)
	if err != nil {
		return fmt.Errorf("creating ERP provider: %w", err)
	}

	if err := erpProvider.SetOrderSituacao(ctx, st.ExternalOrderID, 2); err != nil {
		return fmt.Errorf("cancelling order: %w", err)
	}
	// Estorno incondicional pós-cancel: no-op seguro se nunca lançou (T2) e
	// único jeito de devolver o saldo se lançou (T7). Fecha a corrida C4.
	if err := erpProvider.ReverseOrderStock(ctx, st.ExternalOrderID); err != nil {
		return fmt.Errorf("reversing order stock after cancel: %w", err)
	}
	if err := s.repo.SetCartERPStockLaunched(ctx, cartID, false); err != nil {
		s.logger.Warn("failed to clear erp_stock_launched",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
	if _, err := s.repo.TransitionCartERPOrderState(ctx, cartID, st.State, erpOrderStateCancelled); err != nil {
		return fmt.Errorf("transitioning cart to cancelled: %w", err)
	}
	s.logger.Info("ERP order cancelled and stock returned",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
	)
	return nil
}
