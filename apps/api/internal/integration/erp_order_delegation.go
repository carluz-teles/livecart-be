package integration

// A máquina de estados do pedido vive em internal/erp (erp/order_lifecycle.go).
// Este arquivo mantém em internal/integration:
//
//   - os aliases in-package do estado/sentinela, para não churnar os call sites;
//   - as DELEGAÇÕES finas dos métodos públicos (assinaturas inalteradas);
//   - os COLABORADORES que erp.Service chama de volta (provider, contato, criação
//     do pedido, espelho, eventos), satisfazendo erp.StockCollaborators.

import (
	"context"
	"fmt"
	"time"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/order"
)

// Estado ERP e sentinela vivem no pacote canônico internal/erp (Bloco B2a).
// Mantemos estes aliases in-package enquanto a finalização legada e os reactors
// (B2c-2) ainda usam os nomes unexported, para não churnar os call sites.
const (
	erpOrderStateNone       = erp.OrderStateNone
	erpOrderStateConverting = erp.OrderStateConverting
	erpOrderStateOpen       = erp.OrderStateOpen
	erpOrderStateMutating   = erp.OrderStateMutating
	erpOrderStateConfirmed  = erp.OrderStateConfirmed
	erpOrderStateCancelled  = erp.OrderStateCancelled
)

// ErrCartNotConverted é um alias para o canônico erp.ErrCartNotConverted (mesmo
// valor, então errors.Is continua funcionando nos call sites legados).
var ErrCartNotConverted = erp.ErrCartNotConverted

// =============================================================================
// DELEGAÇÕES (assinaturas públicas inalteradas — call sites migram em B2e)
// =============================================================================

// PrepareCartForPayment delega para erp.Service (Bloco B2c-1).
func (s *Service) PrepareCartForPayment(ctx context.Context, cartID, storeID string) {
	s.erpStock().PrepareCartForPayment(ctx, cartID, storeID)
}

// PrewarmERPContact delega para erp.Service (Bloco B2c-1).
func (s *Service) PrewarmERPContact(ctx context.Context, storeID, platformUserID, platformHandle, name, document, email, phone string) {
	s.erpStock().PrewarmERPContact(ctx, storeID, platformUserID, platformHandle, name, document, email, phone)
}

// EnsureERPOrderForCart delega para erp.Service (Bloco B2c-1).
func (s *Service) EnsureERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().EnsureERPOrderForCart(ctx, cartID, storeID)
}

// MutateERPOrderItems delega para erp.Service (Bloco B2c-1).
func (s *Service) MutateERPOrderItems(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().MutateERPOrderItems(ctx, cartID, storeID)
}

// ConfirmERPOrderPayment delega para erp.Service (Bloco B2c-1).
func (s *Service) ConfirmERPOrderPayment(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	return s.erpStock().ConfirmERPOrderPayment(ctx, cartID, storeID, status)
}

// CancelERPOrderForCart delega para erp.Service (Bloco B2c-1).
func (s *Service) CancelERPOrderForCart(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().CancelERPOrderForCart(ctx, cartID, storeID)
}

// RefundConvertedCartOrder delega para erp.Service (Bloco B2c-1).
func (s *Service) RefundConvertedCartOrder(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().RefundConvertedCartOrder(ctx, cartID, storeID)
}

// RunERPOrderOpsSweep delega para erp.Service (Bloco B2c-1). Segue chamado por
// ticker no main.go (a troca do wiring para erpSvc é B2e).
func (s *Service) RunERPOrderOpsSweep(ctx context.Context) {
	s.erpStock().RunERPOrderOpsSweep(ctx)
}

// CheckTinyStockWebhookDelivery delega para erp.Service (Bloco B2c-1).
func (s *Service) CheckTinyStockWebhookDelivery(ctx context.Context, staleAfter time.Duration) {
	s.erpStock().CheckTinyStockWebhookDelivery(ctx, staleAfter)
}

// finalizeOrConfirmCartERP delega para erp.Service. Mantido para os testes
// in-package que exercitam o caminho pago diretamente.
func (s *Service) finalizeOrConfirmCartERP(ctx context.Context, cartID, storeID string, status *providers.PaymentStatus) error {
	return s.erpStock().FinalizeOrConfirm(ctx, cartID, storeID, status)
}

// RetryERPFinalisation delega para erp.Service. Segue chamado por internal/order
// (o botão de reenviar do painel).
func (s *Service) RetryERPFinalisation(ctx context.Context, cartID, storeID string) error {
	return s.erpStock().RetryERPFinalisation(ctx, cartID, storeID)
}

// reopenerAdapter traduz o relatório de reabertura para o tipo do erp — mesmo
// motivo dos outros adaptadores: nenhum dos dois pacotes importa o outro.
type reopenerAdapter struct{ svc *Service }

func (a reopenerAdapter) CancelCartFromERP(ctx context.Context, cartID, storeID string) (bool, error) {
	return a.svc.CancelCartFromERP(ctx, cartID, storeID)
}

func (a reopenerAdapter) MarkCartPaidFromERP(ctx context.Context, cartID, storeID string, amountCents int64) (bool, error) {
	return a.svc.MarkCartPaidFromERP(ctx, cartID, storeID, amountCents)
}

