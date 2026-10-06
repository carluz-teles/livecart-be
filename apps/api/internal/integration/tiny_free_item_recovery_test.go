package integration

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
)

func TestTinyFreeGiftBlocksWriteButManualReconciliationConfirmsPendingPurchase(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	fx := seedPaidCart(t, 2, 0)
	for _, statement := range []struct {
		sql string
		id  string
	}{
		{`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID},
		{`UPDATE carts SET payment_status='pending',paid_at=NULL,external_order_id='1',erp_order_state='open',erp_order_status='aberto' WHERE id=$1`, fx.cartID},
		{`UPDATE cart_items SET erp_pending_since=now(),erp_confirmed_quantity=1 WHERE cart_id=$1`, fx.cartID},
	} {
		if _, err := testPool.Exec(ctx, statement.sql, statement.id); err != nil {
			t.Fatal(err)
		}
	}
	provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "fixture"}, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	quantity, writes := 1, 0
	provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/pedidos/1") {
			writes++
			return nil, fmt.Errorf("unexpected ERP write")
		}
		body := fmt.Sprintf(`{"id":1,"situacao":0,"itens":[{"produto":{"id":123},"quantidade":%d,"valorUnitario":10},{"produto":{"id":456},"quantidade":1,"valorUnitario":0,"infoAdicional":"Brinde"}]}`, quantity)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	svc := newFinalisationService(provider)
	reserve := func() error {
		return svc.ReserveStockInERP(ctx, fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, "buyer")
	}
	if err := reserve(); !errors.Is(err, providers.ErrOrderItemPriceInvalid) {
		t.Fatalf("missing typed reconciliation error: %v", err)
	}
	var pending bool
	var confirmed int
	read := func() {
		t.Helper()
		if err := testPool.QueryRow(ctx, `SELECT erp_pending_since IS NOT NULL,erp_confirmed_quantity FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&pending, &confirmed); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !pending || confirmed != 1 || writes != 0 {
		t.Fatalf("unconfirmed purchase was acknowledged or gift overwritten: pending=%v confirmed=%d writes=%d", pending, confirmed, writes)
	}
	quantity = 2 // The merchant includes the pending purchase in Tiny, preserving the free gift.
	if err := reserve(); err != nil {
		t.Fatalf("manual reconciliation failed: %v", err)
	}
	read()
	if pending || confirmed != 2 || writes != 0 {
		t.Fatalf("read-back did not confirm exact purchase: pending=%v confirmed=%d writes=%d", pending, confirmed, writes)
	}
}
