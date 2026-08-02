package dashboard_test

// Golden de paridade do overview — fecha a RN-14.
//
// As quatro queries de dinheiro do overview.go liam cart_items; passam a ler
// orders/order_items. A fórmula é a mesma (SUM(quantity*unit_price), que é o
// corpo de cart_product_total_cents); o que muda é o MOMENTO: orders congela no
// pagamento, cart_items reflete o carrinho agora.
//
// Por isso o teste tem duas metades:
//   1. paridade — com o carrinho intacto, NOVO == ANTIGO. Prova que o número
//      que o lojista vê hoje não muda.
//   2. divergência — mutando o cart_item DEPOIS do pagamento, o valor antigo
//      escorrega e o novo fica firme. É a razão de ser da mudança.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// keyword4 gera o codigo de 4 caracteres que o comprador digita na live
// (products.keyword e character(4)).
func keyword4() string {
	return fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
}

// shortID devolve um short_id unico dentro do range aceito pela coluna.
func shortID() int64 {
	return time.Now().UnixNano()%90000 + 1000
}

// seedStore cria uma loja descartavel para este arquivo.
func seedStore(t *testing.T) string {
	t.Helper()
	var id string
	slug := fmt.Sprintf("rn14-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug) VALUES ('RN14 Store', $1) RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("seedStore: %v", err)
	}
	return id
}

// seedPaidOrder cria loja/evento/carrinho pago + order espelho e devolve os ids.
func seedPaidOrder(t *testing.T, storeID string, qty int32, unitPrice int64, paidAt time.Time) (cartID, orderID string) {
	t.Helper()
	ctx := context.Background()

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'ended','Overview RN-14') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	handle := fmt.Sprintf("buyer_%d", time.Now().UnixNano())
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at)
		 VALUES ($1,$2,$2,$4,$5,'checkout','paid',$3) RETURNING id::text`,
		eventID, handle, paidAt, "tok_"+handle, shortID(),
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}

	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Produto RN14','none',$4,$2,$3,100) RETURNING id::text`,
		storeID, keyword4(), unitPrice, "rn14-"+handle,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price) VALUES ($1,$2,$3,$4)`,
		cartID, productID, qty, unitPrice,
	); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}

	// O pedido espelha o carrinho no instante do pagamento, como o selamento faz.
	if err := testPool.QueryRow(ctx,
		`INSERT INTO orders (cart_id, short_id, store_id, event_id, status, total_cents, paid_at)
		 VALUES ($1, (SELECT COALESCE(MAX(short_id),0)+1 FROM orders), $2, $3, 'paid',
		         cart_product_total_cents($1::uuid), $4)
		 RETURNING id::text`,
		cartID, storeID, eventID, paidAt,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price)
		 SELECT $1, ci.product_id, 'Produto RN14', ci.quantity, ci.unit_price
		 FROM cart_items ci WHERE ci.cart_id = $2::uuid`,
		orderID, cartID,
	); err != nil {
		t.Fatalf("seed order_items: %v", err)
	}
	return cartID, orderID
}

