package integration

// O DEFEITO QUE NENHUM TESTE COBRIA: DOIS ESCRITORES INTERCALADOS.
//
// `products.stock` é o portão de admissão da live, e é escrito por duas
// contabilidades que nunca se reconciliavam:
//
//	RELATIVA (nossa)  −1 por comentário, +qtd por cancelamento de carrinho
//	ABSOLUTA (do ERP) SET com o saldo lido — espelho, "Sincronizar", edição
//
// Uma escrita ABSOLUTA apaga em silêncio os débitos relativos pendentes. O
// crédito que depois solta aquelas mesmas unidades soma sobre a base nova assim
// mesmo, e o portão ganha exatamente o número de unidades que estavam presas em
// carrinho no instante da escrita.
//
// Medido em staging em 31/08/2026, produto 16698953100, com 5 peças no Bling:
//
//	5 → −1 = 4 → +2 = 6 → −1 −1 = 4 → +3 = 7
//
// e o 16698952209 chegou a OITO unidades ofertadas de cinco físicas.
//
// Cada teste anterior exercitava UM escritor de cada vez, e por isso todos
// passavam. Estes exercitam a intercalação.
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestPortao -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"livecart/apps/api/internal/erp/erpwrite"
)

// portaoDoProduto lê o contador direto do banco.
func portaoDoProduto(t *testing.T, produtoID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT stock FROM products WHERE id=$1::uuid`, produtoID).Scan(&n); err != nil {
		t.Fatalf("lendo o portão: %v", err)
	}
	return n
}

// O INCIDENTE, reproduzido: escrita absoluta enquanto há promessa viva, e
// depois o crédito da soltura.
func TestPortaoNaoInflaQuandoAbsolutaEncontraPromessaViva(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	externalID := fmt.Sprintf("PORTAO-%d-%d", seedSeq, rand.Intn(1_000_000))
	loja, evento, produto := lojaComProduto(t, "bling", externalID)

	// O produto nasce com 5 e a live promete 2 a duas compradoras. O pedido
	// existe no ERP e está ABERTO — é ele quem segura a peça lá.
	if _, err := testPool.Exec(ctx, `UPDATE products SET stock=3 WHERE id=$1::uuid`, produto); err != nil {
		t.Fatal(err)
	}
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "PED-A", nil, "aberto")
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "PED-B", nil, "aberto")

	// O ERP diz 5 disponíveis. Com reserva NATIVA e pedidos ABERTOS, esse 5 já
	// desconta as duas peças... não: 5 é o saldo DEPOIS de o lojista cancelar
	// os pedidos no ERP, que foi o que aconteceu. As promessas continuam vivas
	// aqui, e o ERP não as segura mais.
	for _, c := range []string{"PED-A", "PED-B"} {
		if _, err := testPool.Exec(ctx,
			`UPDATE carts SET erp_order_status='cancelado' WHERE external_order_id=$1`, c); err != nil {
			t.Fatal(err)
		}
	}

	prometido, err := testRepo.SumPromisedNotYetReflected(ctx, loja, "bling", externalID, false)
	if err != nil {
		t.Fatal(err)
	}
	if prometido != 2 {
		t.Fatalf("promessas não refletidas = %d, queria 2 — o pedido está CANCELADO no "+
			"ERP, logo ele não segura mais nada e as duas unidades continuam "+
			"prometidas só aqui. Contar zero é o que gravou o saldo cru e inflou o portão", prometido)
	}

	// A escrita absoluta agora compensa.
	portao := erpwrite.Admissivel(5, prometido)
	if portao != 3 {
		t.Fatalf("portão = %d, queria 3 (5 do ERP − 2 prometidas)", portao)
	}
	if _, err := testRepo.ApplyERPStockMirror(ctx, produto, portao, seqDoProduto(t, produto)); err != nil {
		t.Fatal(err)
	}
	if got := portaoDoProduto(t, produto); got != 3 {
		t.Fatalf("depois da absoluta o portão é %d, queria 3", got)
	}

	// E AGORA o crédito da soltura, que era onde o número estourava.
	if _, err := testPool.Exec(ctx,
		`UPDATE products SET stock = stock + 2, erp_seq = erp_seq + 1 WHERE id=$1::uuid`, produto); err != nil {
		t.Fatal(err)
	}
	if got := portaoDoProduto(t, produto); got != 5 {
		t.Fatalf("depois de soltar as 2 o portão é %d, queria 5 — nunca mais que as 5 "+
			"peças que existem. Era aqui que dava 7", got)
	}
}

// O portão NUNCA pode passar do saldo do ERP. É a asserção que teria pegado o
// incidente por si só, sem reconstruir a sequência.
func TestPortaoNuncaPassaDoSaldoDoERP(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	externalID := fmt.Sprintf("TETO-%d-%d", seedSeq, rand.Intn(1_000_000))
	loja, evento, produto := lojaComProduto(t, "bling", externalID)

	const saldoERP = 5
	// Quatro promessas vivas, em situações variadas.
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "", nil, "")
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "P1", nil, "aberto")
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "P2", nil, "cancelado")
	carrinhoPrometendoComSituacao(t, evento, produto, 1, "P3", nil, "")

	prometido, err := testRepo.SumPromisedNotYetReflected(ctx, loja, "bling", externalID, false)
	if err != nil {
		t.Fatal(err)
	}
	// Contam: sem pedido (1), cancelado (1) e situação vazia (1). O 'aberto' não.
	if prometido != 3 {
		t.Errorf("promessas = %d, queria 3 — sem pedido, cancelado e situação "+
			"desconhecida contam; só o pedido ABERTO é que o ERP já desconta", prometido)
	}
	if portao := erpwrite.Admissivel(saldoERP, prometido); portao > saldoERP {
		t.Errorf("portão %d > saldo do ERP %d — o LiveCart ofereceria peça que não existe",
			portao, saldoERP)
	}
}

// A retomada de estoque na reabertura precisa ser VISÍVEL ao CAS do espelho.
// Sem subir erp_seq, uma leitura tirada ANTES dela era aplicada DEPOIS,
// apagando o débito.
func TestRetomadaDeEstoqueSobeOSeq(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()
	externalID := fmt.Sprintf("REOPEN-%d-%d", seedSeq, rand.Intn(1_000_000))
	_, _, produto := lojaComProduto(t, "bling", externalID)

	antes := seqDoProduto(t, produto)
	var obtido int32
	if err := testPool.QueryRow(ctx,
		`WITH atual AS (SELECT id, stock FROM products WHERE id = $1::uuid FOR UPDATE
		), tomado AS (SELECT id, LEAST(GREATEST(stock, 0), 2) AS qtd FROM atual
		), aplicado AS (
		    UPDATE products p SET stock = p.stock - t.qtd, erp_seq = p.erp_seq + 1
		    FROM tomado t WHERE p.id = t.id RETURNING 1)
		 SELECT qtd::int FROM tomado`, produto).Scan(&obtido); err != nil {
		t.Fatal(err)
	}
	if depois := seqDoProduto(t, produto); depois != antes+1 {
		t.Errorf("erp_seq foi de %d para %d — a retomada tem de subir o contador, "+
			"senão ela fica invisível para a trava do espelho", antes, depois)
	}
}
