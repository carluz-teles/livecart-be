//go:build integration

package checkout

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/cartpricing"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	paymentdomain "livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/httpx"
)

func TestPaymentQuote_PartialPromotionAfterChargeRequiresReviewWithoutCoveringNewUnits(t *testing.T) {
	for _, method := range []string{"pix", "credit_card"} {
		t.Run(method, func(t *testing.T) {
			f := seedPriceLotCart(t)
			addPriceLot(t, f, 3, 2, 1001)
			requestPriceLot(t, f, 2, 1001)
			integrationID := priceLotPaymentIntegration(t, f.store)
			attempt, err := testRepo.createPaymentAttempt(t.Context(), testPool, f.cart, integrationID,
				"pagarme", method, 1001, []providers.CheckoutItem{{ID: f.product, Quantity: 1, UnitPrice: 1001}})
			if err != nil {
				t.Fatal(err)
			}
			paymentID := "promotion-" + attempt
			if err := testRepo.bindPaymentAttempt(t.Context(), testPool, attempt, paymentID, paymentID); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(t.Context(), `UPDATE products SET stock=1 WHERE id=$1`, f.product); err != nil {
				t.Fatal(err)
			}
			repo := integration.NewRepository(testQueries, testPool)
			promotion, err := repo.PromoteNextWaitlistEntry(t.Context(), f.store, f.product)
			if err != nil || promotion == nil || promotion.Quantity != 1 || promotion.Remaining != 1 {
				t.Fatalf("partial promotion=%+v err=%v", promotion, err)
			}
			payload, err := json.Marshal(map[string]string{"cart_id": f.cart, "store_id": f.store, "payment_id": paymentID})
			if err != nil {
				t.Fatal(err)
			}
			fact := events.Envelope{Name: events.CartPaid, Source: "pagarme", DedupKey: "cart.paid:" + paymentID, Payload: payload}
			_, err = repo.UpdateCartPaymentStatus(t.Context(), f.cart, "paid", paymentID, nil, method, 1001, fact)
			if !errors.Is(err, paymentdomain.ErrPaymentReviewRequired) {
				t.Fatalf("obsolete charge should require review, got %v", err)
			}
			var review bool
			var status, attemptStatus string
			var paidAmount, covered int64
			var paidQuantity, waiting, paidFacts int
			if err := testPool.QueryRow(t.Context(), `SELECT c.payment_review_required,c.payment_status,c.paid_amount_cents,
				ci.paid_quantity,ci.waitlisted_quantity,cp.gross_covered_cents,pa.status,
				(SELECT count(*) FROM event_outbox WHERE dedup_key=$2)
				FROM carts c JOIN cart_items ci ON ci.cart_id=c.id JOIN cart_payments cp ON cp.cart_id=c.id
				JOIN payment_attempts pa ON pa.cart_id=c.id WHERE c.id=$1`, f.cart, fact.DedupKey).Scan(
				&review, &status, &paidAmount, &paidQuantity, &waiting, &covered, &attemptStatus, &paidFacts,
			); err != nil {
				t.Fatal(err)
			}
			if !review || status != "pending" || paidAmount != 1001 || paidQuantity != 0 || waiting != 1 || covered != 0 || attemptStatus != "review_required" || paidFacts != 0 {
				t.Fatalf("obsolete quote: review=%v status=%s money=%d paidQty=%d waiting=%d covered=%d attempt=%s paidFacts=%d",
					review, status, paidAmount, paidQuantity, waiting, covered, attemptStatus, paidFacts)
			}
			expectPriceLots(t, f, []cartpricing.Lot{{Quantity: 3, WaitlistedQuantity: 1, UnitPrice: 1001, TotalPrice: 2002}}, 2002)
		})
	}
}

func TestPaymentQuote_PaidLedgerCoversOnlyAvailableLots(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 3, 2, 1001)
	requestPriceLot(t, f, 2, 1001)
	addPriceLot(t, f, 2, 1, 2002)
	requestPriceLot(t, f, 1, 2002)
	_, err := testQueries.UpdateCartPayment(t.Context(), sqlc.UpdateCartPaymentParams{
		CartID: priceLotUUID(f.cart), PaymentStatus: pgtype.Text{String: "paid", Valid: true},
		CheckoutID: "price-lots-paid", PaidAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		PaymentMethod: pgtype.Text{String: "pix", Valid: true}, AmountCents: 3003,
	})
	if err != nil {
		t.Fatal(err)
	}
	var paid, gross, unpaid int64
	var quantity, paidQuantity, waiting int
	if err := testPool.QueryRow(t.Context(), `SELECT c.paid_amount_cents,cp.gross_covered_cents,
		cart_unpaid_total_cents(c.id),ci.quantity,ci.paid_quantity,ci.waitlisted_quantity
		FROM carts c JOIN cart_payments cp ON cp.cart_id=c.id JOIN cart_items ci ON ci.cart_id=c.id
		WHERE c.id=$1`, f.cart).Scan(&paid, &gross, &unpaid, &quantity, &paidQuantity, &waiting); err != nil {
		t.Fatal(err)
	}
	if paid != 3003 || gross != 3003 || unpaid != 0 || quantity != 2 || paidQuantity != 2 || waiting != 0 {
		t.Fatalf("payment ledger paid=%d gross=%d unpaid=%d quantity=%d paidQty=%d waiting=%d",
			paid, gross, unpaid, quantity, paidQuantity, waiting)
	}
}