func TestOverviewRevenueParityWithOrders(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := newRepo(t)

	storeID := seedStore(t)
	paidAt := time.Now().Add(-2 * time.Hour)
	from := paidAt.Add(-24 * time.Hour)
	to := paidAt.Add(24 * time.Hour)

	// 2 unidades a R$ 25,00 = R$ 50,00
	cartID, _ := seedPaidOrder(t, storeID, 2, 2500, paidAt)

	const wantCents = int64(5000)

	ov, err := repo.GetOverviewRange(ctx, storeID, from, to)
	if err != nil {
		t.Fatalf("GetOverviewRange: %v", err)
	}
	if ov.GMVCents != wantCents {
		t.Errorf("gmv_cents = %d, quero %d", ov.GMVCents, wantCents)
	}
	if ov.PaidOrders != 1 {
		t.Errorf("paid_orders = %d, quero 1", ov.PaidOrders)
	}
	if ov.AverageTicket != wantCents {
		t.Errorf("ticket medio = %d, quero %d", ov.AverageTicket, wantCents)
	}

	series, err := repo.GetRevenueSeriesRange(ctx, storeID, from, to, "day")
	if err != nil {
		t.Fatalf("GetRevenueSeriesRange: %v", err)
	}
	var seriesTotal int64
	for _, p := range series {
		seriesTotal += p.Revenue
	}
	if seriesTotal != wantCents {
		t.Errorf("serie soma %d, quero %d", seriesTotal, wantCents)
	}

	tops, err := repo.GetTopProductsRange(ctx, storeID, from, to)
	if err != nil {
		t.Fatalf("GetTopProductsRange: %v", err)
	}
	if len(tops) != 1 || tops[0].TotalRevenue != wantCents || tops[0].TotalSold != 2 {
		t.Errorf("top produtos = %+v, quero 1 item com 2un e %d", tops, wantCents)
	}

	buyers, err := repo.GetTopBuyersRange(ctx, storeID, from, to)
	if err != nil {
		t.Fatalf("GetTopBuyersRange: %v", err)
	}
	if len(buyers) != 1 || buyers[0].TotalSpent != wantCents {
		t.Errorf("top compradores = %+v, quero 1 com %d", buyers, wantCents)
	}

	// --- a metade que justifica a mudança -----------------------------------
	// Mutar o item DEPOIS do pagamento. O pedido é imutável, então o número
	// confirmado tem de continuar o mesmo; a leitura antiga (cart_items) teria
	// escorregado para 7500.
	if _, err := testPool.Exec(ctx,
		`UPDATE cart_items SET quantity = 3 WHERE cart_id = $1::uuid`, cartID,
	); err != nil {
		t.Fatalf("mutar cart_item: %v", err)
	}

	var legacy int64
	if err := testPool.QueryRow(ctx,
		`SELECT cart_product_total_cents($1::uuid)`, cartID,
	).Scan(&legacy); err != nil {
		t.Fatalf("recalcular do cart: %v", err)
	}
	if legacy != 7500 {
		t.Fatalf("o recalculo do cart devia dar 7500 depois da mutacao, deu %d", legacy)
	}

	ov2, err := repo.GetOverviewRange(ctx, storeID, from, to)
	if err != nil {
		t.Fatalf("GetOverviewRange apos mutacao: %v", err)
	}
	if ov2.GMVCents != wantCents {
		t.Errorf("gmv_cents apos mutar o carrinho = %d, quero %d (congelado no pedido)", ov2.GMVCents, wantCents)
	}
	if ov2.GMVCents == legacy {
		t.Error("gmv_cents seguiu o carrinho mutado — ainda esta lendo cart_items")
	}
}

// Carrinho pago SEM pedido espelho não deve entrar na receita confirmada: é
// exatamente o caso que o backfill da 000103 existiu para cobrir.
func TestOverviewIgnoresPaidCartWithoutOrder(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := newRepo(t)

	storeID := seedStore(t)
	paidAt := time.Now().Add(-time.Hour)
	from := paidAt.Add(-24 * time.Hour)
	to := paidAt.Add(24 * time.Hour)

	var eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'ended','sem pedido') RETURNING id::text`,
		storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		   status, payment_status, paid_at)
		 VALUES ($1,'orfao','orfao',$3,$4,'checkout','paid',$2) RETURNING id::text`,
		eventID, paidAt, fmt.Sprintf("tok_orfao_%d", time.Now().UnixNano()), shortID(),
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Orfao','none',$3,$2,9900,100) RETURNING id::text`,
		storeID, keyword4(), fmt.Sprintf("orfao-%d", time.Now().UnixNano()),
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price) VALUES ($1,$2,1,9900)`,
		cartID, productID,
	); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}

	ov, err := repo.GetOverviewRange(ctx, storeID, from, to)
	if err != nil {
		t.Fatalf("GetOverviewRange: %v", err)
	}
	if ov.GMVCents != 0 {
		t.Errorf("gmv_cents = %d, quero 0 — carrinho pago sem pedido nao e receita confirmada", ov.GMVCents)
	}
	// O funil, esse sim, continua contando o carrinho.
	if ov.PaidCarts != 1 {
		t.Errorf("paid_carts = %d, quero 1 — o funil continua lendo carts", ov.PaidCarts)
	}
}
