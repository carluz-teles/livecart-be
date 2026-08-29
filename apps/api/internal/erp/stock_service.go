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

// StockCollaborators groups the integration-Service helpers the ERP flow still
// calls back into. It is satisfied by integration.Service (wired when the
// delegating methods build the erp.Service), so erp stays free of the
// integration import.
type StockCollaborators interface {
	// ResolveProvider builds the ERP provider client for an active integration,
	// honouring the finalisation tests' scripted-provider seam.
	ResolveProvider(ctx context.Context, integration *Integration) (providers.ERPProvider, error)
	// ResolveExternalProduct maps a local product to its ERP external id.
	// linked=false means there is nothing to move against the ERP — no product
	// syncer wired, the product is not linked, or the lookup failed — and the
	// caller MUST treat that as a silent no-op (the source semantics).
	ResolveExternalProduct(ctx context.Context, storeID, productID string) (externalID string, linked bool)

	// ResolveERPContact finds/creates and enriches the ERP contact for a platform
	// user (the recurring-customer prewarm hits the same cache).
	ResolveERPContact(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, platformUserID, platformHandle, name, document, email, phone string) (string, error)
	// CreateERPOrderForCart cria o pedido de venda do carrinho (situação Aberta,
	// sem pagamento e sem qualquer movimentação de estoque) e grava o
	// external_order_id. É a reserva.
	//
	// Devolve a grade que foi de fato enviada. Ela é o ponto de partida da
	// reconciliação seguinte: sem saber o que o pedido já tem, a reconciliação
	// gastaria um PUT redundante em toda venda.
	CreateERPOrderForCart(ctx context.Context, provider providers.ERPProvider, integration *Integration, storeID, cartID string) ([]providers.ERPOrderItem, error)
	// MirrorToOrder projects the cart's current ERP state into the Order
	// aggregate (best-effort; no-op when the mirror is not wired).
	MirrorToOrder(ctx context.Context, cartID string)
	// MarkFinalisationFailed records the 'failed' finalisation state with the
	// error and emits the group G erp.finalization_failed fact (best-effort).
	MarkFinalisationFailed(ctx context.Context, cartID, msg string)
	// EmitERPOrderFinalized publishes the group G erp.order_finalized fact for a
	// confirmed order (best-effort, dedup by cart).
	EmitERPOrderFinalized(ctx context.Context, storeID, cartID string)
	// EmitERPOrderCancelled publishes the group G erp.order_cancelled fact for a
	// cancelled/refunded order (best-effort, dedup by order id).
	EmitERPOrderCancelled(ctx context.Context, storeID, cartID, externalOrderID, reason string)

	// --- Cart NFe sync / health-check collaborators ---

	// ResolveERPProviderByID resolves the ERP provider for a specific integration
	// id (the health-check anchors on the integration, not the store's active
	// one). Backed by integration.Service.GetERPProvider.
	ResolveERPProviderByID(ctx context.Context, integrationID, storeID string) (providers.ERPProvider, error)
	// HandleProviderError records a provider failure into integration telemetry
	// (integration_logs / status), keeping that ACL concern in the integration
	// package. Best-effort — it never returns.
	HandleProviderError(ctx context.Context, integrationID, operation string, err error)
}

// =============================================================================
// COMENTÁRIO DA LIVE → PEDIDO DE VENDA
// =============================================================================

