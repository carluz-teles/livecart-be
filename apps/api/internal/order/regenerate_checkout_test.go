package order_test

// RN-06 / A10 — o "regerar link" era o segundo escritor que violava o
// invariante de estado do carrinho, e o pior dos dois:
//
//   - devolvia o carrinho para status='active' JÁ COM expires_at no futuro. No
//     vocabulário novo 'active' é "pode pagar, SEM prazo" e 'checkout' é "prazo
//     correndo" — o estado antigo era impossível de representar;
//   - era um Exec cru: não abria transação, não emitia fato nenhum. Sem
//     cart.checkout_armed, armCartExpiry nunca rodava e nenhuma task
//     cart.expire era agendada. Como o sweep de carrinhos foi removido no
//     commit 9a0df99, esse carrinho NUNCA expirava: ficava vivo com prazo
//     vencido, segurando o estoque reservado dos seus itens. É o R5 do pacote.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/order"
)

// seedRegenCart cria loja → evento → carrinho não pago pronto para o regenerate.
func seedRegenCart(t *testing.T, status string) (cartID string) {
	t.Helper()
	ctx := context.Background()
	n := fmt.Sprintf("%d", time.Now().UnixNano())

	var storeID, eventID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO stores (name, slug) VALUES ('Regen', 'regen-'||$1) RETURNING id::text`, n,
	).Scan(&storeID); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO live_events (store_id, status, title) VALUES ($1,'ended','Ev') RETURNING id::text`, storeID,
	).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id,
		    status, payment_status, expires_at)
		 VALUES ($1,'u-'||$2,'@b'||$2,'tok-'||$2, ($2)::bigint % 100000,
		    $3,'pending', now() - interval '1 hour') RETURNING id::text`,
		eventID, n, status,
	).Scan(&cartID); err != nil {
		t.Fatalf("seed cart: %v", err)
	}
	return cartID
}

func TestRegenerateCheckoutLandsInCheckoutAndArmsExpiry(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	cartID := seedRegenCart(t, "expired")
	newExpiry := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Second)

	if err := repo.RegenerateCheckout(ctx, cartID, newExpiry); err != nil {
		t.Fatalf("RegenerateCheckout: %v", err)
	}

	var status, paymentStatus string
	var expiresAt *time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT status, payment_status, expires_at FROM carts WHERE id = $1::uuid`, cartID,
	).Scan(&status, &paymentStatus, &expiresAt); err != nil {
		t.Fatalf("reler cart: %v", err)
	}
	// 'active' com expires_at no futuro é o estado impossível da RN-06.
	if status != "checkout" {
		t.Errorf("status = %q, queria \"checkout\" (prazo correndo)", status)
	}
	if paymentStatus != "pending" {
		t.Errorf("payment_status = %q, queria \"pending\"", paymentStatus)
	}
	if expiresAt == nil || !expiresAt.UTC().Equal(newExpiry) {
		t.Errorf("expires_at = %v, queria %v", expiresAt, newExpiry)
	}

	// O fato é o que arma a task cart.expire. Sem ele o carrinho nunca expira.
	dedup := fmt.Sprintf("cart.checkout_armed:%s:regen:%d", cartID, newExpiry.Unix())
	if n := outboxCount(t, string(events.CartCheckoutArmed), dedup); n != 1 {
		t.Fatalf("event_outbox tem %d linha(s) de cart.checkout_armed, queria 1 — sem o fato nada agenda cart.expire", n)
	}
	var payload struct {
		CartID string `json:"cart_id"`
	}
	if err := json.Unmarshal(outboxPayload(t, dedup), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.CartID != cartID {
		t.Errorf("payload.cart_id = %q, queria %q", payload.CartID, cartID)
	}
}

func TestRegenerateCheckoutTwiceEmitsTwice(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	repo := order.NewRepository(testPool)

	cartID := seedRegenCart(t, "expired")
	first := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	second := first.Add(30 * time.Minute)

	if err := repo.RegenerateCheckout(ctx, cartID, first); err != nil {
		t.Fatalf("primeiro regenerate: %v", err)
	}
	if err := repo.RegenerateCheckout(ctx, cartID, second); err != nil {
		t.Fatalf("segundo regenerate: %v", err)
	}

	// A dedup key precisa variar com o prazo novo. Se reusasse
	// 'cart.checkout_armed:<id>' (a do fechamento do evento), o outbox
	// descartaria a segunda emissão em silêncio e o carrinho voltaria a ser o
	// que nunca expira.
	for _, at := range []time.Time{first, second} {
		dedup := fmt.Sprintf("cart.checkout_armed:%s:regen:%d", cartID, at.Unix())
		if n := outboxCount(t, string(events.CartCheckoutArmed), dedup); n != 1 {
			t.Errorf("prazo %v: %d linha(s) no outbox, queria 1", at, n)
		}
	}
}
