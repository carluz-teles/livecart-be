package integration

// O carrinho que o ERP trouxe de volta, contra o Postgres real.
//
// O lojista cancela pelo LiveCart, nós cancelamos no Tiny, e depois ele reabre
// o pedido LÁ, à mão. Aconteceu em staging em 27/08/2026. Seguir isso é o que
// evita o pior desfecho: pedido vivo no ERP reservando peça, carrinho morto
// aqui, unidade sumida do disponível até alguém reparar.
//
// O caso que decide o desenho é o do estoque que foi embora no meio-tempo.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type carrinhoCancelado struct {
	cartID     string
	eventID    string
	storeID    string
	produto    string
	quantidade int
}

func semearCarrinhoCancelado(t *testing.T, estoqueRestante int, qtd int) carrinhoCancelado {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())
	var c carrinhoCancelado
	must := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("semeando: %v", err)
		}
	}
	must(&c.storeID, `INSERT INTO stores (name, slug) VALUES ('Loja Reabre','reabre-'||$1) RETURNING id::text`, n)
	must(&c.eventID, `INSERT INTO live_events (store_id, status, title, ends_at)
		VALUES ($1,'active','Live Reabre',now()+interval '7 days') RETURNING id::text`, c.storeID)
	must(&c.produto, `INSERT INTO products (store_id, name, keyword, external_source, price, stock)
		VALUES ($1,'Produto','reab','tiny',2000,$2) RETURNING id::text`, c.storeID, estoqueRestante)
	must(&c.cartID, `INSERT INTO carts (event_id, store_id, platform_user_id, platform_handle, token, short_id,
	                                    status, cancelled_reason, external_order_id, erp_order_state, erp_order_status)
		VALUES ($1,$2,'u-'||$3,'@maria'||$3,'tok-'||$3,($3)::bigint % 100000,
		        'cancelled','store_cancelled','TINY-REAB','cancelled','aberto') RETURNING id::text`,
		c.eventID, c.storeID, n)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price) VALUES ($1,$2,$3,2000)`,
		c.cartID, c.produto, qtd); err != nil {
		t.Fatalf("semeando item: %v", err)
	}
	c.quantidade = qtd
	return c
}

func (c carrinhoCancelado) estado(t *testing.T) (status, motivo string, revertidoEm *time.Time, estoque int) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT c.status, COALESCE(c.cancellation_reverted_reason,''), c.cancellation_reverted_at,
		        (SELECT stock FROM products WHERE id=$2)
		 FROM carts c WHERE c.id=$1`, c.cartID, c.produto).Scan(&status, &motivo, &revertidoEm, &estoque); err != nil {
		t.Fatalf("lendo estado: %v", err)
	}
	return
}

// Estoque inteiro disponível: o carrinho volta completo, nada vai para a fila.
func TestReaberturaComEstoqueTrazOCarrinhoInteiro(t *testing.T) {
	requireDB(t)
	c := semearCarrinhoCancelado(t, 10, 3)

	rel, err := testRepo.ReopenCancelledCartFromERP(context.Background(), c.cartID, c.storeID)
	if err != nil {
		t.Fatalf("reabrindo: %v", err)
	}
	if !rel.Reopened {
		t.Fatal("não reabriu um carrinho cancelado pelo lojista")
	}
	if rel.Recuperadas != 3 || rel.EmFila != 0 {
		t.Errorf("recuperadas=%d fila=%d, quero 3 e 0", rel.Recuperadas, rel.EmFila)
	}
	status, motivo, revertido, estoque := c.estado(t)
	if status != "checkout" {
		t.Errorf("status=%q, quero checkout — o link tem de voltar a funcionar", status)
	}
	if motivo != "erp_reopened" {
		t.Errorf("motivo=%q, quero erp_reopened — é o que distingue da corrida do pagamento", motivo)
	}
	if revertido == nil {
		t.Error("não carimbou quando o cancelamento foi desfeito")
	}
	if estoque != 7 {
		t.Errorf("estoque=%d, quero 7 (10 - 3 retomadas)", estoque)
	}
}

