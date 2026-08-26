package erp

// O que já foi pago e o que ainda falta, separados no pedido.
//
// Acrescentar item a um pedido PAGO é uma operação legítima — o lojista faz isso
// o tempo todo: a compradora pagou na live de segunda, pediu mais uma coisa na
// quinta, e ele soma no mesmo pedido para sair um frete só. O ERP permite.
//
// O que ele NÃO faz é preservar o registro do dinheiro. Medido em 26/08/2026:
//
//	venda de R$ 40, uma parcela paga        → parcela: R$ 40  "PIX LAB"
//	acrescenta R$ 105 em itens              → parcela: R$ 145 "PIX LAB"   ← reescrita
//	acrescenta mais R$ 50                   → duas parcelas de R$ 97,50   ← redistribuída
//
// A cada mudança de item o total é redistribuído pelas parcelas existentes, com
// HTTP 204 e sem aviso. O pedido passa a afirmar que a compradora pagou o valor
// novo. E a soma das parcelas é FORÇADA ao total: mandar R$ 60 num pedido de
// R$ 100 grava R$ 100.
//
// Por isso a divisão não é um enfeite de relatório — é a única forma de o pedido
// continuar dizendo a verdade. Reenviá-la é obrigatório depois de toda mudança de
// item num pedido pago, e o lugar dela é o próprio bloco de parcelas do ERP, onde
// o lojista já olha.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// prazoDoSaldo é o vencimento que a parcela em aberto recebe. Uma semana: tempo
// de a compradora receber o link e pagar, sem virar dívida esquecida no ERP.
const prazoDoSaldo = 7 * 24 * time.Hour

// SplitDePagamento é o retrato do dinheiro de um pedido.
type SplitDePagamento struct {
	TotalCents int64
	PagoCents  int64
	SaldoCents int64
	Reescrito  bool
	Motivo     string
}

// RecomporParcelasDoPedidoPago devolve ao pedido a verdade sobre o dinheiro:
// uma parcela com o que a compradora pagou, outra com o que falta.
//
// Só age em carrinho PAGO com pedido. Não faz nada quando o total do pedido já é
// o valor pago — que é o caso normal, e o mais comum.
//
// Recusa-se a escrever quando o pedido ficou MENOR que o valor pago. Aí existe
// crédito a devolver, o ERP não consegue registrar parcelas que somem mais que o
// total, e qualquer número que gravássemos seria mentira. Sobe como erro para
// alguém decidir.
func (s *Service) RecomporParcelasDoPedidoPago(ctx context.Context, cartID, storeID string) (*SplitDePagamento, error) {
	st, err := s.repo.GetCartERPOrderState(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("loading cart ERP order state: %w", err)
	}
	if st.State != OrderStateConfirmed || st.ExternalOrderID == "" {
		return nil, nil // não é venda fechada; não há pagamento a separar
	}

	// Quanto entrou, somando todos os pagamentos do carrinho. Zero significa que
	// o dinheiro ainda não foi contabilizado do lado de cá — e inventar um valor
	// é pior do que deixar o pedido como está.
	pago, quando := st.PaidAmountCents, st.PaidAt
	if pago <= 0 {
		return nil, nil
	}
	if quando.IsZero() {
		quando = time.Now()
	}

	erpProvider, err := s.providerFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	contador, ok := erpProvider.(interface {
		GetOrderTotal(ctx context.Context, orderID string) (int64, bool, error)
		SetOrderInstallments(ctx context.Context, orderID string, parcelas []providers.ERPInstallment) error
	})
	if !ok {
		return nil, nil
	}

	total, _, err := contador.GetOrderTotal(ctx, st.ExternalOrderID)
	if err != nil {
		return nil, fmt.Errorf("reading order total: %w", err)
	}

	split := &SplitDePagamento{TotalCents: total, PagoCents: pago, SaldoCents: total - pago}

	switch {
	case total == pago:
		return split, nil // o pedido vale o que ela pagou; nada a separar
	case total < pago:
		split.Motivo = "o pedido ficou menor que o valor pago — há crédito a devolver"
		logger.From(ctx, s.logger).Error("paid order is now worth less than what was paid; installments left untouched",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", st.ExternalOrderID),
			zap.Int64("paid_cents", pago),
			zap.Int64("order_total_cents", total),
		)
		return split, nil
	}

	parcelas := []providers.ERPInstallment{
		{
			AmountCents: pago,
			DueDate:     quando,
			Note:        fmt.Sprintf("PAGO em %s", quando.In(time.FixedZone("BRT", -3*60*60)).Format("02/01/2006")),
		},
		{
			AmountCents: total - pago,
			DueDate:     time.Now().Add(prazoDoSaldo),
			Note:        "A PAGAR - itens acrescentados depois do pagamento",
		},
	}
	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return contador.SetOrderInstallments(ctx, st.ExternalOrderID, parcelas)
	}); err != nil {
		return split, fmt.Errorf("rewriting installments on the paid order: %w", err)
	}

	split.Reescrito = true
	logger.From(ctx, s.logger).Info("paid order installments split into paid and outstanding",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
		zap.Int64("paid_cents", pago),
		zap.Int64("outstanding_cents", total-pago),
	)
	return split, nil
}
