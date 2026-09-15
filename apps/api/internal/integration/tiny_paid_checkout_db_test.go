package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/database"
)

// Use the application's pool configuration, not pgx's prepared-statement
// default: production serializes parameters with the simple query protocol.
func tinyCheckoutProductionRepository(t *testing.T) *Repository {
	t.Helper()
	requireDB(t)
	pool, err := database.NewPool(t.Context(), testPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if pool.Config().ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatal("Tiny regression tests must exercise the production query protocol")
	}
	return NewRepository(sqlc.New(pool), pool)
}

func TestTinyCheckoutLoadsSeparateFreightAndCardSchedule(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedPaidCart(t, 2, 0)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	exec(`UPDATE carts SET shipping_cost_cents=900,shipping_cost_real_cents=1800,
 shipping_carrier='Correios',shipping_service_name='PAC',customer_name='Teste',customer_phone='11900000000',
 shipping_address='{"street":"Rua Teste","number":"10","zipCode":"01001000","city":"São Paulo","state":"SP"}' WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at)
 VALUES($1,1800,2000,'credit_card','card-1','2026-09-14T15:00:00Z'),($1,900,900,'pix','freight-1','2026-09-14T16:00:00Z')`, fx.cartID)
	exec(`UPDATE order_payments p SET gateway_snapshot='{"payment_id":"card-1","installments":2,"money_release_date":"2026-09-16T15:00:00Z"}'
 FROM orders o WHERE o.id=p.order_id AND o.cart_id=$1`, fx.cartID)
	svc := &Service{repo: tinyCheckoutProductionRepository(t)}
	got, err := svc.loadTinyPaidCheckout(ctx, fx.cartID, fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FreightCents != 900 || got.Shipping.CostCents != 1800 || got.DiscountCents != 200 || len(got.Payments) != 3 {
		t.Fatalf("snapshot %+v", got)
	}
	if got.Payments[0].AmountCents != 900 || got.Payments[0].DueDate.Format("2006-01-02") != "2026-09-16" || got.Payments[1].DueDate.Format("2006-01-02") != "2026-10-16" || got.Payments[2].Method != "pix" {
		t.Fatalf("payments %+v", got.Payments)
	}
	if got.Address.Phone != "11900000000" || got.Address.RecipientName != "Teste" {
		t.Fatalf("address %+v", got.Address)
	}
	if _, err := svc.loadTinyPaidCheckout(ctx, fx.cartID, uuid.NewString()); err == nil {
		t.Fatal("cross-store snapshot allowed")
	}
}

func TestTinyCheckoutJournalPreservesClaimIsolationAndAtomicBinding(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 1, 0)
	var integrationID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM integrations WHERE store_id=$1`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	j := &tinyCheckoutJournal{repo: tinyCheckoutProductionRepository(t), cartID: fx.cartID, storeID: fx.storeID, integrationID: integrationID, sourceID: "1"}
	op := &providers.TinyCheckoutOperation{ID: uuid.NewString(), CartID: fx.cartID, SourceID: "1", TargetID: "2", TargetNumber: "102", StartedAt: time.Now(),
		SourceStockLaunched: true, StockReverseStarted: true, StockReversed: true, StockLaunchStarted: true, StockLaunched: true,
		TargetStatus: providers.ERPOrderStatusFaturado,
		Order:        providers.ERPOrder{Checkout: &providers.ERPOrderCheckout{Payments: []providers.ERPInstallment{{AmountCents: 1000, DueDate: time.Now()}}}}}
	if err := j.Save(ctx, op); err != nil {
		t.Fatal(err)
	}
	loaded, err := j.load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != op.ID || !loaded.StockReversed || !loaded.StockLaunched || !tinySameSnapshot(loaded.Order.Checkout, op.Order.Checkout) {
		t.Fatal("checkpoint changed across JSON roundtrip")
	}
	other := *op
	other.ID = uuid.NewString()
	if err := j.Save(ctx, &other); err == nil {
		t.Fatal("two active finalizations allowed")
	}
	foreign := *j
	foreign.storeID = uuid.NewString()
	if err := foreign.Save(ctx, op); err == nil {
		t.Fatal("foreign store saved checkpoint")
	}
	if err := j.Bind(ctx, op); err == nil {
		t.Fatal("bound cart without mutating claim")
	}
	if _, err := testPool.Exec(ctx, `UPDATE carts SET external_order_id='1',erp_order_state='mutating' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	if err := j.Bind(ctx, op); err != nil {
		t.Fatal(err)
	}
	var cartID, paymentID, status string
	var complete, stockLaunched bool
	if err := testPool.QueryRow(ctx, `SELECT c.external_order_id,p.external_order_id,t.completed,c.erp_order_status,c.erp_stock_launched FROM carts c
	 JOIN orders o ON o.cart_id=c.id JOIN order_payments p ON p.order_id=o.id JOIN tiny_checkout_operations t ON t.cart_id=c.id WHERE c.id=$1`, fx.cartID).Scan(&cartID, &paymentID, &complete, &status, &stockLaunched); err != nil {
		t.Fatal(err)
	}
	if cartID != "2" || paymentID != "2" || !complete || !stockLaunched || status != "faturado" {
		t.Fatal("partial bind")
	}
	if err := j.Bind(ctx, op); err != nil {
		t.Fatal(err)
	}
	var observations int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM erp_order_status_events WHERE cart_id=$1 AND status='faturado' AND source='reconciliation'`, fx.cartID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observations != 1 {
		t.Fatalf("expected one durable status observation, got %d", observations)
	}
}

type tinyJournalFinalizer struct {
	providers.ERPProvider
	calls         int
	sourceAnchors []string
	keepSource    bool
}

func (p *tinyJournalFinalizer) FinalizePaidCheckout(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal) (*providers.OrderResult, error) {
	if !op.Completed {
		p.calls++
		p.sourceAnchors = append(p.sourceAnchors, op.SourceAnchor)
		op.TargetID = "test-final-" + op.ID
		op.Replace = true
		if p.keepSource {
			op.TargetID, op.TargetStatus, op.Replace = op.SourceID, providers.ERPOrderStatusFaturado, false
		}
		if err := journal.Bind(ctx, op); err != nil {
			return nil, err
		}
	}
	return &providers.OrderResult{OrderID: op.TargetID}, nil
}

func TestTinyAdditionalPaymentPreservesReconciledOriginalMarker(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	exec(`UPDATE carts SET external_order_id='1',erp_order_state='mutating' WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,1000,1000,'pix','first',now())`, fx.cartID)
	svc := &Service{repo: tinyCheckoutProductionRepository(t)}
	provider := &tinyJournalFinalizer{keepSource: true}
	if _, err := svc.PrepareTinyPaidOrder(t.Context(), provider, fx.cartID, fx.storeID, "1"); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE carts SET shipping_cost_cents=900 WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,900,900,'pix','freight',now())`, fx.cartID)
	if _, err := svc.PrepareTinyPaidOrder(t.Context(), provider, fx.cartID, fx.storeID, "1"); err != nil {
		t.Fatal(err)
	}
	if len(provider.sourceAnchors) != 2 || provider.sourceAnchors[1] != "lc-cart-"+fx.cartID {
		t.Fatalf("read-only reconciliation invented a replacement marker: %v", provider.sourceAnchors)
	}
}

