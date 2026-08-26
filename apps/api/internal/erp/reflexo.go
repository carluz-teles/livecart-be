package erp

// O caminho de volta: o pedido do ERP refletido no carrinho.
//
// Até aqui a informação só andava num sentido — o comentário virava item, o item
// virava linha do pedido. Mas o pedido é um documento de venda com DOIS donos: o
// lojista abre o painel e acrescenta o que a compradora pediu por DM, corrige uma
// quantidade, tira uma linha. Quando ele faz isso, o carrinho precisa seguir,
// porque é o carrinho que ela vê e paga.
//
// O gatilho é empurrado, não sondado: o ERP dispara `atualizacao_pedido` também
// quando SÓ os itens mudam — medido em 26/08/2026, um PUT de quantidades às
// 20:14:21 produziu o webhook às 20:14:24, com a situação inalterada em "aberto".
//
// Duas regras que definem o resultado:
//
//  1. O PEDIDO manda na grade. Ele é o documento de venda; o carrinho é a
//     projeção dele para a compradora. Divergiu, o pedido vence.
//
//  2. O reflexo NÃO mexe no contador local de estoque. Ele é espelho do
//     `disponivel`, e o `disponivel` já contou a mudança do lojista no instante
//     em que ela entrou no pedido. Descontar aqui também contaria duas vezes.

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// CartSyncCollaborators é o que o reflexo precisa do lado de fora: resolver e
// importar produto, e mexer nos itens do carrinho.
type CartSyncCollaborators interface {
	// ResolveLocalProduct devolve o id local do produto a partir do id no ERP.
	// found=false quando a loja nunca importou aquele produto.
	ResolveLocalProduct(ctx context.Context, storeID, externalProductID string) (productID string, found bool, err error)
	// ImportProductFromERP cadastra na loja um produto que só existia no ERP,
	// como se o lojista o tivesse importado pela tela.
	ImportProductFromERP(ctx context.Context, storeID, externalProductID string) (productID string, err error)
	// SetCartItemQuantity ajusta a quantidade de um item do carrinho.
	SetCartItemQuantity(ctx context.Context, cartID, productID string, quantity int, unitPrice int64) error
	// RemoveCartItem tira o item do carrinho (só quando não há fila).
	RemoveCartItem(ctx context.Context, cartID, productID string) error
}

// SetCartSyncCollaborators liga o reflexo.
func (s *Service) SetCartSyncCollaborators(c CartSyncCollaborators) { s.cartSync = c }

// CartSyncChange é uma diferença encontrada entre o pedido e o carrinho.
type CartSyncChange struct {
	ExternalProductID string
	ProductID         string
	Kind              string // "added" | "quantity" | "removed" | "imported"
	From              int
	To                int
}

// CartSyncReport é o resultado de um reflexo.
type CartSyncReport struct {
	CartID   string
	Skipped  string
	Changes  []CartSyncChange
	Imported int
}

