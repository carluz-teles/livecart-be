package integration

// RN-04 para a fila de espera: NADA expira dentro de um evento ABERTO
// (20/08/2026, produto 1130 da cantodaart). A promoção da fila ganha um TTL de
// 1h; o sweep lazy o expirava mesmo com o evento fechando só no dia seguinte,
// e a cadeia expiração → devolve-estoque-local → promove-próximo propagava uma
// unidade fantasma pela fila inteira. O consertо é na query que alimenta o
// sweep: ela só pode enxergar promoções vencidas cujo EVENTO já fechou.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func seedPromocaoVencida(t *testing.T, eventStatus string, endsAt time.Time) (storeID, eventID, cartID string) {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('RN04Fila','rn04-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at)
		 VALUES ($1,$2,'Semana',$3) RETURNING id::text`, storeID, eventStatus, endsAt,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	var productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Guirlanda','G'||substr($2,1,3),'manual',1000,0,true) RETURNING id::text`, storeID, n,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status)
		 VALUES ($1,'buyer','@buyer','tok-'||$2,(floor(random()*90000)+10000)::int,'checkout','pending')
		 RETURNING id::text`, eventID, n,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	// Promoção notified com o TTL JÁ vencido (1h atrás).
	if _, err := testPool.Exec(ctx,
		`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle,
		     quantity, position, status, cart_id, notified_at, expires_at)
		 VALUES ($1,$2,'buyer','@buyer',1,1,'notified',$3::uuid, now() - interval '2 hours', now() - interval '1 hour')`,
		eventID, productID, cartID,
	); err != nil {
		t.Fatalf("seed waitlist notified: %v", err)
	}
	return
}

func idsExpirados(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := testRepo.ListExpiredNotifiedWaitlist(context.Background())
	if err != nil {
		t.Fatalf("ListExpiredNotifiedWaitlist: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.CartID] = true
	}
	return out
}

// O caso do 1130: evento ABERTO (ends_at amanhã), promoção com TTL vencido.
// A query NÃO pode devolvê-la — expirar aqui é a violação da RN-04.
func TestFilaNaoExpiraComEventoAberto(t *testing.T) {
	requireDB(t)
	_, _, cartID := seedPromocaoVencida(t, "active", time.Now().UTC().Add(24*time.Hour))

	if idsExpirados(t)[cartID] {
		t.Fatal("promoção de EVENTO ABERTO entrou na varredura de expiração — viola a RN-04 (foi a cascata do 1130)")
	}
}

// Contraprova: com o evento ENCERRADO, o TTL vale e a promoção vencida expira
// normalmente (é o período de recuperação pós-fechamento).
func TestFilaExpiraComEventoEncerrado(t *testing.T) {
	requireDB(t)
	_, _, cartID := seedPromocaoVencida(t, "ended", time.Now().UTC().Add(-24*time.Hour))

	if !idsExpirados(t)[cartID] {
		t.Fatal("promoção vencida de EVENTO ENCERRADO NÃO expirou — a fila precisa andar no pós-fechamento")
	}
}

// Borda: evento ainda 'active' no status mas cujo ends_at JÁ passou (janela
// entre o fim e o sweep que marca 'ended'). O TTL vale — o evento acabou de
// fato, mesmo que o rótulo ainda não tenha sido atualizado.
func TestFilaExpiraQuandoEndsAtJaPassouMesmoStatusAtivo(t *testing.T) {
	requireDB(t)
	_, _, cartID := seedPromocaoVencida(t, "active", time.Now().UTC().Add(-10*time.Minute))

	if !idsExpirados(t)[cartID] {
		t.Fatal("evento com ends_at no passado deve permitir expiração da fila mesmo com status ainda 'active'")
	}
}
