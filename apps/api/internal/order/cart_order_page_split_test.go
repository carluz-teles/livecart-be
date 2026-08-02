package order_test

// Gate de cobertura da tela de Pedidos.
//
// A Fatia 7 dividiu a leitura em duas páginas — Pedidos (só carts COM order) e
// Carrinhos (só carts SEM order) — mas a página "Carrinhos" nunca foi construída
// no frontend, e as abas de Pedidos ("Aguardando pagamento", "Cancelados"),
// escritas em maio, sempre falaram de carrinho em aberto. Em campo isso apareceu
// como pedido gerado pela live que não existia em aba nenhuma do painel
// (staging 29/07/2026: 16 carrinhos, 0 visíveis). A lista voltou a ser ancorada
// no CART, com a Order como enriquecimento.
//
// Invariantes agora:
//   G1 Pedidos (repo.List) == TODOS os carts da loja, pagos ou não
//   G2 total_amount vem de orders.total_cents quando há Order (valor congelado)
//   G3 total_amount vem do cart quando ainda não há Order (nunca some inline —
//      usa cart_product_total_cents)
//   G4 is_first_purchase só é verdadeiro para venda concretizada

import (
	"context"
	"fmt"
	"testing"
	"time"

	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/query"
)

// ─── helpers ────────────────────────────────────────────────────────────────

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// seedIsolatedStore cria loja + evento isolados para os testes G*.
func seedIsolatedStore(t *testing.T, tag string) (storeID, eventID string) {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ($1, $2) RETURNING id::text`,
		tag+" Store", tag+"-"+n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seedIsolatedStore store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1, 'ended', $2, now()) RETURNING id::text`,
		storeID, tag+" Ev",
	).Scan(&eventID); err != nil {
		t.Fatalf("seedIsolatedStore event: %v", err)
	}
	return
}

// seedProduct cria um produto na loja. Keyword é CHAR(4): usa 4 dígitos (1000-8999).
func seedProduct(t *testing.T, storeID string, price int64) string {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()
	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000) // 4-digit keyword
	var id string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'P-'||$2, 'none', 'e-'||$2, $3, $4, 100) RETURNING id::text`,
		storeID, n, kw, price,
	).Scan(&id); err != nil {
		t.Fatalf("seedProduct: %v", err)
	}
	return id
}

// insertCart inserts a raw cart and returns its id.
func insertCart(t *testing.T, eventID, handle, token string, shortID int, paymentStatus string, paidAt *time.Time) string {
	t.Helper()
	ctx := context.Background()
	n := randomSuffix()
	var id string
	var err error
	if paidAt != nil {
		err = testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			   status, payment_status, paid_at, coupon_discount_cents, shipping_cost_cents)
			 VALUES ($1, 'u-'||$2, $3, $4, $5,
			   'checkout', $6, $7, 0, 0) RETURNING id::text`,
			eventID, n, handle, token, shortID, paymentStatus, *paidAt,
		).Scan(&id)
	} else {
		err = testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			   status, payment_status, coupon_discount_cents, shipping_cost_cents)
			 VALUES ($1, 'u-'||$2, $3, $4, $5, 'active', $6, 0, 0) RETURNING id::text`,
			eventID, n, handle, token, shortID, paymentStatus,
		).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insertCart: %v", err)
	}
	return id
}

// addItem inserts a cart_item.
func addItem(t *testing.T, cartID, productID string, qty int, price int64) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, $4, 0)`, cartID, productID, qty, price,
	); err != nil {
		t.Fatalf("addItem: %v", err)
	}
}

// ─── G1/G2/G3/G4 ────────────────────────────────────────────────────────────