// ReserveStockInERP segura a peça no ERP para um item que acabou de entrar no
// carrinho. Quem segura é o PEDIDO DE VENDA, e só ele.
//
// Primeiro comentário do comprador: cria o pedido. Do segundo em diante: a
// grade inteira volta por `PUT /pedidos/{id}/itens`, e o ERP reajusta a reserva
// sozinho. Nenhum dos dois toca o saldo físico.
//
// Medido em 26/08/2026 contra a conta real, dois comentários no mesmo carrinho:
//
//	antes             saldo=5  reservado=0  disponivel=5
//	1º comentário     saldo=5  reservado=1  disponivel=4
//	2º comentário     saldo=5  reservado=3  disponivel=2
//
// O que isto substituiu: uma saída manual tipo `S` por comentário, que baixava o
// físico, disparava o webhook de estoque de volta na nossa fila, exigia um
// estorno no pagamento e deixava reserva órfã sempre que qualquer um desses
// passos falhava no meio.
//
// PRÉ-REQUISITO: a conta precisa ter o módulo de Reserva de Estoque ativo. Sem
// ele o pedido não reserva nada e a live venderia às cegas.
func (s *Service) ReserveStockInERP(ctx context.Context, storeID, cartID, eventID, productID string, quantity int, unitPrice int64, platformHandle string) error {
	if _, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny"); err != nil {
		logger.From(ctx, s.logger).Debug("no active ERP integration, skipping stock reservation",
			zap.String("store_id", storeID),
		)
		return nil
	}

	// O carrinho já tem pedido: a grade nova entra por mutação. Cobre o segundo
	// comentário, o item somado depois do pix e a promoção de fila num único
	// ponto — todos são "a grade do banco mudou, mande-a".
	st, stErr := s.repo.GetCartERPOrderState(ctx, cartID)
	switch {
	case stErr != nil || st.State == OrderStateNone:
		// segue para a criação, abaixo
	case st.State == OrderStateCancelled:
		return nil // carrinho encerrado; não ressuscita
	case st.State == OrderStateConverting:
		// 'converting' é ambíguo, e quem desfaz a ambiguidade é o relógio:
		// criação em voo AGORA, ou criação que morreu antes do POST.
		//
		// Se está em voo, ela monta a grade a partir do banco — onde este item já
		// está — e reconcilia ao terminar; EnsureERPOrderForCart apenas registra e
		// sai. Passada a carência, ela retoma, e é assim que um comentário
		// seguinte destrava o carrinho sem esperar a varredura.
		//
		// Antes isto virava erro ("cart não está em 'open'") e o item ficava só no
		// carrinho. Depois virou um `return nil` — que calou o erro mas deixou o
		// carrinho preso, porque ninguém mais chamava a retomada.
		if err := s.EnsureERPOrderForCart(ctx, cartID, storeID); err != nil {
			return fmt.Errorf("resuming order creation for cart %s: %w", cartID, err)
		}
		return nil
	default:
		if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
			return fmt.Errorf("applying grid to cart order: %w", mutErr)
		}
		return nil
	}

	// Primeiro item: o pedido nasce, e ao nascer já segura tudo que está no
	// carrinho — inclusive itens que tenham entrado enquanto isto rodava, porque
	// a grade é sempre reconstruída do banco.
	if err := s.EnsureERPOrderForCart(ctx, cartID, storeID); err != nil {
		return fmt.Errorf("creating sales order for cart %s: %w", cartID, err)
	}
	return nil
}

// AdjustStockReservationDelta aplica um delta de quantidade (positivo ou
// negativo) ao contador local e replica a grade resultante no pedido do ERP.
//
// O contador local é o portão: ele é atômico, é o que a fila de espera consulta
// e é o que responde ao comprador na hora. Por isso ele se move PRIMEIRO, e uma
// recusa do ERP o desfaz — o comprador vê o erro em vez de levar um "ok" sobre
// estoque que o ERP não separou.
//
// Não devolve mais id de movimento: não há movimento. O que existe é o pedido, e
// ele é identificado pelo external_order_id do carrinho.
//
// op rotula o evento emitido; StockOpUnspecified usa o rótulo padrão pelo sinal
// (qty_increase / qty_decrease).
func (s *Service) AdjustStockReservationDelta(ctx context.Context, storeID, cartID, eventID, productID string, delta int, unitPrice int64, platformHandle string, op StockOp) (string, error) {
	if delta == 0 {
		return "", nil
	}

	if delta > 0 {
		if err := s.repo.DecrementProductStock(ctx, productID, delta); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", httpx.DomainError(422, httpx.CodeStockInsufficient, "estoque insuficiente para esse aumento")
			}
			return "", fmt.Errorf("decrementing local stock: %w", err)
		}
	} else {
		if err := s.repo.IncrementProductStock(ctx, productID, -delta); err != nil {
			return "", fmt.Errorf("releasing local stock: %w", err)
		}
	}

	// localCommitted marca se a mutação local acima ainda vale. rollbackLocal a
	// limpa, para o emit adiado disparar SÓ no sucesso definitivo — todo caminho
	// de rollback devolve erro.
	localCommitted := true
	rollbackLocal := func() {
		localCommitted = false
		var err error
		if delta > 0 {
			err = s.repo.IncrementProductStock(ctx, productID, delta)
		} else {
			err = s.repo.DecrementProductStock(ctx, productID, -delta)
		}
		if err != nil {
			logger.From(ctx, s.logger).Error("failed to rollback local stock after ERP failure",
				zap.String("product_id", productID),
				zap.Int("delta", delta),
				zap.Error(err),
			)
		}
	}

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

	// A grade final já está no banco quando chegamos aqui (o chamador grava o
	// cart_item ANTES), então a mutação não precisa saber do delta: ela manda o
	// carrinho inteiro e o ERP converge. Duas edições concorrentes do mesmo
	// carrinho terminam no mesmo lugar por construção.
	st, stErr := s.repo.GetCartERPOrderState(ctx, cartID)
	if stErr != nil || st.State == OrderStateNone || st.State == OrderStateCancelled {
		// Sem pedido ainda (ou já cancelado): nada a espelhar no ERP. O contador
		// local mandou, que é o que o comprador enxerga.
		return "", nil
	}
	if mutErr := s.MutateERPOrderItems(ctx, cartID, storeID); mutErr != nil {
		rollbackLocal()
		return "", fmt.Errorf("applying grid to cart order: %w", mutErr)
	}
	return "", nil
}
