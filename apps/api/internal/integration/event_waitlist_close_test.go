package integration

// RN-32 — o item de fila NÃO ATENDIDO morre com o evento, e o carrinho volta a
// poder expirar.
//
// O furo: o guard do ExpireCart se abstém de expirar QUALQUER carrinho com item
// 'waiting', e o ramo 'waiting' não tem prazo nenhum. Como não existe mais
// sweep de carrinhos, a task cart.expire dispara uma vez, encontra 0 rows e
// encerra — nada mais a re-arma exceto uma promoção da fila, que por definição
// não vem quando o carrinho da frente foi PAGO em vez de expirar. Resultado:
// carrinho permanentemente vivo, com expires_at vencido, segurando o estoque
// reservado dos seus itens não-waitlisted.
//
// O que estes testes travam:
//  1. 'waiting' morre; 'notified' AINDA DENTRO da janela de TTL sobrevive (o
//     PRD proíbe explicitamente matar quem acabou de ser promovido);
//  2. 'notified' com janela JÁ VENCIDA morre junto;
//  3. depois disso o guard do ExpireCart deixa de vetar e o carrinho expira;
//  4. os carrinhos afetados voltam da operação para o chamador re-armar
//     cart.expire — sem isso eles continuariam vivos mesmo com a fila morta.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type waitlistCloseFixture struct {
	eventID   string
	storeID   string
	productID string
	carts     map[string]string // rótulo -> cart id
}

func seedWaitlistCloseFixture(t *testing.T) waitlistCloseFixture {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())
	f := waitlistCloseFixture{carts: map[string]string{}}

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('RN32','rn32-'||$1) RETURNING id::text`, n,
	).Scan(&f.storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, ends_at)
		 VALUES ($1,'ended','Semana','multi', now() - interval '1 day') RETURNING id::text`, f.storeID,
	).Scan(&f.eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Vestido','V'||substr($2,1,3),'manual',1000,0,true) RETURNING id::text`, f.storeID, n,
	).Scan(&f.productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// Três compradores, três carrinhos já com o prazo VENCIDO (o evento fechou
	// ontem), cada um com um item de fila em estado diferente.
	newCart := func(label string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			     status, payment_status, expires_at)
			 VALUES ($1, $2::text, '@'||$2::text, 'tok-'||$2::text||'-'||$3::text, (floor(random()*90000)+10000)::int,
			     'checkout','pending', now() - interval '1 hour') RETURNING id::text`,
			f.eventID, label, n,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %s: %v", label, err)
		}
		f.carts[label] = id
		return id
	}
	addWaitlistItem := func(label, status string, expiresAt any, position int) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle,
			     quantity, position, status, cart_id, expires_at)
			 VALUES ($1,$2,$3::text,'@'||$3::text,1,$4,$5::text,$6::uuid,$7)`,
			f.eventID, f.productID, label, position, status, f.carts[label], expiresAt,
		); err != nil {
			t.Fatalf("seed waitlist %s: %v", label, err)
		}
	}

	newCart("waiting")
	newCart("notified-vivo")
	newCart("notified-vencido")
	addWaitlistItem("waiting", "waiting", nil, 1)
	addWaitlistItem("notified-vivo", "notified", time.Now().UTC().Add(2*time.Hour), 2)
	addWaitlistItem("notified-vencido", "notified", time.Now().UTC().Add(-2*time.Hour), 3)

	return f
}

func waitlistStatusByCart(t *testing.T, cartID string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM waitlist_items WHERE cart_id = $1::uuid`, cartID,
	).Scan(&status); err != nil {
		t.Fatalf("ler status da fila: %v", err)
	}
	return status
}

func TestExpireEventWaitlistSparesLivePromotion(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWaitlistCloseFixture(t)

	entries, err := testRepo.ExpireEventWaitlist(ctx, f.eventID)
	if err != nil {
		t.Fatalf("ExpireEventWaitlist: %v", err)
	}

	if got := waitlistStatusByCart(t, f.carts["waiting"]); got != "expired" {
		t.Errorf("item 'waiting' ficou %q — sem morrer, o carrinho é eterno", got)
	}
	if got := waitlistStatusByCart(t, f.carts["notified-vencido"]); got != "expired" {
		t.Errorf("item 'notified' com janela vencida ficou %q, queria \"expired\"", got)
	}
	// O predicado antigo desta query (status IN ('waiting','notified')) mataria
	// este aqui — o comprador foi promovido e ainda tem prazo válido.
	if got := waitlistStatusByCart(t, f.carts["notified-vivo"]); got != "notified" {
		t.Errorf("item 'notified' DENTRO da janela virou %q — o PRD proíbe matar quem foi promovido", got)
	}

	// Os carrinhos desbloqueados voltam para o chamador re-armar cart.expire.
	unblocked := map[string]bool{}
	for _, e := range entries {
		unblocked[e.CartID] = true
		// RN-28 gatilho 5: sem comprador e sem nome do produto a fila fechava
		// muda — o carrinho voltava a poder expirar e o cliente só descobria
		// pelo silêncio.
		if e.PlatformHandle == "" || e.ProductName == "" {
			t.Errorf("entrada sem dados para a DM: %+v", e)
		}
	}
	if !unblocked[f.carts["waiting"]] || !unblocked[f.carts["notified-vencido"]] {
		t.Errorf("carrinhos desbloqueados não vieram no retorno: %+v", entries)
	}
	if unblocked[f.carts["notified-vivo"]] {
		t.Error("carrinho do promovido veio no retorno — ele não foi desbloqueado")
	}
}

func TestCartExpiresOnlyAfterWaitlistClose(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	f := seedWaitlistCloseFixture(t)
	cartID := f.carts["waiting"]

	// Antes: o guard do ExpireCart se abstém — é aqui que o carrinho ficava
	// preso para sempre.
	before, err := testRepo.ExpireCartAndReleaseStock(ctx, cartID, f.storeID)
	if err != nil {
		t.Fatalf("ExpireCartAndReleaseStock (antes): %v", err)
	}
	if before.Eligible {
		t.Fatal("carrinho com item 'waiting' expirou antes do fechamento da fila — o guard existe justamente para isso")
	}

	if _, err := testRepo.ExpireEventWaitlist(ctx, f.eventID); err != nil {
		t.Fatalf("ExpireEventWaitlist: %v", err)
	}

	after, err := testRepo.ExpireCartAndReleaseStock(ctx, cartID, f.storeID)
	if err != nil {
		t.Fatalf("ExpireCartAndReleaseStock (depois): %v", err)
	}
	if !after.Eligible {
		t.Fatal("carrinho continuou inexpirável mesmo com a fila encerrada")
	}

	var status string
	if err := testPool.QueryRow(ctx,
		`SELECT status FROM carts WHERE id = $1::uuid`, cartID,
	).Scan(&status); err != nil {
		t.Fatalf("reler cart: %v", err)
	}
	if status != "expired" {
		t.Errorf("cart.status = %q, queria \"expired\"", status)
	}
}