func priceLotPaymentIntegration(t *testing.T, store string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO integrations(store_id,type,provider,status)
		VALUES($1,'payment','pagarme','active') RETURNING id::text`, store).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPaymentQuote_PriceLotsPreserveExactCentsAndRejectSameTotalSwaps(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 3, 2, 1001)
	addPriceLot(t, f, 1, 0, 2002)
	integrationID := priceLotPaymentIntegration(t, f.store)
	valid := []providers.CheckoutItem{
		{ID: f.product, Name: "Price lot", Quantity: 1, UnitPrice: 1001},
		{ID: f.product, Name: "Price lot", Quantity: 1, UnitPrice: 2002},
	}
	for _, method := range []string{"pix", "credit_card"} {
		t.Run(method, func(t *testing.T) {
			attempt, err := testRepo.createPaymentAttempt(
				t.Context(), testPool, f.cart, integrationID, "pagarme", method, 3003, valid,
			)
			if err != nil || attempt == "" {
				t.Fatalf("exact mixed-price quote: attempt=%s err=%v", attempt, err)
			}
			var amount int64
			var fingerprintMatches bool
			if err := testPool.QueryRow(t.Context(), `SELECT amount_cents,
				cart_fingerprint=cart_payment_fingerprint(cart_id)
				FROM payment_attempts WHERE id=$1`, attempt).Scan(&amount, &fingerprintMatches); err != nil {
				t.Fatal(err)
			}
			if amount != 3003 || !fingerprintMatches {
				t.Fatalf("frozen quote amount=%d fingerprintMatches=%v", amount, fingerprintMatches)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		amount int64
	}{
		{name: "one cent below the quote", amount: 3002},
		{name: "one cent above the quote", amount: 3004},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testRepo.createPaymentAttempt(
				t.Context(), testPool, f.cart, integrationID, "pagarme", "pix", tc.amount, valid,
			)
			var domain *httpx.ServiceError
			if !errors.As(err, &domain) || domain.Reason != string(httpx.CodeCartItemChanged) {
				t.Fatalf("inexact charge accepted or wrong rejection: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		items []providers.CheckoutItem
	}{
		{name: "same total changed prices", items: []providers.CheckoutItem{
			{ID: f.product, Quantity: 1, UnitPrice: 1002},
			{ID: f.product, Quantity: 1, UnitPrice: 2001},
		}},
		{name: "duplicate line", items: append(append([]providers.CheckoutItem{}, valid...), valid[0])},
		{name: "waiting units included", items: []providers.CheckoutItem{
			{ID: f.product, Quantity: 3, UnitPrice: 1001},
			{ID: f.product, Quantity: 1, UnitPrice: 2002},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testRepo.createPaymentAttempt(
				t.Context(), testPool, f.cart, integrationID, "pagarme", "pix", 3003, tc.items,
			)
			var domain *httpx.ServiceError
			if !errors.As(err, &domain) || domain.Reason != string(httpx.CodeCartItemChanged) {
				t.Fatalf("invalid gateway items accepted or wrong rejection: %v", err)
			}
		})
	}
}

func TestPaymentFingerprint_SinglePriceRemainsCompatibleWithPendingLegacyQuote(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 2, 1, 1001)
	addPriceLot(t, f, 1, 0, 1001)
	var current, legacy string
	if err := testPool.QueryRow(t.Context(), `SELECT cart_payment_fingerprint(c.id),md5(jsonb_build_object(
		'items',(SELECT jsonb_agg(jsonb_build_array(product_id,quantity,waitlisted_quantity,paid_quantity,unit_price)
		 ORDER BY product_id) FROM cart_items WHERE cart_id=c.id),
		'shipping',c.shipping_cost_cents,'coupon',c.coupon_discount_cents,
		'pix_discount',(SELECT pix_discount_percent FROM live_events WHERE id=c.event_id)
	)::text) FROM carts c WHERE c.id=$1`, f.cart).Scan(&current, &legacy); err != nil {
		t.Fatal(err)
	}
	if current != legacy {
		t.Fatalf("unchanged single-price cart invalidates an existing gateway attempt: %s != %s", current, legacy)
	}
}

func TestPaymentFingerprint_MixedPriceSwapDetectedEvenWhenQuantityAndTotalAreUnchanged(t *testing.T) {
	f := seedPriceLotCart(t)
	addPriceLot(t, f, 1, 0, 1001)
	addPriceLot(t, f, 1, 0, 2002)
	var before, after string
	if err := testPool.QueryRow(t.Context(), `SELECT cart_payment_fingerprint($1)`, f.cart).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), `UPDATE cart_item_price_lots
		SET unit_price=CASE unit_price WHEN 1001 THEN 1002 ELSE 2001 END
		WHERE cart_item_id IN (SELECT id FROM cart_items WHERE cart_id=$1)`, f.cart); err != nil {
		t.Fatal(err)
	}
	var amount int64
	if err := testPool.QueryRow(t.Context(), `SELECT cart_payment_fingerprint($1),cart_available_total_cents($1)`, f.cart).Scan(&after, &amount); err != nil {
		t.Fatal(err)
	}
	if amount != 3003 || before == after {
		t.Fatalf("same-total swap not identified: amount=%d before=%s after=%s", amount, before, after)
	}
}
