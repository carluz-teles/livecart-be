package integration

// Estorno de carrinho em voo suprime o sync de estoque vindo do ERP.
//
// O caso real (staging, 03/08): o lojista cancelou dois carrinhos. Os produtos
// 1001 e 1004 tinham reserva ativa nos DOIS, logo dois estornos pendentes cada.
//
//	19:06:43.957  cancelamento commitado → estoque local do 1001 = 5 (correto)
//	19:06:44.2    1º estorno chega no Tiny  → Tiny 3→4
//	19:06:44.547  webhook: local=5, erp=4, downgrade_only → GRAVA 4   ← a perda
//	19:06:48.070  2º estorno: local=4, erp=5 → 5>=4 → PRESERVA 4      ← a trava
//
// O crédito local é atômico (uma transação); o estorno no ERP é sequencial, uma
// chamada HTTP por reserva, e cada uma dispara um webhook. Nessa janela o Tiny
// está atrás de nós POR NOSSA CAUSA — "ERP menor que o local" não é redução do
// lojista. `downgrade_only` deixa passar exatamente a direção que causa o dano,
// e depois a mesma regra impede a autocorreção quando o ERP alcança.
//
// Por isso a supressão tem de ser total (skip), não "só reduções". E por isso o
// gatilho é reserva ATIVA de carrinho TERMINAL: unidade já devolvida ao estoque
// local cujo estorno no ERP ainda não fechou.
//
// Carrinho VIVO com reserva ativa não entra aqui — é o estado normal de quem
// está comprando, e quem cobre isso é HasStockGuardForProduct.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestHasPendingCartReversalForProduct(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	n := fmt.Sprintf("%d", time.Now().UnixNano())
	var storeID, eventID, productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Rev Store', 'rev-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','Semana Black', now() + interval '2 days') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	externalID := "ext-rev-" + n
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, external_id, price, stock, active)
		 VALUES ($1,'Cafe',right($2,4),'tiny',$3,5000,5,true) RETURNING id::text`,
		storeID, n, externalID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	newCart := func(status, suffix string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status)
			 VALUES ($1::uuid,'u-'||$2,'@'||$2,'tok-'||$2||'-'||$3,(('x'||md5($2||$3))::bit(20)::int),$4)
			 RETURNING id::text`,
			eventID, suffix, n, status,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %s: %v", suffix, err)
		}
		return id
	}

	reserve := func(cartID, status string) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, status)
			 VALUES ($1::uuid,$2::uuid,$3::uuid,$4,1,$5)`,
			eventID, cartID, productID, externalID, status,
		); err != nil {
			t.Fatalf("seed reservation: %v", err)
		}
	}

	check := func() bool {
		t.Helper()
		got, err := testRepo.HasPendingCartReversalForProduct(ctx, externalID, storeID)
		if err != nil {
			t.Fatalf("HasPendingCartReversalForProduct: %v", err)
		}
		return got
	}

	t.Run("sem reserva nenhuma nao suprime", func(t *testing.T) {
		if check() {
			t.Error("suprimiu sem haver estorno em voo — o sync normal do lojista pararia de funcionar")
		}
	})

	t.Run("carrinho VIVO com reserva ativa nao suprime", func(t *testing.T) {
		reserve(newCart("active", "vivo"), "active")
		if check() {
			t.Error("carrinho em compra nao e estorno em voo — quem cobre esse caso e o guard de evento ativo")
		}
	})

	t.Run("carrinho CANCELADO com reserva ativa SUPRIME", func(t *testing.T) {
		reserve(newCart("cancelled", "cancelado"), "active")
		if !check() {
			t.Error("nao suprimiu com estorno em voo — e exatamente aqui que o webhook do Tiny grava um saldo atrasado por cima do nosso")
		}
	})

	t.Run("carrinho EXPIRADO com reserva ativa SUPRIME", func(t *testing.T) {
		reserve(newCart("expired", "expirado"), "active")
		if !check() {
			t.Error("expiracao credita o local igual ao cancelamento e tem a mesma janela")
		}
	})
}

// A janela NÃO fecha na marcação `reversed` — fecha alguns minutos depois.
//
// Marcar a reserva como estornada só diz que a chamada ao ERP retornou. O
// webhook que o Tiny dispara por aquele movimento chega DEPOIS e é ele que
// carrega o saldo. Em staging (04/08) foram 27 segundos de diferença, e nesse
// intervalo o eco do nosso próprio estorno sobrescreveu um estoque local que
// estava CERTO: 5 virou 2, 9, 7 e 6.
//
// Este teste guarda as duas pontas: ainda suprime logo após o estorno, e volta
// ao normal quando a janela passa — senão o produto congelaria para sempre
// depois do primeiro cancelamento.
func TestPendingReversalGuardLiberaAposAJanela(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	n := fmt.Sprintf("%d", time.Now().UnixNano())
	var storeID, eventID, productID, cartID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Rev Store 2', 'rev2-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,'active','E', now() + interval '2 days') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	externalID := "ext-rev2-" + n
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, external_id, price, stock, active)
		 VALUES ($1,'Cafe2',right($2,4),'tiny',$3,5000,5,true) RETURNING id::text`,
		storeID, n, externalID,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status)
		 VALUES ($1::uuid,'u2','@u2','tok2-'||$2,(('x'||md5('u2'||$2))::bit(20)::int),'cancelled')
		 RETURNING id::text`, eventID, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, status)
		 VALUES ($1::uuid,$2::uuid,$3::uuid,$4,1,'active')`,
		eventID, cartID, productID, externalID,
	); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	got, err := testRepo.HasPendingCartReversalForProduct(ctx, externalID, storeID)
	if err != nil {
		t.Fatalf("antes do estorno: %v", err)
	}
	if !got {
		t.Fatal("deveria suprimir enquanto o estorno nao fechou")
	}

	// Estorno acabou AGORA: o webhook do Tiny ainda está a caminho.
	if _, err := testPool.Exec(ctx,
		`UPDATE stock_reservations SET status='reversed', reversed_at=now() WHERE cart_id=$1::uuid`, cartID,
	); err != nil {
		t.Fatalf("marcar estornada: %v", err)
	}
	got, err = testRepo.HasPendingCartReversalForProduct(ctx, externalID, storeID)
	if err != nil {
		t.Fatalf("logo apos o estorno: %v", err)
	}
	if !got {
		t.Error("soltou na marcacao — e nesse intervalo que o eco do proprio estorno sobrescreve o estoque correto")
	}

	// Janela passada: o saldo do ERP volta a ser confiável.
	if _, err := testPool.Exec(ctx,
		`UPDATE stock_reservations SET reversed_at = now() - interval '10 minutes' WHERE cart_id=$1::uuid`, cartID,
	); err != nil {
		t.Fatalf("envelhecer o estorno: %v", err)
	}
	got, err = testRepo.HasPendingCartReversalForProduct(ctx, externalID, storeID)
	if err != nil {
		t.Fatalf("depois da janela: %v", err)
	}
	if got {
		t.Error("continuou suprimindo depois da janela — o produto ficaria congelado para sempre")
	}
}