func (a reopenerAdapter) ReopenCartFromERP(ctx context.Context, cartID, storeID string) (erp.ReopenReport, error) {
	rel, err := a.svc.ReopenCartFromERP(ctx, cartID, storeID)
	return erp.ReopenReport{
		Reopened:    rel.Reopened,
		Recuperadas: rel.Recuperadas,
		EmFila:      rel.EmFila,
	}, err
}

// MergeERPOrdersIntoCart delega para erp.Service: faz o pedido do carrinho
// eterno absorver o que os outros seguravam, e só então solta os outros.
func (s *Service) MergeERPOrdersIntoCart(ctx context.Context, destCartID, storeID string, orfaos []erp.ERPOrderMerge) (*erp.MergeReport, error) {
	return s.erpStock().MergeERPOrdersIntoCart(ctx, destCartID, storeID, orfaos)
}

// RecomporParcelasDoPedidoPago delega para erp.Service: separa, no pedido, o que
// já foi pago do que ainda falta.
func (s *Service) RecomporParcelasDoPedidoPago(ctx context.Context, cartID, storeID string) (*erp.SplitDePagamento, error) {
	return s.erpStock().RecomporParcelasDoPedidoPago(ctx, cartID, storeID)
}

// SyncCartFromERPOrder delega para erp.Service: traz para o carrinho o que o
// pedido no ERP diz hoje.
func (s *Service) SyncCartFromERPOrder(ctx context.Context, cartID, storeID string) (*erp.CartSyncReport, error) {
	return s.erpStock().SyncCartFromERPOrder(ctx, cartID, storeID)
}

// RunERPOrderStatusSweep delega para erp.Service.
func (s *Service) RunERPOrderStatusSweep(ctx context.Context, staleAfter time.Duration, limit int) {
	s.erpStock().RunERPOrderStatusSweep(ctx, staleAfter, limit)
}

// ListERPOrderStatusHistory satisfaz order.ERPOrderStatusReader: devolve o
// trajeto do pedido no ERP para a tela de pedidos, sem que o pacote order
// precise importar este.
func (s *Service) ListERPOrderStatusHistory(ctx context.Context, cartID string) ([]order.ERPOrderStatusEntry, error) {
	rows, err := s.repo.ListERPOrderStatusHistory(ctx, cartID)
	if err != nil {
		return nil, err
	}
	out := make([]order.ERPOrderStatusEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, order.ERPOrderStatusEntry{
			Status:         r.Status,
			PreviousStatus: r.PreviousStatus,
			OrderNumber:    r.OrderNumber,
			Source:         r.Source,
			ObservedAt:     r.ObservedAt,
		})
	}
	return out, nil
}

// =============================================================================
// COLABORADORES (satisfazem erp.StockCollaborators — Bloco B2c-1)
// =============================================================================

// ResolveERPContact converte a Integration neutra e reusa o helper legado
// resolveERPContact (find/create + enriquecimento do contato Tiny).
func (s *Service) ResolveERPContact(ctx context.Context, provider providers.ERPProvider, integration *erp.Integration, storeID, platformUserID, platformHandle, name, document, email, phone string) (string, error) {
	return s.resolveERPContact(ctx, provider, integrationRowFromERP(integration), storeID, platformUserID, platformHandle, name, document, email, phone)
}

// CreateERPOrderForCart carrega o carrinho e cria o pedido de venda no ERP —
// situação Aberta, sem pagamento e sem movimentação de estoque. Grava o
// external_order_id no carrinho em caso de sucesso.
func (s *Service) CreateERPOrderForCart(ctx context.Context, provider providers.ERPProvider, integration *erp.Integration, storeID, cartID string) ([]providers.ERPOrderItem, error) {
	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart for ERP order: %w", err)
	}
	return s.createERPOrderForCart(ctx, provider, integrationRowFromERP(integration), storeID, cart.EventID, *cart)
}

// MarkFinalisationFailed reusa o helper legado (grava 'failed' + emite o fato).
func (s *Service) MarkFinalisationFailed(ctx context.Context, cartID, msg string) {
	s.markFinalisationFailed(ctx, cartID, msg, nil)
}

// MirrorToOrder reusa o helper legado que projeta o estado ERP na Order.
func (s *Service) MirrorToOrder(ctx context.Context, cartID string) {
	s.mirrorToOrder(ctx, cartID)
}

// EmitERPOrderFinalized reusa o helper legado do fato erp.order_finalized.
func (s *Service) EmitERPOrderFinalized(ctx context.Context, storeID, cartID string) {
	s.emitERPOrderFinalized(ctx, storeID, cartID)
}

// EmitERPOrderCancelled publica o fato group G erp.order_cancelled (best-effort,
// dedup por external order id). Único emissor deste fato hoje.
func (s *Service) EmitERPOrderCancelled(ctx context.Context, storeID, cartID, externalOrderID, reason string) {
	_ = events.EmitInternal(ctx, s.repo.queries, events.ERPOrderCancelled, "erp.order_cancelled:"+externalOrderID, struct {
		StoreID         string `json:"store_id"`
		CartID          string `json:"cart_id"`
		ExternalOrderID string `json:"external_order_id"`
		Provider        string `json:"provider"`
		Reason          string `json:"reason"`
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: s.providerDaLoja(ctx, storeID), Reason: reason})
}
