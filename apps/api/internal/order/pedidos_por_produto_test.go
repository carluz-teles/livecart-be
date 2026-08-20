package order_test

// Rastreabilidade produto → pedidos (pedido do cliente, 20/08/2026): "é
// extremamente difícil rastrear a partir de um produto quais pedidos estão
// com aquele produto".
//
// Duas superfícies, mesmo banco de dados de verdade:
//   1. o filtro productId da listagem (deep-link /orders?product=...) — passa
//      pelo buildOrderListConditions, então vale para lista E contadores;
//   2. o breakdown por status do modal do produto (pedidos + unidades por par
//      status × payment_status).

import (
	"context"
	"testing"

	"livecart/apps/api/internal/order"
)

func TestListagemFiltraPorProduto(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "FiltroProduto")
	vaso := seedProduct(t, storeID, 1000)
	prato := seedProduct(t, storeID, 2000)

	comVaso := insertCart(t, eventID, "ana", "tok-fp-1", 9101, "pending", nil)
	addItem(t, comVaso, vaso, 2, 1000)
	comPrato := insertCart(t, eventID, "bia", "tok-fp-2", 9102, "pending", nil)
	addItem(t, comPrato, prato, 1, 2000)
	// Carrinho com os DOIS produtos: entra no filtro e não pode duplicar linha.
	comAmbos := insertCart(t, eventID, "clara", "tok-fp-3", 9103, "pending", nil)
	addItem(t, comAmbos, vaso, 1, 1000)
	addItem(t, comAmbos, prato, 3, 2000)

	rows := listar(t, storeID, order.OrderFilters{ProductID: &vaso})
	if len(rows) != 2 {
		t.Fatalf("filtro por produto devolveu %d pedidos; esperava 2 (com vaso e com ambos)", len(rows))
	}
	ids := map[string]bool{rows[0].ID: true, rows[1].ID: true}
	if !ids[comVaso] || !ids[comAmbos] {
		t.Errorf("pedidos filtrados = %v; esperava {%s, %s}", ids, comVaso, comAmbos)
	}
	if ids[comPrato] {
		t.Errorf("pedido sem o produto entrou no filtro")
	}
}

func TestBreakdownDoProdutoPorStatus(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "BreakProduto")
	vaso := seedProduct(t, storeID, 1000)
	outro := seedProduct(t, storeID, 500)

	// 2 pedidos pagos (3 unidades), 1 aguardando pagamento (2 un),
	// 1 expirado (1 un) e 1 pedido de OUTRO produto (não pode aparecer).
	pago1 := insertCart(t, eventID, "d1", "tok-bp-1", 9201, "paid", nil)
	addItem(t, pago1, vaso, 1, 1000)
	pago2 := insertCart(t, eventID, "d2", "tok-bp-2", 9202, "paid", nil)
	addItem(t, pago2, vaso, 2, 1000)
	aberto := insertCart(t, eventID, "d3", "tok-bp-3", 9203, "pending", nil)
	addItem(t, aberto, vaso, 2, 1000)
	expirado := insertCart(t, eventID, "d4", "tok-bp-4", 9204, "pending", nil)
	addItem(t, expirado, vaso, 1, 1000)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE carts SET status = 'expired' WHERE id = $1`, expirado); err != nil {
		t.Fatalf("expirando carrinho: %v", err)
	}
	semVaso := insertCart(t, eventID, "d5", "tok-bp-5", 9205, "paid", nil)
	addItem(t, semVaso, outro, 9, 500)

	rows, err := order.NewRepository(testPool).GetProductOrderBreakdown(
		context.Background(), storeID, vaso)
	if err != nil {
		t.Fatalf("GetProductOrderBreakdown: %v", err)
	}

	got := map[string][2]int{} // status|payment -> {pedidos, unidades}
	total := 0
	for _, r := range rows {
		got[r.Status+"|"+r.PaymentStatus] = [2]int{r.Orders, r.Units}
		total += r.Orders
	}
	if total != 4 {
		t.Fatalf("breakdown somou %d pedidos; esperava 4 (o de outro produto ficou de fora)", total)
	}
	// insertCart com paidAt=nil e pagamento "paid" grava status 'active'.
	if v := got["active|paid"]; v != [2]int{2, 3} {
		t.Errorf("pagos = %v; esperava 2 pedidos / 3 unidades", v)
	}
	if v := got["active|pending"]; v != [2]int{1, 2} {
		t.Errorf("aguardando pagamento = %v; esperava 1 pedido / 2 unidades", v)
	}
	if v := got["expired|pending"]; v != [2]int{1, 1} {
		t.Errorf("expirados = %v; esperava 1 pedido / 1 unidade", v)
	}
}
