package live

// RN-06 / A10 — o carrinho PAGO não transiciona no fechamento do evento.
//
// FinalizeCartsByEvent movia para 'checkout' TODO carrinho 'active', inclusive
// os já pagos: o filtro de payment_status incidia só no CASE do expires_at,
// nunca no WHERE. E o pagamento nunca muda carts.status (UpdateCartPayment só
// grava payment_status/paid_at e zera expires_at), então o carrinho pago chega
// aqui como 'active' e saía como 'checkout' — que no vocabulário novo significa
// "prazo correndo" para uma venda já concluída. De carona, gerava um
// cart.checkout_armed inútil que virava um ScheduleExpiry no-op.
//
// Com a decisão 7 (pagar DURANTE o evento) isso deixou de ser hipótese.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestFinalizeCartsSkipsPaidCart(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID, eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Finalize','fin-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title, type, cart_expiration_minutes)
		 VALUES ($1,'active','Semana','multi',60) RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	newCart := func(suffix, paymentStatus string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
			     status, payment_status, paid_at)
			 VALUES ($1,'u-'||$2,'@b'||$2,'tok-'||$2, ($3)::bigint % 100000,
			     'active', $4::text, CASE WHEN $4::text = 'paid' THEN now() ELSE NULL END)
			 RETURNING id::text`,
			eventID, suffix, n, paymentStatus,
		).Scan(&id); err != nil {
			t.Fatalf("seed cart %s: %v", suffix, err)
		}
		return id
	}
	openCart := newCart(n+"-open", "pending")
	paidCart := newCart(n+"-paid", "paid")

	count, err := testRepo.FinalizeCartsByEvent(ctx, eventID)
	if err != nil {
		t.Fatalf("FinalizeCartsByEvent: %v", err)
	}
	if count != 1 {
		t.Errorf("finalizou %d carrinho(s), queria 1 — o pago não pode entrar", count)
	}

	read := func(id string) (string, *time.Time) {
		t.Helper()
		var status string
		var expiresAt *time.Time
		if err := testPool.QueryRow(ctx,
			`SELECT status, expires_at FROM carts WHERE id = $1::uuid`, id,
		).Scan(&status, &expiresAt); err != nil {
			t.Fatalf("reler cart: %v", err)
		}
		return status, expiresAt
	}

	if status, expiresAt := read(openCart); status != "checkout" || expiresAt == nil {
		t.Errorf("carrinho aberto: status=%q expires_at=%v — queria checkout com prazo", status, expiresAt)
	}
	if status, expiresAt := read(paidCart); status != "active" || expiresAt != nil {
		t.Errorf("carrinho PAGO foi transicionado: status=%q expires_at=%v — venda concluída não ganha prazo", status, expiresAt)
	}

	// E o pago também não gera o fato: cart.checkout_armed nele seria um
	// ScheduleExpiry no-op, ruído no outbox e no worker.
	var armed int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM event_outbox WHERE dedup_key = 'cart.checkout_armed:' || $1`, paidCart,
	).Scan(&armed); err != nil {
		t.Fatalf("contar outbox: %v", err)
	}
	if armed != 0 {
		t.Errorf("carrinho pago gerou %d cart.checkout_armed", armed)
	}
}
