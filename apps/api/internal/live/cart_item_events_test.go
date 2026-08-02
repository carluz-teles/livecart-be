package live

// O log de atribuição precisa gravar no caminho REAL de adição, no mesmo tx do
// upsert. Sem isso o alocador não tem o que repartir e a métrica por sessão
// volta a ser first-touch.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seq é o sequence_order da sessão dentro do evento — live_sessions tem
// UNIQUE (event_id, sequence_order) desde a 000090, então cada transmissão da
// mesma campanha precisa do seu.
func seedSession(t *testing.T, eventID string, seq int) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO live_sessions (event_id, status, sequence_order)
		 VALUES ($1::uuid,'active',$2) RETURNING id::text`,
		eventID, seq,
	).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func seedProduct(t *testing.T, eventID string, price int64) string {
	t.Helper()
	ctx := context.Background()
	var storeID string
	if err := testPool.QueryRow(ctx,
		`SELECT store_id::text FROM live_events WHERE id = $1::uuid`, eventID,
	).Scan(&storeID); err != nil {
		t.Fatalf("resolver store: %v", err)
	}
	var id string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'Vestido','none',$2,$3,$4,100) RETURNING id::text`,
		storeID,
		fmt.Sprintf("ext-%d", time.Now().UnixNano()),
		fmt.Sprintf("%04d", time.Now().UnixNano()%10000),
		price,
	).Scan(&id); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return id
}

func TestAddCartItemLogsAttribution(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	segunda := seedSession(t, eventID, 1)
	quarta := seedSession(t, eventID, 2)
	cart, _ := getOrCreate(t, eventID, "maria")
	produto := seedProduct(t, eventID, 2500)

	add := func(sessionID string, qty int) {
		t.Helper()
		if err := testRepo.AddCartItem(ctx, AddCartItemParams{
			CartID:    cart.ID,
			ProductID: produto,
			SessionID: sessionID,
			Quantity:  qty,
			UnitPrice: 2500,
		}); err != nil {
			t.Fatalf("AddCartItem: %v", err)
		}
	}

	// A compra que motiva a regra: 1un na live de segunda, 1un na de quarta.
	add(segunda, 1)
	add(quarta, 1)

	// cart_items sozinho continua dizendo "tudo veio da segunda" — first-touch.
	var itemQty int
	var firstTouch string
	if err := testPool.QueryRow(ctx,
		`SELECT quantity, COALESCE(session_id::text,'') FROM cart_items
		 WHERE cart_id = $1::uuid AND product_id = $2::uuid`,
		cart.ID, produto,
	).Scan(&itemQty, &firstTouch); err != nil {
		t.Fatalf("ler cart_item: %v", err)
	}
	if itemQty != 2 {
		t.Errorf("cart_items.quantity = %d, quero 2", itemQty)
	}
	if firstTouch != segunda {
		t.Errorf("cart_items.session_id = %s, esperava o first-touch (%s)", firstTouch, segunda)
	}

	// O log, esse sim, sabe a verdade.
	rows, err := testPool.Query(ctx,
		`SELECT COALESCE(session_id::text,''), quantity, unit_price
		 FROM cart_item_events WHERE cart_id = $1::uuid AND product_id = $2::uuid
		 ORDER BY created_at, id`,
		cart.ID, produto,
	)
	if err != nil {
		t.Fatalf("ler log: %v", err)
	}
	defer rows.Close()

	var adds []CartItemAddition
	for rows.Next() {
		var a CartItemAddition
		if err := rows.Scan(&a.SessionID, &a.Quantity, &a.UnitPrice); err != nil {
			t.Fatalf("scan log: %v", err)
		}
		adds = append(adds, a)
	}
	if len(adds) != 2 {
		t.Fatalf("log tem %d linhas, quero 2", len(adds))
	}
	if adds[0].SessionID != segunda || adds[1].SessionID != quarta {
		t.Errorf("log = %+v, quero segunda depois quarta", adds)
	}

	// E o alocador reparte certo em cima dele.
	allocs := AllocateBySession(itemQty, adds)
	if len(allocs) != 2 || allocs[0].Quantity != 1 || allocs[1].Quantity != 1 {
		t.Errorf("alocacao = %+v, quero 1un para cada sessao", allocs)
	}
	if allocs[0].SessionID != segunda || allocs[1].SessionID != quarta {
		t.Errorf("alocacao creditou as sessoes erradas: %+v", allocs)
	}
}

// Adição sem sessão (item posto pelo painel) grava com session_id NULL em vez
// de estourar na FK.
func TestAddCartItemWithoutSessionLogsNull(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	cart, _ := getOrCreate(t, eventID, "joao")
	produto := seedProduct(t, eventID, 1000)

	if err := testRepo.AddCartItem(ctx, AddCartItemParams{
		CartID:    cart.ID,
		ProductID: produto,
		Quantity:  1,
		UnitPrice: 1000,
	}); err != nil {
		t.Fatalf("AddCartItem sem sessao: %v", err)
	}

	var sessionIsNull bool
	if err := testPool.QueryRow(ctx,
		`SELECT session_id IS NULL FROM cart_item_events WHERE cart_id = $1::uuid`, cart.ID,
	).Scan(&sessionIsNull); err != nil {
		t.Fatalf("ler log: %v", err)
	}
	if !sessionIsNull {
		t.Error("adicao sem sessao devia gravar session_id NULL")
	}
}