func TestCartOrderPageSplit_UnionCoversAllCarts(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	storeID, eventID := seedIsolatedStore(t, "g3")
	prodID := seedProduct(t, storeID, 5000)
	prodID2 := seedProduct(t, storeID, 3000)

	// C1: cart pago → order materializada
	now := time.Now()
	paidCartID := insertCart(t, eventID, "@g3paid", "tok-g3p", 77101, "paid", &now)
	addItem(t, paidCartID, prodID, 2, 5000)
	if err := l.OnCartPaid(ctx, paidCartID, storeID, 10000, nil); err != nil {
		t.Fatalf("C1 OnCartPaid: %v", err)
	}

	// C2: cart não-pago (ativo, sem order)
	unpaidCartID := insertCart(t, eventID, "@g3pending", "tok-g3u", 77102, "pending", nil)
	addItem(t, unpaidCartID, prodID, 1, 5000)

	// C3: cart pago → order materializada → simulação de estorno
	refCartID := insertCart(t, eventID, "@g3ref", "tok-g3r", 77103, "paid", &now)
	addItem(t, refCartID, prodID2, 1, 3000)
	if err := l.OnCartPaid(ctx, refCartID, storeID, 3000, nil); err != nil {
		t.Fatalf("C3 OnCartPaid: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET payment_status = 'refunded' WHERE id = $1`, refCartID,
	); err != nil {
		t.Fatalf("C3 refund: %v", err)
	}

	// ─── G1: Pedidos contém TODOS os carts da loja ────────────────────────────

	orderRepo := order.NewRepository(testPool)
	orderResult, err := orderRepo.List(ctx, order.ListOrdersParams{
		StoreID:    storeID,
		Pagination: query.Pagination{Page: 1, Limit: 100},
		Sorting:    query.Sorting{SortBy: "created_at", SortOrder: "desc"},
	})
	if err != nil {
		t.Fatalf("G1 order list: %v", err)
	}

	byID := make(map[string]order.OrderRow)
	for _, o := range orderResult.Orders {
		byID[o.ID] = o
	}

	if _, ok := byID[paidCartID]; !ok {
		t.Errorf("G1: cart pago %s deve aparecer em Pedidos", paidCartID)
	}
	if _, ok := byID[refCartID]; !ok {
		t.Errorf("G1: cart estornado %s deve aparecer em Pedidos", refCartID)
	}
	if _, ok := byID[unpaidCartID]; !ok {
		t.Errorf("G1: cart NÃO pago %s deve aparecer em Pedidos — é ele que o "+
			"lojista precisa ver e cancelar; era o bug de campo", unpaidCartID)
	}

	rows, err := testPool.Query(ctx,
		`SELECT c.id::text FROM carts c
		 JOIN live_events e ON e.id = c.event_id
		 WHERE e.store_id = $1`, storeID)
	if err != nil {
		t.Fatalf("G1 all carts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("G1 scan: %v", err)
		}
		if _, ok := byID[id]; !ok {
			t.Errorf("G1: cart %s sumiu da tela de Pedidos", id)
		}
	}

	// ─── G2: com Order, o total é o valor CONGELADO na venda ─────────────────

	for _, cartID := range []string{paidCartID, refCartID} {
		var orderTotalCents int64
		if err := testPool.QueryRow(ctx,
			`SELECT COALESCE(total_cents, 0) FROM orders WHERE cart_id = $1`, cartID,
		).Scan(&orderTotalCents); err != nil {
			t.Fatalf("G2 query: %v", err)
		}
		if got := byID[cartID].TotalAmount; got != orderTotalCents {
			t.Errorf("G2: cart %s total_amount=%d != orders.total_cents=%d",
				cartID, got, orderTotalCents)
		}
	}

	// ─── G3: sem Order, o total vem do cart pela função canônica ─────────────

	var cartTotalCents int64
	if err := testPool.QueryRow(ctx,
		`SELECT cart_product_total_cents($1)`, unpaidCartID,
	).Scan(&cartTotalCents); err != nil {
		t.Fatalf("G3 query: %v", err)
	}
	if got := byID[unpaidCartID].TotalAmount; got != cartTotalCents {
		t.Errorf("G3: cart não pago %s total_amount=%d != cart_product_total_cents=%d",
			unpaidCartID, got, cartTotalCents)
	}
	if cartTotalCents == 0 {
		t.Error("G3: o seed deveria dar total > 0 — o teste não está provando nada")
	}

	// ─── G4: primeira compra é atributo de VENDA, não de carrinho aberto ─────

	if byID[unpaidCartID].IsFirstPurchase {
		t.Errorf("G4: cart não pago %s marcado como primeira compra", unpaidCartID)
	}
}