// SyncCartFromERPOrder traz para o carrinho o que o pedido diz hoje.
//
// Roda sob o mesmo CAS open→mutating das escritas: se uma mutação nossa está em
// voo, o reflexo desiste — ela vai reenviar a grade do banco e o pedido resultante
// é o que o próximo reflexo vai ler. Sem isso os dois se atropelariam, cada um
// desfazendo o outro.
//
// Carrinho pago, cancelado ou vencido não é tocado: a grade daquela venda já está
// fechada, e mexer nela mudaria o que a compradora pagou.
func (s *Service) SyncCartFromERPOrder(ctx context.Context, cartID, storeID string) (*CartSyncReport, error) {
	if s.cartSync == nil {
		return nil, nil
	}
	rel := &CartSyncReport{CartID: cartID}

	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != OrderStateOpen || st.ExternalOrderID == "" {
		rel.Skipped = "carrinho não está aberto com pedido (estado " + st.State + ")"
		return rel, nil
	}

	won, err := s.repo.TransitionCartERPOrderState(ctx, cartID, OrderStateOpen, OrderStateMutating)
	if err != nil {
		return nil, fmt.Errorf("claiming cart for reflection: %w", err)
	}
	if !won {
		// Uma escrita nossa está em voo. Ela mandará a grade do banco, e o
		// próximo reflexo lê o resultado — desistir aqui é o certo.
		rel.Skipped = "escrita em voo neste pedido"
		return rel, nil
	}
	defer func() {
		fim := context.WithoutCancel(ctx)
		if _, backErr := s.repo.TransitionCartERPOrderState(fim, cartID, OrderStateMutating, OrderStateOpen); backErr != nil {
			logger.From(fim, s.logger).Error("failed to return cart to open after reflection",
				zap.String("cart_id", cartID), zap.Error(backErr))
		}
		s.collab.MirrorToOrder(fim, cartID)
	}()

	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	doPedido, err := erpProvider.GetOrderItems(ctx, st.ExternalOrderID)
	if err != nil {
		return nil, fmt.Errorf("reading order items for reflection: %w", err)
	}

	doCarrinho, err := s.repo.ListNonWaitlistedCartItems(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("listing cart items for reflection: %w", err)
	}
	noCarrinho := make(map[string]NonWaitlistedCartItem, len(doCarrinho))
	for _, it := range doCarrinho {
		if it.ProductExternalID != "" {
			noCarrinho[it.ProductExternalID] = it
		}
	}

	vistos := make(map[string]bool, len(doPedido))
	for _, linha := range doPedido {
		vistos[linha.ProductID] = true
		atual, existe := noCarrinho[linha.ProductID]

		if existe && atual.Quantity == linha.Quantity {
			continue
		}

		productID := atual.ProductID
		if !existe {
			// Produto que a loja nunca importou: cadastra agora, como se o
			// lojista o tivesse trazido pela tela. Sem isso o item ficaria no
			// pedido e invisível no carrinho — a compradora pagaria por algo que
			// ela não vê.
			id, achado, resErr := s.cartSync.ResolveLocalProduct(ctx, storeID, linha.ProductID)
			if resErr != nil {
				return rel, fmt.Errorf("resolving product %s for reflection: %w", linha.ProductID, resErr)
			}
			if !achado {
				novo, impErr := s.cartSync.ImportProductFromERP(ctx, storeID, linha.ProductID)
				if impErr != nil {
					// Um produto que não dá para importar não pode derrubar o
					// reflexo inteiro: as outras linhas ainda valem.
					logger.From(ctx, s.logger).Error("could not import a product the merchant added to the order",
						zap.String("cart_id", cartID),
						zap.String("external_product_id", linha.ProductID),
						zap.Error(impErr))
					continue
				}
				id = novo
				rel.Imported++
				rel.Changes = append(rel.Changes, CartSyncChange{
					ExternalProductID: linha.ProductID, ProductID: id, Kind: "imported",
				})
			}
			productID = id
		}

		de := 0
		if existe {
			de = atual.Quantity
		}
		if err := s.cartSync.SetCartItemQuantity(ctx, cartID, productID, linha.Quantity, linha.UnitPrice); err != nil {
			return rel, fmt.Errorf("setting cart item from order: %w", err)
		}
		kind := "quantity"
		if !existe {
			kind = "added"
		}
		rel.Changes = append(rel.Changes, CartSyncChange{
			ExternalProductID: linha.ProductID, ProductID: productID,
			Kind: kind, From: de, To: linha.Quantity,
		})
	}

	// Linha que sumiu do pedido sai do carrinho. É o lojista removendo o item —
	// e o carrinho não pode continuar cobrando por ele.
	for ext, it := range noCarrinho {
		if vistos[ext] {
			continue
		}
		if err := s.cartSync.RemoveCartItem(ctx, cartID, it.ProductID); err != nil {
			return rel, fmt.Errorf("removing cart item the merchant deleted: %w", err)
		}
		rel.Changes = append(rel.Changes, CartSyncChange{
			ExternalProductID: ext, ProductID: it.ProductID,
			Kind: "removed", From: it.Quantity, To: 0,
		})
	}

	if len(rel.Changes) > 0 {
		logger.From(ctx, s.logger).Info("cart updated from the ERP order",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", st.ExternalOrderID),
			zap.Int("changes", len(rel.Changes)),
			zap.Int("products_imported", rel.Imported),
		)
	}
	return rel, nil
}

var _ = providers.ERPOrderItem{}