func TestTinyAdditionalPaymentKeepsReplacementOwnership(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	ctx := t.Context()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	exec(`UPDATE carts SET external_order_id='1',erp_order_state='mutating' WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,1000,1000,'pix','first-payment',now())`, fx.cartID)
	svc := &Service{repo: tinyCheckoutProductionRepository(t)}
	provider := &tinyJournalFinalizer{}
	first, err := svc.PrepareTinyPaidOrder(ctx, provider, fx.cartID, fx.storeID, "1")
	if err != nil {
		t.Fatal(err)
	}
	var marker string
	if err := testPool.QueryRow(ctx, `SELECT progress->'Order'->>'external_id' FROM tiny_checkout_operations WHERE cart_id=$1`, fx.cartID).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if len("lc-cart-"+marker) > 50 {
		t.Fatal("Tiny marker will be truncated")
	}
	again, err := svc.PrepareTinyPaidOrder(ctx, provider, fx.cartID, fx.storeID, first)
	if err != nil || again != first || provider.calls != 1 {
		t.Fatalf("replay recreated order: %s %v", again, err)
	}
	// Recreate the database state left by an interruption after the remote POST
	// succeeded but before its binding transaction committed.
	exec(`UPDATE tiny_checkout_operations SET completed=false,progress=jsonb_set(progress,'{Completed}','false'::jsonb) WHERE cart_id=$1`, fx.cartID)
	exec(`UPDATE carts SET external_order_id='1',erp_order_state='mutating' WHERE id=$1`, fx.cartID)
	exec(`UPDATE carts SET shipping_cost_cents=900 WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,900,900,'pix','additional-freight',now())`, fx.cartID)
	second, err := svc.PrepareTinyPaidOrder(ctx, provider, fx.cartID, fx.storeID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if second == first || provider.calls != 3 || provider.sourceAnchors[0] != "lc-cart-"+fx.cartID || provider.sourceAnchors[2] != "lc-cart-"+marker {
		t.Fatalf("lost replacement ownership: %+v", provider.sourceAnchors)
	}
}
