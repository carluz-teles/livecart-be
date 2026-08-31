package order

// ESTORNO É FATO, NÃO CAMPO.
//
// O botão "Marcar como reembolsado" escrevia carts.payment_status e parava aí.
// O lojista viu o sintoma: o pedido estornado aparecia em "Precisam atenção" em
// vez de "Cancelado". Mas o carrinho preso era só o primeiro de seis efeitos que
// não aconteciam — a Order seguia paga, o pedido no ERP seguia segurando peça, o
// cupom seguia consumido, o e-mail não saía e a comissão não voltava.

import (
	"context"
	"testing"
)

type pagamentoEspiao struct {
	estornou string
	pagou    string
	erro     error
}

func (p *pagamentoEspiao) ConfirmManualPayment(_ context.Context, cartID, _ string) error {
	p.pagou = cartID
	return p.erro
}

func (p *pagamentoEspiao) ConfirmManualRefund(_ context.Context, cartID, _ string) error {
	p.estornou = cartID
	return p.erro
}

func TestEstornoPassaPeloCasoDeUsoQueEmiteOFato(t *testing.T) {
	espiao := &pagamentoEspiao{}
	s := &Service{manualPayment: espiao}

	refunded := "refunded"
	err := s.aplicarTransicaoDePagamento(context.Background(), UpdateOrderInput{
		ID: "cart-1", StoreID: "loja-1", PaymentStatus: &refunded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if espiao.estornou != "cart-1" {
		t.Error("o estorno não passou pelo caso de uso — a coluna seria escrita direto " +
			"e o carrinho ficaria preso em 'Precisam atenção' para sempre")
	}
}

func TestPagamentoManualTambemPassaPeloCasoDeUso(t *testing.T) {
	espiao := &pagamentoEspiao{}
	s := &Service{manualPayment: espiao}

	paid := "paid"
	if err := s.aplicarTransicaoDePagamento(context.Background(), UpdateOrderInput{
		ID: "cart-2", StoreID: "loja-1", PaymentStatus: &paid,
	}); err != nil {
		t.Fatal(err)
	}
	if espiao.pagou != "cart-2" {
		t.Error("marcar como pago por este caminho não emitia fato — a venda não " +
			"chegaria ao ERP nem ao relatório")
	}
}

// Os outros status continuam sendo escrita de campo: um carrinho voltando a
// 'pending' depois de uma falha não é fato nenhum — nada aconteceu com dinheiro.
func TestStatusSemFatoContinuaSendoEscritaDeCampo(t *testing.T) {
	espiao := &pagamentoEspiao{}
	s := &Service{manualPayment: espiao, repo: nil}

	for _, st := range []string{"pending", "failed", "cancelled"} {
		func() {
			defer func() {
				// repo nil: chegar no repositório é o que se quer provar, e o
				// pânico é a evidência de que NÃO passou pelo caso de uso.
				_ = recover()
			}()
			status := st
			_ = s.aplicarTransicaoDePagamento(context.Background(), UpdateOrderInput{
				ID: "cart-3", StoreID: "loja-1", PaymentStatus: &status,
			})
		}()
		if espiao.estornou != "" || espiao.pagou != "" {
			t.Errorf("status %q virou fato de pagamento — só 'paid' e 'refunded' são", st)
		}
	}
}
