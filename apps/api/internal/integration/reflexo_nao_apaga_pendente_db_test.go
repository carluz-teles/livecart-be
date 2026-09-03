package integration

// O REFLEXO NÃO PODE APAGAR O QUE NÓS NÃO CONSEGUIMOS ESCREVER.
//
// "Não está no pedido do ERP" tem DUAS causas, e elas pedem o oposto uma da
// outra:
//
//	o lojista removeu       → apagar do carrinho é o certo
//	a nossa escrita falhou  → apagar destrói uma compra já confirmada
//
// Produção, 01/09/2026, @dany.lifestyle: comentou 2091 às 19:19:06 UTC, entrou
// no carrinho, recebeu a DM "Novo item adicionado: Pote com Tampa Pinha –
// 11cm". A escrita no Tiny morreu no 429 daquela live. No dia seguinte alguém
// editou o pedido lá, o reflexo rodou — "changes=8" — e apagou a linha achando
// que o lojista a tinha removido.
//
// Nenhuma mutação registrou a remoção. A compradora tinha a prova; a loja não
// tinha o item; ninguém tinha o rastro.
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestReflexo -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

func semearCarrinhoComItem(t *testing.T) (cartID, productID string) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))

	var storeID, eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Pendente','pend-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'live','Live', now() + interval '2 hours') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Pote com Tampa Pinha','2091','tiny',9190,5,true) RETURNING id::text`, storeID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1,'u-dany','@dany.lifestyle','tok-'||$2,(floor(random()*90000)+10000)::int,'active','pending')
		 RETURNING id::text`, eventID, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1,$2,1,9190,0)`, cartID, productID); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}
	return cartID, productID
}

func itensNoCarrinho(t *testing.T, cartID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM cart_items WHERE cart_id = $1::uuid`, cartID).Scan(&n); err != nil {
		t.Fatalf("contando itens: %v", err)
	}
	return n
}

func TestReflexoApagaLinhaQueOLojistaRemoveu(t *testing.T) {
	requireDB(t)
	cartID, productID := semearCarrinhoComItem(t)
	svc := resilienceTestService(t)

	// Sem marca de pendente: o ERP conhecia a linha e ela sumiu de lá por mão
	// humana. Apagar é o comportamento certo — e precisa continuar existindo.
	if err := svc.RemoveCartItem(context.Background(), cartID, productID); err != nil {
		t.Fatalf("RemoveCartItem: %v", err)
	}
	if n := itensNoCarrinho(t, cartID); n != 0 {
		t.Errorf("itens = %d, queria 0 — o lojista removeu do pedido e o carrinho "+
			"não pode seguir cobrando", n)
	}
}

func TestReflexoPreservaLinhaQueNaoConseguimosEscrever(t *testing.T) {
	requireDB(t)
	cartID, productID := semearCarrinhoComItem(t)
	svc := resilienceTestService(t)
	ctx := context.Background()

	// A escrita falhou — é o caso da @dany.lifestyle.
	if err := testRepo.MarcarItemPendenteNoERP(ctx, cartID, productID); err != nil {
		t.Fatalf("MarcarItemPendenteNoERP: %v", err)
	}

	if err := svc.RemoveCartItem(ctx, cartID, productID); err != nil {
		t.Fatalf("RemoveCartItem: %v", err)
	}
	if n := itensNoCarrinho(t, cartID); n != 1 {
		t.Fatalf("itens = %d, queria 1 — o reflexo apagou uma compra que a "+
			"compradora já teve confirmada por DM", n)
	}

	// E quando a escrita finalmente chega, a proteção sai junto: o reflexo volta
	// a mandar naquela linha, senão o item do lojista nunca mais poderia ser
	// removido.
	if err := testRepo.ConfirmarItemNoERP(ctx, cartID, productID); err != nil {
		t.Fatalf("ConfirmarItemNoERP: %v", err)
	}
	if err := svc.RemoveCartItem(ctx, cartID, productID); err != nil {
		t.Fatalf("RemoveCartItem após confirmar: %v", err)
	}
	if n := itensNoCarrinho(t, cartID); n != 0 {
		t.Errorf("itens = %d, queria 0 — confirmada, a linha volta a obedecer o ERP", n)
	}
}

// A marca guarda a PRIMEIRA falha. Se cada tentativa a reescrevesse, "há quanto
// tempo isto está pendente" mentiria, e a varredura trataria uma linha presa há
// horas como recém-falhada.
func TestMarcaDePendenteGuardaAPrimeiraFalha(t *testing.T) {
	requireDB(t)
	cartID, productID := semearCarrinhoComItem(t)
	ctx := context.Background()

	if err := testRepo.MarcarItemPendenteNoERP(ctx, cartID, productID); err != nil {
		t.Fatal(err)
	}
	var primeira string
	if err := testPool.QueryRow(ctx,
		`SELECT erp_pending_since::text FROM cart_items WHERE cart_id=$1::uuid`, cartID,
	).Scan(&primeira); err != nil {
		t.Fatal(err)
	}
	if err := testRepo.MarcarItemPendenteNoERP(ctx, cartID, productID); err != nil {
		t.Fatal(err)
	}
	var depois string
	if err := testPool.QueryRow(ctx,
		`SELECT erp_pending_since::text FROM cart_items WHERE cart_id=$1::uuid`, cartID,
	).Scan(&depois); err != nil {
		t.Fatal(err)
	}
	if primeira != depois {
		t.Errorf("a marca andou (%s → %s) — a idade da pendência é o que decide "+
			"a urgência do reenvio", primeira, depois)
	}
}