// O CASO QUE DECIDE O DESENHO: outra compradora levou parte da peça no
// meio-tempo. O carrinho volta com o que há e o resto espera — recusar tudo por
// causa de uma unidade jogaria fora o carrinho inteiro.
func TestReaberturaSemEstoqueMandaODiferencaParaAFila(t *testing.T) {
	requireDB(t)
	c := semearCarrinhoCancelado(t, 1, 3) // pediu 3, sobrou 1

	rel, err := testRepo.ReopenCancelledCartFromERP(context.Background(), c.cartID, c.storeID)
	if err != nil {
		t.Fatalf("reabrindo: %v", err)
	}
	if !rel.Reopened {
		t.Fatal("desistiu do carrinho inteiro por falta de estoque")
	}
	if rel.Recuperadas != 1 {
		t.Errorf("recuperadas=%d, quero 1 — o que havia", rel.Recuperadas)
	}
	if rel.EmFila != 2 {
		t.Errorf("fila=%d, quero 2 — o que outra compradora levou", rel.EmFila)
	}

	var fila int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(quantity),0)::int FROM waitlist_items
		 WHERE cart_id=$1 AND status='waiting'`, c.cartID).Scan(&fila); err != nil {
		t.Fatalf("lendo a fila: %v", err)
	}
	if fila != 2 {
		t.Errorf("na fila de espera = %d, quero 2 — sem isso a compradora nunca é "+
			"promovida quando a peça voltar", fila)
	}

	var waitlisted int
	if err := testPool.QueryRow(context.Background(),
		`SELECT waitlisted_quantity FROM cart_items WHERE cart_id=$1`, c.cartID).Scan(&waitlisted); err != nil {
		t.Fatalf("lendo o item: %v", err)
	}
	if waitlisted != 2 {
		t.Errorf("waitlisted_quantity=%d, quero 2 — é o que o checkout usa para "+
			"não cobrar peça que não existe", waitlisted)
	}
	if _, _, _, estoque := c.estado(t); estoque != 0 {
		t.Errorf("estoque=%d, quero 0 — levou tudo que havia", estoque)
	}
}

// Estoque zerado: o carrinho volta inteiro em fila. Continua valendo a pena —
// o pedido no ERP para de ser órfão e a compradora entra na fila de verdade.
func TestReaberturaSemNadaEmEstoqueVoltaTodoEmFila(t *testing.T) {
	requireDB(t)
	c := semearCarrinhoCancelado(t, 0, 2)

	rel, err := testRepo.ReopenCancelledCartFromERP(context.Background(), c.cartID, c.storeID)
	if err != nil {
		t.Fatalf("reabrindo: %v", err)
	}
	if !rel.Reopened || rel.Recuperadas != 0 || rel.EmFila != 2 {
		t.Errorf("reaberto=%v recuperadas=%d fila=%d, quero true/0/2",
			rel.Reopened, rel.Recuperadas, rel.EmFila)
	}
}

// Carrinho VENCIDO não ressuscita. Prazo vencido não é engano, é regra — e um
// clique no ERP não passa por cima dela.
func TestCarrinhoVencidoNaoRessuscita(t *testing.T) {
	requireDB(t)
	c := semearCarrinhoCancelado(t, 10, 1)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET status='expired', cancelled_reason=NULL WHERE id=$1`, c.cartID); err != nil {
		t.Fatalf("vencendo: %v", err)
	}

	rel, err := testRepo.ReopenCancelledCartFromERP(context.Background(), c.cartID, c.storeID)
	if err != nil {
		t.Fatalf("reabrindo: %v", err)
	}
	if rel.Reopened {
		t.Error("ressuscitou um carrinho VENCIDO — prazo vencido é regra, não engano")
	}
	if _, _, _, estoque := c.estado(t); estoque != 10 {
		t.Errorf("mexeu no estoque (%d) de um carrinho que não devia reabrir", estoque)
	}
}

// Repetir a reabertura não retoma estoque duas vezes: o guard de status já
// recusa, e é isso que torna o webhook reentregável.
func TestReaberturaRepetidaNaoRetomaEstoqueDuasVezes(t *testing.T) {
	requireDB(t)
	c := semearCarrinhoCancelado(t, 10, 3)
	ctx := context.Background()

	if _, err := testRepo.ReopenCancelledCartFromERP(ctx, c.cartID, c.storeID); err != nil {
		t.Fatalf("1ª reabertura: %v", err)
	}
	rel2, err := testRepo.ReopenCancelledCartFromERP(ctx, c.cartID, c.storeID)
	if err != nil {
		t.Fatalf("2ª reabertura: %v", err)
	}
	if rel2.Reopened {
		t.Error("reabriu de novo um carrinho que já estava aberto")
	}
	if _, _, _, estoque := c.estado(t); estoque != 7 {
		t.Errorf("estoque=%d, quero 7 — a reentrega do webhook não pode debitar "+
			"duas vezes", estoque)
	}
}
