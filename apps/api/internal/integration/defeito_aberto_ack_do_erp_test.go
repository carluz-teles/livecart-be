package integration

// DEFEITO ABERTO — o espelho do ERP ainda repõe unidade vendida.
//
// A trava de sequência fecha um lado da corrida e deixa o outro aberto. Os dois
// se parecem, e a diferença é qual evento o contador marca:
//
//	erp_seq marca quando o movimento entra no NOSSO contador.
//	O saldo do Tiny só muda quando o delta chega LÁ.
//
// Entre um e outro existe a chamada HTTP inteira — 200 a 500ms medidos em
// produção. Uma leitura do ERP tirada nesse intervalo descreve o estado
// ANTERIOR a uma saída que o nosso contador já registrou, e a trava aprova
// mesmo assim: ela só verifica se o seq mudou DESDE a leitura, e nesse caso o
// movimento é mais velho que a leitura.
//
// O resultado é a unidade do comprador voltando para o estoque. É a direção
// cara: oferece peça que não existe, promove a fila em cima de vaga fantasma e
// confirma venda de produto esgotado.
//
// Por que não está corrigido aqui: a correção é um segundo contador, marcando
// quando o ERP CONFIRMOU o movimento (`erp_ack_seq`), e o espelho passando a
// exigir `erp_ack_seq = erp_seq` — "o ERP já sabe de tudo que eu sei". O
// desenho está pronto e é exato, mas exige que TODO movimento que sobe o seq
// receba exatamente um ack. Hoje são 15 chamadas em 5 pacotes (inventory, erp,
// live, integration, checkout), e um ack faltante trava o espelho daquele SKU
// em silêncio — cego para o lojista vendendo em outro canal, que é o defeito
// que a trava foi criada para não recriar. Parear os 15 é mexer em checkout e
// live, e isso é decisão de quando, não de se.
//
// Enquanto não é corrigido: a exposição é proporcional à concorrência no MESMO
// SKU durante a janela HTTP. Com estoque folgado e ritmo normal de live ela é
// rara; numa disputa apertada, não.
//
// Para reproduzir, remova o t.Skip.

import (
	"context"
	"testing"

	"livecart/apps/api/db/sqlc"
)

const defeitoAberto = "DEFEITO ABERTO: falta o contador de confirmação do ERP (erp_ack_seq). " +
	"Ver o comentário no topo de defeito_aberto_ack_do_erp_test.go"

// A prova determinística. Nenhuma concorrência, nenhum tempo: só a ordem.
func TestDefeitoEspelhoAplicaLeituraAnteriorAoNossoMovimento(t *testing.T) {
	t.Skip(defeitoAberto)

	requireDB(t)
	ctx := context.Background()

	fx := seedScaleEvent(t)
	const inicial = 10
	productID := seedSoldOutProductWithQueue(t, fx, inicial, 0)
	repo := NewRepository(sqlc.New(testPool), testPool)

	// 1. Um comprador leva 1 unidade. O seq sobe AGORA, no mesmo comando que
	//    baixa o estoque. O delta para o Tiny ainda não saiu.
	if err := repo.DecrementProductStock(ctx, productID, 1); err != nil {
		t.Fatalf("reserva: %v", err)
	}

	// 2. Um webhook do Tiny chega — venda do lojista em outro canal, por
	//    exemplo. Lemos o comprovante.
	seq := seqDoProduto(t, productID)

	// 3. Consultamos o Tiny, que ainda NÃO recebeu a nossa saída: responde 10.
	saldoDoTiny := inicial

	// 4. A trava compara o seq: nada mudou desde o passo 2, então ela aprova —
	//    e grava 10 por cima de um estoque que já é 9.
	if _, err := repo.ApplyERPStockMirror(ctx, productID, saldoDoTiny, seq); err != nil {
		t.Fatalf("espelho: %v", err)
	}

	if got := estoqueDoProduto(t, productID); got != inicial-1 {
		t.Errorf("estoque = %d, quero %d: a unidade do comprador VOLTOU. O seq já "+
			"contava o movimento, mas o Tiny ainda não sabia dele — e a trava só "+
			"pergunta se algo mudou DESDE a leitura, não se o ERP está em dia",
			got, inicial-1)
	}
}
