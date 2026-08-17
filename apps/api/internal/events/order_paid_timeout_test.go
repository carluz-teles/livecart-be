package events

// O `order.paid` rodava com o teto da fila normal: 15 segundos.
//
// Dentro desse orçamento ele precisa, em série e passando pelo limitador do
// Tiny: resolver o contato, buscar a dimensão de CADA item para o frete,
// resolver forma de pagamento, de recebimento e de envio, criar o pedido,
// aprová-lo (chamada separada) e lançar o estoque. Um pedido de 1 item fecha
// em 2-3s; os de 5 e 7 itens não fecharam.
//
// O ponto em que ele estoura é o que dói: quando o prazo acaba depois do POST
// ter chegado ao Tiny, o pedido nasce lá e a resposta se perde. A aprovação,
// que vem depois, nunca roda — o pedido fica "Em aberto" em vez de "Aprovado",
// e as retentativas recebem 409. Foram 3 pedidos pagos assim em 16/08.

import (
	"testing"
	"time"
)

func TestOrderPaidTemTetoMaiorQueODaFila(t *testing.T) {
	daFila := DefaultPolicies[QueueNormal].Timeout

	doEvento, ok := EventTimeouts[OrderPaid]
	if !ok {
		t.Fatalf("order.paid não tem teto próprio e herda os %s da fila — "+
			"não cabe finalizar um pedido de vários itens nesse tempo", daFila)
	}
	if doEvento <= daFila {
		t.Errorf("teto de order.paid é %s, menor ou igual ao da fila (%s); "+
			"o override não muda nada", doEvento, daFila)
	}
}

// O estorno percorre o mesmo caminho de ERP e tem o mesmo problema de orçamento.
func TestOrderRefundedAcompanhaOMesmoTeto(t *testing.T) {
	if _, ok := EventTimeouts[OrderRefunded]; !ok {
		t.Error("order.refunded ficou com o teto da fila; ele faz o mesmo trabalho de ERP que order.paid")
	}
}

// Teto generoso demais também é defeito: a task fica segurando um worker da
// fila normal, e a fila só tem 2 slots de concorrência (server.go). Uma janela
// larga o bastante para um pedido grande, e curta o bastante para não travar a
// fila, é o que se procura aqui.
func TestTetoNaoEhLargoAPontoDeTravarAFila(t *testing.T) {
	const limite = 5 * time.Minute

	for nome, teto := range EventTimeouts {
		if teto > limite {
			t.Errorf("teto de %s é %s, acima de %s — uma task presa nesse tempo "+
				"segura um dos 2 workers da fila normal", nome, teto, limite)
		}
	}
}
