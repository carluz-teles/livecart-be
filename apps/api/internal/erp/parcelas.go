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
//
// ═══ O DESCONTO É UMA PARCELA ═══
//
// Cupom e desconto de PIX fazem o dinheiro que entra ser MENOR que o preço cheio
// das unidades. E o total do pedido no ERP é o preço cheio: `valorDesconto` é
// gravável só na criação — medido em 26/08/2026, `PUT /pedidos` com
// valorDesconto devolve 204 e ignora o campo, e uma linha com valor negativo é
// recusada com "Este valor deve ser maior que 0". Como o pedido nasce no
// primeiro comentário, muito antes de existir cupom ou forma de pagamento, não
// há instante em que esse campo pudesse ser preenchido com a verdade.
//
// A saída não é contornar a regra de que as parcelas somam o total — é usá-la.
// O desconto vira uma parcela sua, nomeada:
//
//	pedido de R$ 100, PIX com 5% de desconto, R$ 95 recebidos
//	→ [ R$ 95 "PAGO PIX 26/08" ] + [ R$ 5 "DESCONTO (cupom/PIX)" ]        = 100
//
//	depois a compradora acrescenta R$ 45 em itens
//	→ [ R$ 95 "PAGO" ] + [ R$ 5 "DESCONTO" ] + [ R$ 45 "A PAGAR" ]        = 145
//
//	e paga o saldo em outra cobrança
//	→ [ R$ 95 "PAGO" ] + [ R$ 45 "PAGO" ] + [ R$ 5 "DESCONTO" ]           = 145
//
// Sem essa parcela, o desconto apareceria como saldo devedor e o pedido cobraria
// da compradora um valor que ela nunca deveu.

import (
	"context"
	"fmt"
	"strings"
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
	// TotalCents é o preço CHEIO do pedido no ERP — sem desconto, porque o ERP
	// não aceita descontar depois da criação.
	TotalCents int64
	// PagoCents é o que entrou de fato, somando todas as cobranças.
	PagoCents int64
	// DescontoCents é o que foi abatido: a diferença entre o preço cheio das
	// unidades cobertas e o dinheiro que entrou por elas.
	DescontoCents int64
	// SaldoCents é o que ainda falta pagar. Negativo significa crédito a
	// devolver, e nesse caso nada é escrito no ERP.
	SaldoCents int64
	// Pagamentos é em quantas cobranças o dinheiro entrou.
	Pagamentos int
	Reescrito  bool
	Motivo     string
}

// CartPayment é uma cobrança do livro de pagamentos do carrinho.
type CartPayment struct {
	AmountCents int64
	// GrossCoveredCents é quanto de preço cheio esta cobrança liquidou, frete
	// incluído. A diferença para AmountCents é o desconto daquela cobrança.
	GrossCoveredCents int64
	Method            string
	CheckoutID        string
	PaidAt            time.Time
}

