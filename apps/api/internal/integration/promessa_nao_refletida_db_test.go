package integration

// A SOMA DAS PROMESSAS QUE O ERP AINDA NÃO DESCONTA.
//
// Prova, contra o Postgres de verdade, os três defeitos que
// SumPromisedNotYetReflected conserta em SumPromisedWithoutERPOrder:
//
//  1. `external_source` era o literal 'tiny'. Para um produto Bling a soma
//     devolvia ZERO, o portão recebia o disponível CRU do ERP e era reabastecido
//     com estoque que já tinha dono — o −13 de 26/08 letra por letra, num ERP novo.
//  2. Faltava `store_id`. Duas lojas com produtos de mesmo `external_id` somavam
//     as promessas uma da outra.
//  3. `external_order_id IS NULL` assumia que, existindo o pedido, o ERP já o
//     desconta. MEDIDO contra o Bling em 29/08/2026: o `virtual_stock.updated`
//     chega de 9 a 22 SEGUNDOS depois do `order.created`. Nessa janela ninguém
//     contava a unidade, e o portão subia com estoque comprometido.
//
// Os três vivem no SQL — um literal, um WHERE que falta e um predicado de tempo.
// Nenhum apareceria com repositório simulado, que devolveria o que mandássemos.
//
// Rodar:
//
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	go test -run TestPromessaNaoRefletida -v ./apps/api/internal/integration/

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// lojaComProduto cria loja + evento + produto com o external_id e a fonte
// pedidos. Devolve (storeID, eventID, productID).
func lojaComProduto(t *testing.T, fonte, externalID string) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	n := fmt.Sprintf("%d-%d", seedSeq, rand.Intn(1_000_000))

	var storeID, eventID, productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Promessa', 'promessa-'||$1) RETURNING id::text`,
		n).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1, 'active', 'Live', now() + interval '1 day') RETURNING id::text`,
		storeID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1, 'P '||$2, $3, $4, $5, 1000, 100) RETURNING id::text`,
		storeID, n, fonte, externalID, fmt.Sprintf("K%03d", seedSeq%1000)).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return storeID, eventID, productID
}

// carrinhoPrometendo cria um carrinho vivo segurando `qtd` unidades.
// `pedidoERP` vazio = ainda sem pedido no ERP. `iniciadoEm` carimba
// erp_op_started_at, que é o que decide a janela de atraso.
func carrinhoPrometendo(t *testing.T, eventID, productID string, qtd int, pedidoERP string, iniciadoEm *time.Time) {
	t.Helper()
	ctx := context.Background()
	seedSeq++
	uniq := fmt.Sprintf("c%d-%d", seedSeq, rand.Intn(1_000_000))

	var cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status,
		                    external_order_id, erp_op_started_at)
		 VALUES ($1, 'u-'||$2, '@'||$2, 'tk-'||$2, (floor(random()*2000000000))::int, 'checkout', 'unpaid',
		         NULLIF($3,''), $4)
		 RETURNING id::text`, eventID, uniq, pedidoERP, iniciadoEm).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1, $2, $3, 1000, 0)`, cartID, productID, qtd); err != nil {
		t.Fatalf("seed cart_item: %v", err)
	}
}

func TestPromessaNaoRefletida(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()

	// O MESMO external_id em duas lojas e dois ERPs — o cenário que a versão
	// antiga soma errado por não filtrar loja nem fonte.
	const mesmoID = "99887766"
	lojaBling, eventoBling, prodBling := lojaComProduto(t, "bling", mesmoID)
	_, eventoTiny, prodTiny := lojaComProduto(t, "tiny", mesmoID)

	// 1) Produto Bling, carrinho vivo SEM pedido no ERP: 3 unidades prometidas.
	carrinhoPrometendo(t, eventoBling, prodBling, 3, "", nil)

	t.Run("produto Bling é contado — a versão antiga devolvia zero", func(t *testing.T) {
		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 3 {
			t.Fatalf("soma = %d, queria 3 — sem isso o portão recebe o saldo CRU do ERP "+
				"e é reabastecido com estoque que já tem dono", nova)
		}

		antiga, err := testRepo.SumPromisedWithoutERPOrder(ctx, mesmoID)
		if err != nil {
			t.Fatal(err)
		}
		if antiga == nova {
			t.Errorf("a query antiga também devolveu %d — ela tem 'tiny' literal e "+
				"NÃO deveria enxergar produto Bling; o teste perdeu o sentido", antiga)
		}
	})

	t.Run("a loja vizinha não vaza para dentro da soma", func(t *testing.T) {
		// Mesmo external_id, outra loja, outro ERP: 7 unidades.
		carrinhoPrometendo(t, eventoTiny, prodTiny, 7, "", nil)

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 3 {
			t.Errorf("soma da loja Bling = %d, queria 3 — as 7 unidades da outra loja vazaram", nova)
		}
	})

	t.Run("pedido RECÉM-criado ainda conta — a janela de atraso medida", func(t *testing.T) {
		agora := time.Now()
		carrinhoPrometendo(t, eventoBling, prodBling, 2, "PEDIDO-NOVO", &agora)

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 5 { // 3 sem pedido + 2 com pedido de agora
			t.Errorf("soma = %d, queria 5 — o pedido criado agora ainda não foi refletido "+
				"pelo ERP (medido: o evento demora de 9 a 22 segundos)", nova)
		}
	})

	t.Run("pedido ANTIGO deixa de contar — o ERP já o desconta", func(t *testing.T) {
		velho := time.Now().Add(-10 * time.Minute)
		carrinhoPrometendo(t, eventoBling, prodBling, 4, "PEDIDO-VELHO", &velho)

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 5 {
			t.Errorf("soma = %d, queria 5 — o pedido de 10 minutos atrás JÁ está no saldo "+
				"do ERP, e contá-lo de novo subtrairia duas vezes", nova)
		}
	})
}
