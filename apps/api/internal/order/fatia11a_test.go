package order_test

// Fatia 11a: os listeners da Order emitem os fatos de barramento order.paid /
// order.refunded (aditivos — sem consumidor ainda) de forma TRANSACIONAL junto
// da materialização/flip, via outbox, com dedup por order_id (exactly-once).
// Gated em TEST_DATABASE_URL (reusa a infra do on_cart_paid_test.go:
// seedPaidCart / seedMaterialisedOrder / newListener / requireDB).
//
// Invariantes travadas:
//   E1 OnCartPaid materializa a Order E emite 1 order.paid no event_outbox,
//      dedup_key="order.paid:<order_id>", payload {order_id,cart_id,store_id,gmv_cents}.
//   E2 replay de OnCartPaid não duplica a linha do outbox (exactly-once).
//   E3 OnCartRefunded emite 1 order.refunded, dedup_key="order.refunded:<order_id>",
//      payload {order_id,cart_id,store_id}.
//   E4 replay de OnCartRefunded não duplica.
//   E5 OnCartCancelled NÃO emite order.refunded (não há order.cancelled no barramento).
//   E6 o dedup do outbox (ON CONFLICT (dedup_key)) colapsa emissões repetidas.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"livecart/apps/api/internal/events"
)

// orderIDByCart resolve o id da Order materializada para o cart.
func orderIDByCart(t *testing.T, cartID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id::text FROM orders WHERE cart_id = $1`, cartID).Scan(&id); err != nil {
		t.Fatalf("orderIDByCart(%s): %v", cartID, err)
	}
	return id
}

// outboxCount conta linhas do event_outbox por (name, dedup_key).
func outboxCount(t *testing.T, name, dedupKey string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM event_outbox WHERE name = $1 AND dedup_key = $2`,
		name, dedupKey).Scan(&n); err != nil {
		t.Fatalf("outboxCount(%s,%s): %v", name, dedupKey, err)
	}
	return n
}

// outboxPayload lê o payload da (única) linha do outbox com o dedup_key.
func outboxPayload(t *testing.T, dedupKey string) []byte {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT payload FROM event_outbox WHERE dedup_key = $1`, dedupKey).Scan(&raw); err != nil {
		t.Fatalf("outboxPayload(%s): %v", dedupKey, err)
	}
	return raw
}

// ─── E1 order.paid emitido na materialização ─────────────────────────────────

func TestFatia11a_OnCartPaid_EmitsOrderPaid(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 2, 5000, 0, 0) // gmv = 10000
	if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 10000, nil); err != nil {
		t.Fatalf("OnCartPaid: %v", err)
	}

	orderID := orderIDByCart(t, r.cartID)
	key := "order.paid:" + orderID
	if n := outboxCount(t, string(events.OrderPaid), key); n != 1 {
		t.Fatalf("E1: want 1 order.paid outbox row, got %d", n)
	}

	var p struct {
		OrderID  string `json:"order_id"`
		CartID   string `json:"cart_id"`
		StoreID  string `json:"store_id"`
		GMVCents int64  `json:"gmv_cents"`
	}
	if err := json.Unmarshal(outboxPayload(t, key), &p); err != nil {
		t.Fatalf("E1: unmarshal payload: %v", err)
	}
	if p.OrderID != orderID || p.CartID != r.cartID || p.StoreID != r.storeID || p.GMVCents != 10000 {
		t.Errorf("E1: payload=%+v; want order=%s cart=%s store=%s gmv=10000",
			p, orderID, r.cartID, r.storeID)
	}
}

// ─── E2 replay não duplica (exactly-once via short-circuit + dedup) ───────────

func TestFatia11a_OnCartPaid_ExactlyOnceOnReplay(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedPaidCart(t, 1, 8000, 0, 0)
	for i := 0; i < 3; i++ {
		if err := l.OnCartPaid(ctx, r.cartID, r.storeID, 8000, nil); err != nil {
			t.Fatalf("OnCartPaid call %d: %v", i+1, err)
		}
	}

	key := "order.paid:" + orderIDByCart(t, r.cartID)
	if n := outboxCount(t, string(events.OrderPaid), key); n != 1 {
		t.Fatalf("E2: want exactly 1 order.paid row after replay, got %d", n)
	}
}

// ─── E3 order.refunded emitido no flip refunded ──────────────────────────────

func TestFatia11a_OnCartRefunded_EmitsOrderRefunded(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)
	orderID := orderIDByCart(t, r.cartID)

	if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("OnCartRefunded: %v", err)
	}

	key := "order.refunded:" + orderID
	if n := outboxCount(t, string(events.OrderRefunded), key); n != 1 {
		t.Fatalf("E3: want 1 order.refunded row, got %d", n)
	}

	var p struct {
		OrderID string `json:"order_id"`
		CartID  string `json:"cart_id"`
		StoreID string `json:"store_id"`
	}
	if err := json.Unmarshal(outboxPayload(t, key), &p); err != nil {
		t.Fatalf("E3: unmarshal payload: %v", err)
	}
	if p.OrderID != orderID || p.CartID != r.cartID || p.StoreID != r.storeID {
		t.Errorf("E3: payload=%+v; want order=%s cart=%s store=%s", p, orderID, r.cartID, r.storeID)
	}
}

// ─── E4 replay do refunded não duplica ───────────────────────────────────────

func TestFatia11a_OnCartRefunded_ExactlyOnceOnReplay(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)
	orderID := orderIDByCart(t, r.cartID)

	for i := 0; i < 3; i++ {
		if err := l.OnCartRefunded(ctx, r.cartID, r.storeID); err != nil {
			t.Fatalf("OnCartRefunded call %d: %v", i+1, err)
		}
	}

	key := "order.refunded:" + orderID
	if n := outboxCount(t, string(events.OrderRefunded), key); n != 1 {
		t.Fatalf("E4: want exactly 1 order.refunded row after replay, got %d", n)
	}
}

// ─── E5 cancelled NÃO emite order.refunded ───────────────────────────────────

func TestFatia11a_OnCartCancelled_DoesNotEmitOrderRefunded(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	l := newListener(t)

	r := seedMaterialisedOrder(t, l)
	orderID := orderIDByCart(t, r.cartID)

	if err := l.OnCartCancelled(ctx, r.cartID, r.storeID); err != nil {
		t.Fatalf("OnCartCancelled: %v", err)
	}

	// cancelled compartilha applyTerminalStatus com refunded, mas não há
	// order.cancelled no barramento nesta fatia — a emissão é só para refunded.
	if n := outboxCount(t, string(events.OrderRefunded), "order.refunded:"+orderID); n != 0 {
		t.Fatalf("E5: cancelled must not emit order.refunded, got %d rows", n)
	}
}

// ─── E6 dedup do outbox colapsa emissões repetidas (backstop de exactly-once) ─

func TestFatia11a_OutboxDedupCollapsesRepeatEmits(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	key := fmt.Sprintf("order.paid:dedup-%d", time.Now().UnixNano())
	payload := map[string]any{"order_id": "x", "cart_id": "y", "store_id": "z"}
	for i := 0; i < 2; i++ {
		if err := events.EmitInternal(ctx, testQueries, events.OrderPaid, key, payload); err != nil {
			t.Fatalf("EmitInternal call %d: %v", i+1, err)
		}
	}

	if n := outboxCount(t, string(events.OrderPaid), key); n != 1 {
		t.Fatalf("E6: outbox dedup must collapse to 1 row, got %d", n)
	}
}