// CartPaymentLedger é o livro de pagamentos. Opcional de propósito: repositório
// que não o implementa simplesmente não tem a divisão, em vez de quebrar.
type CartPaymentLedger interface {
	ListCartPayments(ctx context.Context, cartID string) ([]CartPayment, error)
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

	pagamentos, err := s.pagamentosDoCarrinho(ctx, cartID)
	if err != nil {
		return nil, err
	}
	if len(pagamentos) == 0 {
		// Sem cobrança registrada não dá para afirmar quanto entrou, e inventar
		// é pior do que deixar o pedido como está.
		return nil, nil
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

	var pago, bruto int64
	for _, p := range pagamentos {
		pago += p.AmountCents
		bruto += p.GrossCoveredCents
	}
	desconto := bruto - pago
	if desconto < 0 {
		// Entrou MAIS do que o preço cheio do que foi coberto. Não é desconto,
		// é uma conta que não fecha — e gravar isso como parcela espalharia o
		// erro para dentro do ERP.
		desconto = 0
	}

	split := &SplitDePagamento{
		TotalCents:    total,
		PagoCents:     pago,
		DescontoCents: desconto,
		SaldoCents:    total - pago - desconto,
		Pagamentos:    len(pagamentos),
	}

	switch {
	case split.SaldoCents == 0 && len(pagamentos) == 1 && desconto == 0:
		// Um pagamento, sem desconto, cobrindo o pedido inteiro: a parcela que o
		// ERP já tem diz exatamente isso. Reescrevê-la gastaria uma escrita do
		// teto de 30/min para não mudar nada.
		return split, nil
	case split.SaldoCents < 0:
		split.Motivo = "o pedido ficou menor que o valor pago — há crédito a devolver"
		logger.From(ctx, s.logger).Error("paid order is now worth less than what was paid; installments left untouched",
			zap.String("cart_id", cartID),
			zap.String("external_order_id", st.ExternalOrderID),
			zap.Int64("paid_cents", pago),
			zap.Int64("discount_cents", desconto),
			zap.Int64("order_total_cents", total),
		)
		return split, nil
	}

	parcelas := extratoDeParcelas(pagamentos, desconto, split.SaldoCents)
	if err := s.escreverNoERP(ctx, storeID, cartID, func(ctx context.Context) error {
		return contador.SetOrderInstallments(ctx, st.ExternalOrderID, parcelas)
	}); err != nil {
		return split, fmt.Errorf("rewriting installments on the paid order: %w", err)
	}

	split.Reescrito = true
	logger.From(ctx, s.logger).Info("paid order installments rebuilt from the payment ledger",
		zap.String("cart_id", cartID),
		zap.String("external_order_id", st.ExternalOrderID),
		zap.Int("payments", len(pagamentos)),
		zap.Int64("paid_cents", pago),
		zap.Int64("discount_cents", desconto),
		zap.Int64("outstanding_cents", split.SaldoCents),
	)
	return split, nil
}

// montarParcelas escreve o extrato: uma parcela por cobrança, uma para o
// desconto concedido e uma para o que falta. A soma fecha com o total do pedido
// por construção — e tem de fechar, porque o ERP substitui em silêncio qualquer
// divisão que não feche.
func extratoDeParcelas(pagamentos []CartPayment, desconto, saldo int64) []providers.ERPInstallment {
	parcelas := make([]providers.ERPInstallment, 0, len(pagamentos)+2)
	for _, p := range pagamentos {
		parcelas = append(parcelas, providers.ERPInstallment{
			AmountCents: p.AmountCents,
			DueDate:     p.PaidAt,
			Note:        notaDePagamento(p),
		})
	}
	if desconto > 0 {
		// Sem esta linha o desconto viraria saldo devedor, e o pedido cobraria
		// da compradora um valor que ela nunca deveu.
		parcelas = append(parcelas, providers.ERPInstallment{
			AmountCents: desconto,
			DueDate:     pagamentos[len(pagamentos)-1].PaidAt,
			Note:        "DESCONTO concedido (cupom/PIX) - nao cobrar",
		})
	}
	if saldo > 0 {
		parcelas = append(parcelas, providers.ERPInstallment{
			AmountCents: saldo,
			DueDate:     time.Now().Add(prazoDoSaldo),
			Note:        "A PAGAR - itens acrescentados depois do pagamento",
		})
	}
	return parcelas
}

func notaDePagamento(p CartPayment) string {
	quando := p.PaidAt.In(brasilia).Format("02/01/2006")
	if p.Method != "" {
		return fmt.Sprintf("PAGO %s em %s", strings.ToUpper(p.Method), quando)
	}
	return "PAGO em " + quando
}

var brasilia = time.FixedZone("BRT", -3*60*60)

// pagamentosDoCarrinho lê o livro. Sem ele — repositório antigo, loja que ainda
// não migrou — a divisão simplesmente não acontece, que é melhor do que
// adivinhar em quantas vezes o dinheiro entrou.
func (s *Service) pagamentosDoCarrinho(ctx context.Context, cartID string) ([]CartPayment, error) {
	livro, ok := s.repo.(CartPaymentLedger)
	if !ok {
		return nil, nil
	}
	pagamentos, err := livro.ListCartPayments(ctx, cartID)
	if err != nil {
		return nil, fmt.Errorf("reading the cart payment ledger: %w", err)
	}
	return pagamentos, nil
}
