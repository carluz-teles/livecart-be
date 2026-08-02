package live

// Prova de ponta a ponta da RN-29: um pedido com o MESMO produto comprado em
// duas transmissões gera duas linhas de order_items, uma por sessão, e a soma
// das quantidades continua igual à do carrinho.
//
// Mora no pacote live (e não em order/listeners) porque é aqui que estão o
// AddCartItem real e o alocador; o teste monta o carrinho pelo caminho de
// produção e depois exercita a alocação exatamente como o selamento faz.

import (
	"context"
	"testing"
)

func TestOrderItemsSplitBySession(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	eventID := seedEvent(t)
	segunda := seedSession(t, eventID, 1)
	quarta := seedSession(t, eventID, 2)
	cart, _ := getOrCreate(t, eventID, "maria")

	vestido := seedProduct(t, eventID, 2500)
	bolsa := seedProduct(t, eventID, 8000)

	add := func(session, product string, qty int, price int64) {
		t.Helper()
		if err := testRepo.AddCartItem(ctx, AddCartItemParams{
			CartID: cart.ID, ProductID: product, SessionID: session,
			Quantity: qty, UnitPrice: price,
		}); err != nil {
			t.Fatalf("AddCartItem: %v", err)
		}
	}

	// Semana Black: vestido na segunda e de novo na quarta; bolsa só na terça.
	add(segunda, vestido, 1, 2500)
	add(quarta, vestido, 1, 2500)
	add(quarta, bolsa, 1, 8000)

	// Repete o que o selamento faz: quantidade final do carrinho + log.
	type key struct{ product, session string }
	got := map[key]int{}

	rows, err := testPool.Query(ctx,
		`SELECT ci.product_id::text, ci.quantity FROM cart_items ci WHERE ci.cart_id = $1::uuid`,
		cart.ID)
	if err != nil {
		t.Fatalf("ler cart_items: %v", err)
	}
	type finalItem struct {
		product string
		qty     int
	}
	var finals []finalItem
	for rows.Next() {
		var f finalItem
		if err := rows.Scan(&f.product, &f.qty); err != nil {
			t.Fatalf("scan: %v", err)
		}
		finals = append(finals, f)
	}
	rows.Close()

	for _, f := range finals {
		logRows, err := testPool.Query(ctx,
			`SELECT COALESCE(session_id::text,''), quantity, unit_price
			 FROM cart_item_events WHERE cart_id = $1::uuid AND product_id = $2::uuid
			 ORDER BY created_at, id`,
			cart.ID, f.product)
		if err != nil {
			t.Fatalf("ler log: %v", err)
		}
		var adds []CartItemAddition
		for logRows.Next() {
			var a CartItemAddition
			if err := logRows.Scan(&a.SessionID, &a.Quantity, &a.UnitPrice); err != nil {
				t.Fatalf("scan log: %v", err)
			}
			adds = append(adds, a)
		}
		logRows.Close()

		allocs := AllocateBySession(f.qty, adds)

		total := 0
		for _, a := range allocs {
			got[key{f.product, a.SessionID}] += a.Quantity
			total += a.Quantity
		}
		// O invariante, no dado real.
		if total != f.qty {
			t.Errorf("produto %s: soma alocada %d != quantidade do carrinho %d", f.product, total, f.qty)
		}
	}

	// O vestido vira DUAS linhas de pedido, uma por transmissão.
	if got[key{vestido, segunda}] != 1 {
		t.Errorf("vestido/segunda = %d, quero 1", got[key{vestido, segunda}])
	}
	if got[key{vestido, quarta}] != 1 {
		t.Errorf("vestido/quarta = %d, quero 1", got[key{vestido, quarta}])
	}
	// A bolsa, uma só.
	if got[key{bolsa, quarta}] != 1 {
		t.Errorf("bolsa/quarta = %d, quero 1", got[key{bolsa, quarta}])
	}
	if got[key{bolsa, segunda}] != 0 {
		t.Errorf("bolsa creditada a segunda indevidamente: %d", got[key{bolsa, segunda}])
	}

	// "Quanto cada transmissão vendeu": segunda R$25, quarta R$25 + R$80.
	receitaSegunda := got[key{vestido, segunda}] * 2500
	receitaQuarta := got[key{vestido, quarta}]*2500 + got[key{bolsa, quarta}]*8000
	if receitaSegunda != 2500 {
		t.Errorf("receita da segunda = %d, quero 2500", receitaSegunda)
	}
	if receitaQuarta != 10500 {
		t.Errorf("receita da quarta = %d, quero 10500", receitaQuarta)
	}
	// E a soma das transmissões fecha com o total do carrinho.
	var totalCarrinho int
	if err := testPool.QueryRow(ctx,
		`SELECT cart_product_total_cents($1::uuid)::int`, cart.ID,
	).Scan(&totalCarrinho); err != nil {
		t.Fatalf("total do carrinho: %v", err)
	}
	if receitaSegunda+receitaQuarta != totalCarrinho {
		t.Errorf("soma das sessoes = %d, total do carrinho = %d — a metrica em dois niveis nao fecharia",
			receitaSegunda+receitaQuarta, totalCarrinho)
	}
}
