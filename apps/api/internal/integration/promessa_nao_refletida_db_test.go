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
	carrinhoPrometendoComSituacao(t, eventID, productID, qtd, pedidoERP, iniciadoEm, "aberto")
}

func carrinhoPrometendoComSituacao(
	t *testing.T, eventID, productID string, qtd int, pedidoERP string,
	iniciadoEm *time.Time, situacao string,
) string {
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
	if pedidoERP != "" && situacao != "" {
		if _, err := testPool.Exec(ctx,
			`UPDATE carts SET erp_order_status=$2 WHERE id=$1::uuid`, cartID, situacao); err != nil {
			t.Fatalf("seed erp_order_status: %v", err)
		}
	}
	return cartID
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

	// Os dois primeiros casos não dependem do modo: carrinho SEM pedido conta
	// nos dois. Fixado em false (nativo) para deixar isso explícito.
	const contaComPedido = false

	t.Run("produto Bling é contado — a versão antiga devolvia zero", func(t *testing.T) {
		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, contaComPedido)
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

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, contaComPedido)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 3 {
			t.Errorf("soma da loja Bling = %d, queria 3 — as 7 unidades da outra loja vazaram", nova)
		}
	})

	t.Run("reserva NATIVA: carrinho COM pedido não conta — o ERP já o descontou", func(t *testing.T) {
		agora := time.Now()
		carrinhoPrometendo(t, eventoBling, prodBling, 2, "PEDIDO-NOVO", &agora)

		// contaComPedido=false é o modo NATIVO: o pedido já tirou a peça do
		// `disponivel` do ERP, e somá-la aqui tiraria duas vezes.
		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, false)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 3 { // só as 3 sem pedido
			t.Errorf("soma = %d, queria 3 — as 2 unidades com pedido JÁ estão fora do "+
				"disponível do ERP; contá-las de novo é o desconto em dobro que "+
				"fez o LiveCart mostrar 1 enquanto o Bling mostrava 3", nova)
		}
	})

	t.Run("reserva NATIVA: a idade do pedido é irrelevante", func(t *testing.T) {
		// A versão anterior tinha uma janela de 45 s e contava pedido recente.
		// Numa live TODO pedido é recente, então a janela errava SEMPRE.
		velho := time.Now().Add(-10 * time.Minute)
		carrinhoPrometendo(t, eventoBling, prodBling, 4, "PEDIDO-VELHO", &velho)

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, false)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 3 {
			t.Errorf("soma = %d, queria 3 — com reserva nativa nenhum carrinho COM "+
				"pedido conta, tenha ele 2 segundos ou 10 minutos", nova)
		}
	})

	t.Run("reserva LOCAL: tudo conta, porque o ERP não sabe de nada", func(t *testing.T) {
		// Neste ponto a loja tem 3 sem pedido + 2 com pedido novo + 4 com
		// pedido velho = 9 unidades vivas.
		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, true)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 9 {
			t.Errorf("soma = %d, queria 9 — no modo local o pedido no ERP não segura "+
				"nada, então toda unidade viva precisa sair do saldo lido", nova)
		}
	})

	// PEDIDO APAGADO NO ERP CONTINUA CONTANDO — e este é o caso que engana.
	//
	// 'nao_encontrado' é TERMINAL para a varredura: não adianta perguntar de
	// novo por um pedido que o lojista apagou. É fácil concluir daí que ele
	// também está resolvido para a promessa — e é o contrário. O pedido sumiu,
	// e com ele a baixa: o saldo do ERP NÃO desconta aquela venda.
	//
	// Deixá-lo de fora faria a promessa parar de contar, o portão subir com
	// peça que já tem dono, e a live vender o que não existe.
	t.Run("reserva NATIVA: pedido APAGADO no ERP volta a contar", func(t *testing.T) {
		agora := time.Now()
		carrinhoPrometendoComSituacao(t, eventoBling, prodBling, 5,
			"PEDIDO-APAGADO", &agora, "nao_encontrado")

		nova, err := testRepo.SumPromisedNotYetReflected(ctx, lojaBling, "bling", mesmoID, false)
		if err != nil {
			t.Fatal(err)
		}
		if nova != 8 { // as 3 sem pedido + as 5 do pedido apagado
			t.Errorf("soma = %d, queria 8 — o pedido sumiu do ERP, logo o saldo de lá "+
				"NÃO desconta essas 5 unidades; tratá-las como refletidas infla o portão", nova)
		}
	})
}

// A NOVA QUERY NÃO PODE DIVERGIR DA QUE RODA EM PRODUÇÃO.
//
// SumPromisedWithoutERPOrder é a versão em produção hoje, com anos de live em
// cima dela. A regra dela é uma linha:
//
//	AND (c.external_order_id IS NULL OR c.external_order_id = '')
//
// Sem janela de tempo, sem nada. SumPromisedNotYetReflected foi escrita para
// substituí-la (a antiga tem 'tiny' literal e não filtra loja) e, no caminho,
// GANHOU uma janela de 45 segundos que a antiga nunca teve. Aplicada a todos os
// providers, ela faria TODA loja Tiny descontar estoque em dobro no instante em
// que a branch chegasse a produção — porque numa live todo carrinho tem menos
// de 45 s.
//
// Este teste existe para que a substituição seja provada, e não presumida: no
// modo NATIVO (o padrão do Tiny), as duas têm de devolver o MESMO número, em
// todos os arranjos de carrinho que importam.
func TestNovaSomaEhEquivalenteAQueRodaEmProducao(t *testing.T) {
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL não definida")
	}
	ctx := context.Background()

	// external_id único: a query ANTIGA não filtra por loja, então qualquer
	// outro produto com o mesmo id somaria junto e o teste mentiria.
	externalID := fmt.Sprintf("EQV-%d-%d", seedSeq, rand.Intn(1_000_000))
	_, evento, produto := lojaComProduto(t, "tiny", externalID)
	loja := ""
	if err := testPool.QueryRow(ctx,
		`SELECT store_id::text FROM products WHERE id=$1::uuid`, produto).Scan(&loja); err != nil {
		t.Fatal(err)
	}

	agora := time.Now()
	velho := agora.Add(-10 * time.Minute)
	arranjos := []struct {
		nome   string
		qtd    int
		pedido string
		quando *time.Time
	}{
		{"sem pedido", 3, "", nil},
		{"pedido recém-criado", 2, "TINY-NOVO", &agora},
		{"pedido antigo", 4, "TINY-VELHO", &velho},
		{"pedido sem carimbo de operação", 1, "TINY-SEM-STAMP", nil},
	}

	for _, a := range arranjos {
		carrinhoPrometendo(t, evento, produto, a.qtd, a.pedido, a.quando)

		antiga, err := testRepo.SumPromisedWithoutERPOrder(ctx, externalID)
		if err != nil {
			t.Fatalf("%s: query de produção: %v", a.nome, err)
		}
		// contaComPedido=false é o modo NATIVO, que é o padrão do Tiny.
		nova, err := testRepo.SumPromisedNotYetReflected(ctx, loja, "tiny", externalID, false)
		if err != nil {
			t.Fatalf("%s: query nova: %v", a.nome, err)
		}
		if antiga != nova {
			t.Fatalf("depois de acrescentar %q: produção diz %d e a nova diz %d.\n"+
				"  A substituta DIVERGIU da query que roda hoje. Se a diferença for "+
				"deliberada, ela precisa de um teste próprio dizendo por quê — foi "+
				"exatamente assim que uma janela de 45 s entrou sem ninguém notar e "+
				"passou a descontar toda reserva duas vezes.", a.nome, antiga, nova)
		}
	}
}
