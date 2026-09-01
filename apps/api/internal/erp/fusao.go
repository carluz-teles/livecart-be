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

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// ERPOrderMerge é um pedido que perdeu o carrinho para a fusão.
type ERPOrderMerge struct {
	SourceCartID    string
	ExternalOrderID string
	// SemDinheiro afirma que esta origem NÃO tem cobrança registrada, e por isso
	// pode ser solta mesmo se o pedido dela já estiver 'confirmed'.
	//
	// Quem chama tem de ter lido isso ANTES de vincular os carrinhos: depois do
	// vínculo, o livro de pagamentos da origem já responde pelo grupo do
	// anfitrião, e uma releitura aqui viria vazia para qualquer origem — o que
	// faria toda fusão parecer segura.
	//
	// Falso é o padrão e o valor conservador: a origem vai pelo cancelamento
	// comum, que recusa 'confirmed' e protege o pedido pago.
	SemDinheiro bool
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
		if err := s.soltarPedidoDaFusao(ctx, o, storeID); err != nil {
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

// soltarPedidoDaFusao solta a origem pelo caminho que o estado dela permite.
//
// O cancelamento comum não sai de 'confirmed' — é a guarda que impede um estorno
// acidental, e ela fica. Mas um pedido confirmado SEM cobrança nenhuma não tem
// estorno para acontecer: soltá-lo é arrumação, e recusá-lo deixava a fusão sem
// saída (a junção do painel travava com os dois lados intocáveis, staging 28/08).
//
// Sem a afirmação explícita de que não há dinheiro, vai pelo caminho comum.
func (s *Service) soltarPedidoDaFusao(ctx context.Context, o ERPOrderMerge, storeID string) error {
	if o.ExternalOrderID == "" {
		return nil
	}

	// ═══ O CARRINHO JÁ NÃO RESPONDE PELO PRÓPRIO PEDIDO ═══
	//
	// Quando isto roda, o vínculo da junção JÁ está gravado — e
	// GetCartERPOrderState resolve um carrinho juntado para o ANFITRIÃO, de
	// propósito, para que as escritas seguintes caiam no pedido certo
	// (`COALESCE(orig.joined_to_cart_id, orig.id)`).
	//
	// Consequência: qualquer caminho que cancele "o pedido DO CARRINHO" de
	// origem cancela, na verdade, o pedido do anfitrião — justamente o que tem
	// de SOBREVIVER, porque acabou de receber a grade somada dos dois.
	//
	// Medido em produção 01/09/2026, junção do #1349 no #1252: a intenção
	// registrada era soltar o 848241852 (origem) e o cancelamento saiu no
	// 848127017 (anfitrião). O anfitrião levou os 4 itens e morreu; o webhook
	// do Tiny voltou 'cancelado', o LiveCart cancelou o carrinho, e a
	// compradora terminou sem pedido nenhum.
	//
	// A defesa é comparar: se o carrinho resolve para um pedido DIFERENTE do
	// que nos mandaram soltar, ele está juntado e o caminho por carrinho está
	// proibido.
	st, err := s.repo.GetCartERPOrderState(ctx, o.SourceCartID)
	if err != nil {
		return fmt.Errorf("lendo o estado do pedido de origem: %w", err)
	}
	if st == nil || st.ExternalOrderID != o.ExternalOrderID {
		return s.cancelarPedidoAvulsoNoERP(ctx, storeID, o.SourceCartID, o.ExternalOrderID)
	}

	if o.SemDinheiro && st.State == OrderStateConfirmed {
		return s.soltarPedidoConfirmado(ctx, o.SourceCartID, storeID, "join")
	}
	return s.CancelERPOrderForCart(ctx, o.SourceCartID, storeID)
}

// cancelarPedidoAvulsoNoERP cancela um pedido pelo ID, sem passar pela máquina
// de estados do carrinho.
//
// É o caminho do pedido ÓRFÃO: o carrinho de origem já foi desligado dele (a
// junção zera external_order_id e põe o estado em 'none'), então não há
// transição a fazer — e tentar fazê-la leria o pedido do anfitrião.
func (s *Service) cancelarPedidoAvulsoNoERP(ctx context.Context, storeID, cartID, orderID string) error {
	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return err
	}
	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return erpProvider.SetOrderSituacao(ctx, orderID, providers.SituacaoCancelada)
	}); err != nil {
		return fmt.Errorf("cancelling order %s: %w", orderID, err)
	}
	s.collab.EmitERPOrderCancelled(ctx, storeID, cartID, orderID, "join")
	logger.From(ctx, s.logger).Info("pedido de origem da junção cancelado pelo id",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", orderID),
	)
	return nil
}
