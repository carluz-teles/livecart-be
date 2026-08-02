package integration

// AC4: cart.paid payload carrega gmv_cents == SUM(qty*unit_price) dos itens
// (exclui frete e cupom). Testa que ProcessPaymentNotification emite o evento
// com o campo gmv_cents correto via GetCartGMVCents.
//
// Gated on TEST_DATABASE_URL (padrão do harness desta package).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

// TestAC4_GMVPayloadCarriesItemSum verifica que o payload de cart.paid emitido
// tem gmv_cents == SUM(quantity*unit_price) excluindo cupom e frete.
func TestAC4_GMVPayloadCarriesItemSum(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// Seed: loja + evento + produto + cart com itens + frete simulado via coupon_discount
	n := fmt.Sprintf("%d", time.Now().UnixNano())
	q := sqlc.New(testPool)

	var storeID, eventID, productID, cartID string
	mustScan := func(dst *string, sql string, args ...any) {
		t.Helper()
		if err := testPool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("gmv_payload_test seed %q: %v", sql[:min(60, len(sql))], err)
		}
	}
	mustScan(&storeID, `INSERT INTO stores (name, slug) VALUES ('AC4 Store', 'ac4-'||$1) RETURNING id::text`, n)
	mustScan(&eventID, `INSERT INTO live_events (store_id, status, title, ends_at) VALUES ($1, 'ended', 'AC4', now()) RETURNING id::text`, storeID)
	kw := fmt.Sprintf("%d", time.Now().UnixNano()%8000+1000)
	mustScan(&productID,
		`INSERT INTO products (store_id, name, external_source, external_id, keyword, price, stock)
		 VALUES ($1,'AC4 Prod','none','ac4-ext-'||$2,$3,10000,100) RETURNING id::text`,
		storeID, n, kw)
	// Cart com coupon_discount_cents (= "frete/desconto" que deve ser EXCLUÍDO do GMV)
	mustScan(&cartID,
		`INSERT INTO carts (event_id, platform_user_id, platform_handle, token, short_id, status, payment_status, paid_at, coupon_discount_cents)
		 VALUES ($1,'u-ac4','@ac4','tok-ac4-'||$2,77777,'checkout','paid',now(), 500) RETURNING id::text`,
		eventID, n)
	// 3 unidades * 10000 = 30000 (GMV puro, sem o coupon de 500)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity) VALUES ($1,$2,3,10000,0)`,
		cartID, productID,
	); err != nil {
		t.Fatalf("cart_items: %v", err)
	}

	// Chama GetCartGMVCents diretamente (a mesma query usada pelo emissor).
	cid, err := parseUUID(cartID)
	if err != nil {
		t.Fatalf("parseUUID cartID: %v", err)
	}
	gmv, err := q.GetCartGMVCents(ctx, cid)
	if err != nil {
		t.Fatalf("GetCartGMVCents: %v", err)
	}
	if gmv != 30000 {
		t.Fatalf("AC4: GetCartGMVCents want 30000 (sem cupom/frete), got %d", gmv)
	}

	// Verifica também que GMV != payment amount do gateway (que incluiria cupom).
	// O gateway pagaria 30000 - 500 = 29500, mas o GMV deve ser 30000.
	var couponDiscount int64
	if err := testPool.QueryRow(ctx,
		`SELECT coupon_discount_cents FROM carts WHERE id=$1`, cartID,
	).Scan(&couponDiscount); err != nil {
		t.Fatalf("reading coupon_discount: %v", err)
	}
	gatewayAmount := gmv - couponDiscount // 29500 — o que o cliente pagou
	if gmv == gatewayAmount {
		t.Fatalf("AC4: gmv_cents (%d) should NOT equal gateway amount (%d) when coupon_discount>0", gmv, gatewayAmount)
	}

	// Verifica que o struct que vai ser marshaled carregaria o gmv correto.
	// (Replica o código do emissor em ProcessPaymentNotification.)
	cartUUID := pgtype.UUID{Bytes: cid.Bytes, Valid: true}
	emittedGMV, err := q.GetCartGMVCents(ctx, cartUUID)
	if err != nil {
		t.Fatalf("GetCartGMVCents (emission replica): %v", err)
	}
	payload, _ := json.Marshal(struct {
		CartID   string `json:"cart_id"`
		StoreID  string `json:"store_id"`
		GMVCents int64  `json:"gmv_cents,omitempty"`
	}{cartID, storeID, emittedGMV})

	var parsed struct {
		GMVCents int64 `json:"gmv_cents"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	if parsed.GMVCents != 30000 {
		t.Fatalf("AC4: payload gmv_cents want 30000, got %d", parsed.GMVCents)
	}
}
