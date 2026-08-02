package dashboard_test

// As queries que a ROTA de fato serve — não as homônimas do sqlc.
//
// dashboard.sql tinha a versão corrigida de GetTopProducts (paid-only, sobre
// order_items) e ela era testada; só que o handler nunca a chamou. Quem
// respondia /dashboard/top-products era o SQL inline de repository.go, ainda
// somando cart_items de qualquer carrinho. Dois arquivos, dois comportamentos, e
// o teste apontado para o que não estava no ar.
//
// Estes testes exercitam o Repository — o caminho que o lojista vê.

import (
	"context"
	"testing"
)

// mutaCarrinhoPago acrescenta uma unidade ao carrinho JÁ PAGO da seed e devolve
// quantos centavos isso somaria a quem recalculasse dos cart_items. É o único
// cenário em que "congelado" e "recalculado" divergem, e é o que separa ler
// orders de re-somar o carrinho.
func mutaCarrinhoPago(t *testing.T, seed f5Seed) int64 {
	t.Helper()
	var extra int64
	if err := testPool.QueryRow(context.Background(),
		`UPDATE cart_items ci
		 SET quantity = ci.quantity + 1
		 FROM carts c JOIN orders o ON o.cart_id = c.id
		 WHERE ci.cart_id = c.id AND ci.product_id = $2::uuid
		   AND o.store_id = $1::uuid AND o.status = 'paid'
		 RETURNING ci.unit_price`,
		seed.storeID, seed.prodBID,
	).Scan(&extra); err != nil {
		t.Fatalf("mutar carrinho pago: %v", err)
	}
	return extra
}

func TestServedTopProductsReadsOrderItems(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)
	repo := newRepo(t)

	rows, err := repo.GetTopProducts(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetTopProducts: %v", err)
	}

	got := map[string]int64{}
	gotQty := map[string]int{}
	for _, r := range rows {
		got[r.ID] = r.TotalRevenue
		gotQty[r.ID] = r.TotalSold
	}

	if got[seed.prodAID] != seed.paidRevA {
		t.Errorf("prodA revenue = %d, quero %d (só o pago)", got[seed.prodAID], seed.paidRevA)
	}
	if gotQty[seed.prodAID] != int(seed.paidQtyA) {
		t.Errorf("prodA qty = %d, quero %d", gotQty[seed.prodAID], seed.paidQtyA)
	}
	// O carrinho não-pago tem 5 unidades a mais de prodA. Se ele vazar, o
	// "mais vendido" da loja vira ficção.
	if got[seed.prodAID] == seed.paidRevA+seed.unpaidExtraRevA {
		t.Errorf("carrinho não-pago vazou para top-products: %d", got[seed.prodAID])
	}
	// E a order pending_payment também não conta.
	if gotQty[seed.prodAID] > int(seed.paidQtyA) {
		t.Errorf("order não-paga contada: qty = %d", gotQty[seed.prodAID])
	}
}

func TestServedProductSalesReadsOrderItems(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)
	repo := newRepo(t)

	out, err := repo.GetProductSales(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetProductSales: %v", err)
	}

	var total int64
	for _, d := range out.Data {
		for _, v := range d.Values {
			total += v
		}
	}
	want := seed.paidRevA + seed.paidRevB
	if total != want {
		t.Errorf("receita do gráfico = %d, quero %d (só pedidos pagos)", total, want)
	}
	if total == want+seed.unpaidExtraRevA {
		t.Errorf("carrinho não-pago vazou para product-sales: %d", total)
	}
}

func TestServedTopBuyersReadsFrozenTotal(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)
	repo := newRepo(t)

	// O carrinho pago ganha uma unidade DEPOIS do pagamento (edição pelo
	// painel). O pedido não muda; uma re-soma dos cart_items mudaria.
	extra := mutaCarrinhoPago(t, seed)

	buyers, err := repo.GetTopBuyers(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetTopBuyers: %v", err)
	}
	if len(buyers) != 1 {
		t.Fatalf("compradores = %d, quero 1 (só quem pagou)", len(buyers))
	}

	want := seed.paidRevA + seed.paidRevB
	if buyers[0].TotalSpent != want {
		t.Errorf("total gasto = %d, quero %d (o congelado do pedido)", buyers[0].TotalSpent, want)
	}
	if buyers[0].TotalSpent == want+extra {
		t.Errorf("total gasto foi recalculado dos cart_items: %d", buyers[0].TotalSpent)
	}
}

func TestServedRevenueByPaymentReadsFrozenTotal(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)
	repo := newRepo(t)

	extra := mutaCarrinhoPago(t, seed)

	rows, err := repo.GetRevenueByPaymentMethod(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetRevenueByPaymentMethod: %v", err)
	}

	var total int64
	for _, r := range rows {
		total += r.Revenue
	}
	want := seed.paidRevA + seed.paidRevB
	if total != want {
		t.Errorf("receita por método = %d, quero %d", total, want)
	}
	if total == want+extra {
		t.Errorf("receita por método foi recalculada dos cart_items: %d", total)
	}
}

func TestServedStatsAndTopProductsAgree(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	seed := seedF5(t)
	repo := newRepo(t)

	stats, err := repo.GetStats(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	rows, err := repo.GetTopProducts(ctx, seed.storeID)
	if err != nil {
		t.Fatalf("GetTopProducts: %v", err)
	}
	var somaProdutos int64
	for _, r := range rows {
		somaProdutos += r.TotalRevenue
	}

	// Com um único pedido pago na loja, o faturamento total e a soma dos
	// produtos vendidos são o mesmo número. Se as duas rotas lerem fontes
	// diferentes, isso deixa de valer sem ninguém perceber.
	if stats.TotalRevenue != somaProdutos {
		t.Errorf("faturamento (%d) != soma dos produtos vendidos (%d)", stats.TotalRevenue, somaProdutos)
	}
}
