package integration

// Colaboradores do reflexo ERP → carrinho.
//
// O pedido é um documento de venda com dois donos. Quando o lojista mexe nele
// pelo painel, o carrinho tem de seguir — e se o produto que ele acrescentou
// nunca foi importado, a loja passa a tê-lo, como se ele o tivesse trazido pela
// tela. Ver erp/reflexo.go.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// ResolveLocalProduct devolve o id local do produto a partir do id no ERP.
func (s *Service) ResolveLocalProduct(ctx context.Context, storeID, externalProductID string) (string, bool, error) {
	// GetProductIDByExternalID já devolve "" sem erro quando não existe.
	id, err := s.repo.GetProductIDByExternalID(ctx, storeID, "tiny", externalProductID)
	if err != nil {
		return "", false, err
	}
	return id, id != "", nil
}

// ImportProductFromERP cadastra na loja um produto que só existia no ERP.
//
// Reusa exatamente o caminho da importação manual — mesmo ImportProduct que a
// tela chama. O produto entra sem keyword: ele não foi anunciado na live, e
// inventar um código de quatro dígitos criaria um gatilho que ninguém combinou.
func (s *Service) ImportProductFromERP(ctx context.Context, storeID, externalProductID string) (string, error) {
	if s.productSyncer == nil {
		return "", fmt.Errorf("importação de produto não está ligada neste processo")
	}
	integration, err := s.repo.GetActiveByProvider(ctx, storeID, "erp", "tiny")
	if err != nil {
		return "", fmt.Errorf("loading ERP integration: %w", err)
	}
	provider, err := s.erpProviderFor(ctx, integration)
	if err != nil {
		return "", fmt.Errorf("creating ERP provider: %w", err)
	}
	detailed, err := provider.GetProduct(ctx, externalProductID)
	if err != nil {
		return "", fmt.Errorf("fetching product %s from ERP: %w", externalProductID, err)
	}
	return s.productSyncer.ImportProduct(ctx, storeID, integration.Provider, *detailed)
}

// SetCartItemQuantity ajusta a quantidade de um item do carrinho.
func (s *Service) SetCartItemQuantity(ctx context.Context, cartID, productID string, quantity int, unitPrice int64) error {
	cart, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	prod, err := parseUUID(productID)
	if err != nil {
		return err
	}
	return s.repo.queries.SetCartItemQuantityFromERP(ctx, sqlc.SetCartItemQuantityFromERPParams{
		CartID:    cart,
		ProductID: prod,
		Quantity:  pgtype.Int4{Int32: int32(quantity), Valid: true},
		UnitPrice: pgtype.Int8{Int64: unitPrice, Valid: true},
	})
}

// RemoveCartItem tira o item do carrinho.
func (s *Service) RemoveCartItem(ctx context.Context, cartID, productID string) error {
	cart, err := parseUUID(cartID)
	if err != nil {
		return err
	}
	prod, err := parseUUID(productID)
	if err != nil {
		return err
	}
	return s.repo.queries.RemoveCartItemFromERP(ctx, sqlc.RemoveCartItemFromERPParams{
		CartID:    cart,
		ProductID: prod,
	})
}

// reflexoDePedido coalesce os reflexos por pedido.
var reflexoDePedido = novoCoalescedor()

// CoalescerReflexo devolve o coalescedor dos reflexos.
func (s *Service) CoalescerReflexo() *coalescedor { return reflexoDePedido }

// CartIDByExternalOrder resolve o carrinho a partir do id do pedido no ERP.
// Devolve "" quando o pedido não é de nenhum carrinho da loja.
func (s *Service) CartIDByExternalOrder(ctx context.Context, storeID, externalOrderID string) (string, error) {
	id, err := s.repo.FindCartByExternalOrderID(ctx, externalOrderID, storeID)
	if err != nil {
		return "", nil // inclusive ErrNoRows: pedido de outro canal
	}
	return id, nil
}
