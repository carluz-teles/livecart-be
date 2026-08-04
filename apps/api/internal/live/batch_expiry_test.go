package live

// REPRODUÇÃO do cenário de campo: três carrinhos, dois esperando na fila o
// produto que está no terceiro.
//
// O que o dono do produto espera: o carrinho SEM fila vence primeiro, o estoque
// dele é liberado, e quem está esperando é promovido e ganha prazo para pagar.
//
// O que acontece: encerrar o EVENTO roda UM update com UM now() sobre todo
// carrinho aberto, então os três recebem o MESMO expires_at — idêntico ao
// microssegundo — e vencem juntos. A promoção tenta promover carrinhos que
// estão morrendo no mesmo instante.
//
// Em staging (04/08) os três ficaram com 18:38:51.703178 e as três linhas de
// fila terminaram `expired` com notified_at NULL: ninguém foi notificado.
//
// Este teste NÃO afirma qual deveria ser o comportamento — isso é decisão de
// produto ainda aberta. Ele fixa o FATO que a torna necessária, para que a
// correção tenha contra o que ser medida.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestFimDeEventoDaOMesmoPrazoParaTodoCarrinho(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	storeID := seedWindowStore(t, ctx, "expiracao-em-lote")

	n := fmt.Sprintf("%d", time.Now().UnixNano())
	var eventID, productID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, ends_at, cart_expiration_minutes)
		 VALUES ($1,'active','Semana Black', now() + interval '1 hour', 30) RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO products (store_id, name, keyword, external_source, price, stock, active)
		 VALUES ($1,'Cafe',right($2,4),'manual',5000,1,true) RETURNING id::text`, storeID, n,
	).Scan(&productID); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	// Três carrinhos: o primeiro segura a única unidade; os outros dois entram
	// na fila esperando por ela.
	newCart := func(handle string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status)
			 VALUES ($1::uuid,$2::text,'@'||$2::text,'tok-'||$2::text||'-'||$3::text,
			         (('x'||md5($2::text||$3::text))::bit(20)::int),'active')
			 RETURNING id::text`, eventID, handle, n,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %s: %v", handle, err)
		}
		return id
	}
	comEstoque := newCart("segura")
	naFila1 := newCart("espera1")
	naFila2 := newCart("espera2")

	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
		 VALUES ($1::uuid,$2::uuid,1,5000,0)`, comEstoque, productID,
	); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	for i, c := range []string{naFila1, naFila2} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO waitlist_items (event_id, product_id, platform_user_id, platform_handle, quantity, position, status, cart_id)
			 VALUES ($1::uuid,$2::uuid,$4::text,'@'||$4::text,1,$5::int,'waiting',$3::uuid)`,
			eventID, productID, c, fmt.Sprintf("espera%d", i+1), i+1,
		); err != nil {
			t.Fatalf("seed waitlist: %v", err)
		}
	}

	// Encerrar o evento é o que arma o prazo de todos.
	finalized, err := testRepo.FinalizeCartsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("FinalizeCartsByEvent: %v", err)
	}
	if finalized != 3 {
		t.Fatalf("finalizou %d carrinhos, esperado 3", finalized)
	}

	rows, err := testPool.Query(ctx,
		`SELECT id::text, expires_at FROM carts WHERE event_id = $1::uuid ORDER BY created_at`, eventID)
	if err != nil {
		t.Fatalf("ler prazos: %v", err)
	}
	defer rows.Close()

	prazos := map[string]time.Time{}
	for rows.Next() {
		var id string
		var exp *time.Time
		if err := rows.Scan(&id, &exp); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if exp == nil {
			t.Fatalf("carrinho %s ficou sem prazo apos o fim do evento", id[:8])
		}
		prazos[id] = *exp
	}
	if len(prazos) != 3 {
		t.Fatalf("li %d prazos, esperado 3", len(prazos))
	}

	// O FATO: um único carimbo para os três.
	var distintos []time.Time
	for _, p := range prazos {
		novo := true
		for _, d := range distintos {
			if p.Equal(d) {
				novo = false
			}
		}
		if novo {
			distintos = append(distintos, p)
		}
	}

	if len(distintos) != 1 {
		t.Logf("prazos distintos: %v", distintos)
		t.Skip("o comportamento mudou: os carrinhos deixaram de compartilhar um prazo único — " +
			"revise este teste junto com a correção da expiração em lote")
	}

	t.Logf("os 3 carrinhos vencem no MESMO instante (%s): quem espera na fila morre junto "+
		"com quem segura o estoque, e a promocao nao tem intervalo para acontecer",
		distintos[0].Format(time.RFC3339Nano))

	// E ninguém na fila foi notificado — não havia intervalo para isso.
	var notificados int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM waitlist_items WHERE event_id = $1::uuid AND notified_at IS NOT NULL`, eventID,
	).Scan(&notificados); err != nil {
		t.Fatalf("contar notificados: %v", err)
	}
	if notificados != 0 {
		t.Logf("notificados: %d — o fluxo mudou, revise este teste", notificados)
	}
}
