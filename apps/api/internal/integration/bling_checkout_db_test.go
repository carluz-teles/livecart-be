//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestBlingCheckoutLoadsChargedFreightAndLedgerFromDatabase(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fx := seedPaidCart(t, 2, 0)
	svc := &Service{repo: testRepo}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_source='bling',external_id='123' WHERE id=$1`, fx.productID)
	exec(`UPDATE carts SET shipping_cost_cents=900,shipping_cost_real_cents=1800,
 shipping_carrier='Correios',shipping_service_name='PAC',shipping_deadline_days=5,
 customer_name='Comprador Teste',shipping_address='{"zipCode":"01001000","street":"Rua Teste","number":"10","city":"São Paulo","state":"SP"}'::jsonb WHERE id=$1`, fx.cartID)
	if _, err := svc.LoadERPOrderCheckout(ctx, fx.cartID, fx.storeID); err == nil {
		t.Fatal("accepted paid cart with no payment ledger")
	}
	exec(`INSERT INTO cart_payments (cart_id,amount_cents,gross_covered_cents,method,checkout_id)
 VALUES ($1,1800,2000,'pix','bling-checkout-items'),($1,900,900,'pix','bling-checkout-freight')`, fx.cartID)
	got, err := svc.LoadERPOrderCheckout(ctx, fx.cartID, fx.storeID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FreightCents != 900 || got.DiscountCents != 200 || got.Shipping == nil || got.Shipping.CostCents != 1800 || got.Shipping.Carrier != "Correios" || got.Shipping.DeadlineDays != 5 {
		t.Fatalf("commercial snapshot: %+v", got)
	}
	if got.Address == nil || got.Address.Street != "Rua Teste" || got.Address.ZipCode != "01001000" || got.Address.RecipientName != "Comprador Teste" {
		t.Fatalf("delivery address: %+v", got.Address)
	}
	var paid int64
	for _, p := range got.Payments {
		paid += p.AmountCents
		if p.Method != "pix" || p.DueDate.IsZero() {
			t.Fatalf("payment identity lost: %+v", p)
		}
	}
	if len(got.Payments) != 2 || paid != 2700 {
		t.Fatalf("payment ledger: %+v", got.Payments)
	}
	if _, err := svc.LoadERPOrderCheckout(ctx, fx.cartID, uuid.NewString()); err == nil {
		t.Fatal("accepted another store")
	}
	exec(`UPDATE products SET external_id=NULL WHERE id=$1`, fx.productID)
	if _, err := svc.LoadERPOrderCheckout(ctx, fx.cartID, fx.storeID); err == nil {
		t.Fatal("allocated unlinked local product to ERP payment")
	}
}
