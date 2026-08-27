package erp

// Fundir carrinhos é fundir os pedidos.
//
// Quando um comprador vira VIP, os carrinhos abertos que ele tem em eventos
// diferentes viram um só — é o que "carrinho eterno que acumula entre eventos"
// sempre significou. Do lado de cá isso é mover itens entre linhas de tabela.
// Do lado do ERP, cada um daqueles carrinhos tem um pedido de venda segurando
// peça, e esvaziar o carrinho não conta isso a ninguém: o pedido antigo fica lá,
// reservando unidades que agora pertencem a outro pedido. A mesma peça contada
// duas vezes, para sempre.
//
// ═══ A ORDEM ═══
//
// Primeiro o pedido de destino CRESCE, depois o de origem é CANCELADO.
//
// Invertido — cancelar antes — existe um instante em que ninguém segura aquelas
// unidades. Numa live isso é tempo de sobra para outra compradora levá-las, e o
// comprador que acabou de ser promovido a VIP perde o que já tinha no carrinho.
//
// Na ordem certa as unidades ficam reservadas DUAS vezes por um segundo, e o
// disponível do ERP fica negativo nesse intervalo. Já foi medido que ele aceita
// (chegou a −4 sem reclamar) e que cancelar devolve tudo, inclusive o excesso.
// Reservar demais por um segundo é recuperável; vender a peça de outra pessoa,
// não.

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// ERPOrderMerge é um pedido que perdeu o carrinho para a fusão.
type ERPOrderMerge struct {
	SourceCartID    string
	ExternalOrderID string
}

// MergeReport diz o que aconteceu com cada pedido órfão.
type MergeReport struct {
	DestCartID string
	// Released são os pedidos de origem cancelados: a reserva deles voltou ao
	// ERP e as unidades agora estão no pedido do carrinho eterno.
	Released []string
	// Stuck são os que continuam segurando peça. Cada um é peça contada duas
	// vezes até alguém resolver, e por isso sobe como erro, não como aviso.
	Stuck []string
}

// MergeERPOrdersIntoCart faz o pedido do carrinho eterno absorver o que os
// outros seguravam, e só então solta os outros.
//
// Roda DEPOIS do commit da consolidação: os itens já estão no carrinho de
// destino, então a grade reconstruída do banco já é a soma. Não recebe a lista
// de itens de propósito — quem manda na grade é sempre o banco.
func (s *Service) MergeERPOrdersIntoCart(ctx context.Context, destCartID, storeID string, orfaos []ERPOrderMerge) (*MergeReport, error) {
	rel := &MergeReport{DestCartID: destCartID}
	if len(orfaos) == 0 {
		return rel, nil
	}

	// 1. O destino cresce. Enquanto isto não valer, nada é solto.
	if err := s.EnsureERPOrderForCart(ctx, destCartID, storeID); err != nil {
		return rel, fmt.Errorf("garantindo o pedido do carrinho eterno antes de soltar os outros: %w", err)
	}
	if err := s.MutateERPOrderItems(ctx, destCartID, storeID); err != nil {
		// Sem o destino crescido, cancelar os outros deixaria a compra do
		// comprador sem reserva nenhuma. Sai daqui sem soltar nada: os pedidos
		// antigos continuam segurando as peças dele, que é o estado seguro.
		if errors.Is(err, ErrPedidoFaturado) {
			return rel, fmt.Errorf("o carrinho eterno já tem pedido faturado e não recebe os itens dos outros: %w", err)
		}
		return rel, fmt.Errorf("crescendo o pedido do carrinho eterno: %w", err)
	}

	// 2. Agora sim: cada pedido de origem é cancelado, e a reserva volta.
	//
	// O laço não para no primeiro erro. Cada pedido é independente, e desistir
	// no primeiro deixaria os seguintes segurando peça sem ninguém sabendo.
	for _, o := range orfaos {
		if err := s.CancelERPOrderForCart(ctx, o.SourceCartID, storeID); err != nil {
			rel.Stuck = append(rel.Stuck, o.ExternalOrderID)
			logger.From(ctx, s.logger).Error("merged cart's ERP order could not be released; it is still holding stock",
				zap.String("dest_cart_id", destCartID),
				zap.String("source_cart_id", o.SourceCartID),
				zap.String("external_order_id", o.ExternalOrderID),
				zap.Error(err),
			)
			continue
		}
		rel.Released = append(rel.Released, o.ExternalOrderID)
	}

	logger.From(ctx, s.logger).Info("vip promotion merged the ERP orders",
		zap.String("dest_cart_id", destCartID),
		zap.Int("released", len(rel.Released)),
		zap.Int("still_holding_stock", len(rel.Stuck)),
	)
	if len(rel.Stuck) > 0 {
		return rel, fmt.Errorf("%d pedido(s) do ERP continuam reservando peça depois da fusão: %v",
			len(rel.Stuck), rel.Stuck)
	}
	return rel, nil
}
