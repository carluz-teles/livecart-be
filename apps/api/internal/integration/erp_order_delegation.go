package integration

// Bloco B2c-1 — a máquina de estados Design C (pedido-como-reserva) foi extraída
// para internal/erp (erp/order_lifecycle.go). Este arquivo mantém em
// internal/integration:
//
//   - os aliases in-package do estado/sentinela ainda usados pela finalização
//     LEGADA e pelos reactors (B2c-2), sem churnar ~30 call sites;
//   - as DELEGAÇÕES finas dos métodos públicos (assinaturas inalteradas) — os
//     call sites em checkout/main.go seguem chamando integration.Service por ora
//     (a troca para erp.Service é B2e);
//   - o bridge finalizeOrConfirmCartERP (confirm Design C → fallback legado);
//   - os COLABORADORES que erp.Service chama de volta (provider/contato/criação
//     de pedido/estorno/mirror/eventos), satisfazendo erp.StockCollaborators.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
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

// finalizeOrConfirmCartERP é a entrada única do caminho pago: tenta o confirm
// do pedido-como-reserva (2 PUTs, zero estoque, em erp.Service) e cai na
// finalização LEGADA (ainda aqui, B2c-2) quando o cart não foi convertido.
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

// =============================================================================
// COLABORADORES (satisfazem erp.StockCollaborators — Bloco B2c-1)
// =============================================================================

// OrderAtCheckoutEnabled reporta se a loja opera no modo pedido-como-reserva
// (design C). Reusa o flag por loja da Fase 3 (ERP_ORDER_AT_CHECKOUT_STORE_IDS,
// "*" = todas) parseado em NewService.
func (s *Service) OrderAtCheckoutEnabled(storeID string) bool {
	return s.orderAtCheckoutAll || s.orderAtCheckoutStoreIDs[storeID]
}

// ResolveERPContact converte a Integration neutra e reusa o helper legado
// resolveERPContact (find/create + enriquecimento do contato Tiny).
func (s *Service) ResolveERPContact(ctx context.Context, provider providers.ERPProvider, integration *erp.Integration, storeID, platformUserID, platformHandle, name, document, email, phone string) (string, error) {
	return s.resolveERPContact(ctx, provider, integrationRowFromERP(integration), storeID, platformUserID, platformHandle, name, document, email, phone)
}

// CreateFinalERPOrderForConversion carrega o cart e reusa createFinalERPOrder
// para criar o pedido SEM pagamento e SEM launch (a conversão do design C
// orquestra o launch depois). Grava external_order_id no cart no sucesso.
func (s *Service) CreateFinalERPOrderForConversion(ctx context.Context, provider providers.ERPProvider, integration *erp.Integration, storeID, cartID string) error {
	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return fmt.Errorf("loading cart for conversion: %w", err)
	}
	return s.createFinalERPOrder(ctx, provider, integrationRowFromERP(integration), storeID, cart.EventID, *cart, nil, false)
}

// ReverseCartReservationsPerRow reusa o helper legado de estorno per-row das
// saídas manuais do cart.
func (s *Service) ReverseCartReservationsPerRow(ctx context.Context, provider providers.ERPProvider, cartID string) error {
	return s.reverseCartReservationsPerRow(ctx, provider, cartID)
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
	}{StoreID: storeID, CartID: cartID, ExternalOrderID: externalOrderID, Provider: "tiny", Reason: reason})
}
