package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/zap"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
)

func TestTinyReflectionAcknowledgesOnlyVerifiedPendingItems(t *testing.T) {
	requireDB(t)
	for _, scenario := range []string{"legacy match", "versioned match", "missing item", "older quantity", "different price", "duplicate product", "newer local quantity", "invoiced order", "read failure"} {
		t.Run(scenario, func(t *testing.T) {
			fx := seedPaidCart(t, 2, 0)
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
			exec(`UPDATE carts SET external_order_id='1',erp_order_state='open',erp_order_status='aberto' WHERE id=$1`, fx.cartID)
			exec(`UPDATE cart_items SET erp_pending_since=now()-interval '10 days',erp_confirmed_quantity=NULL WHERE cart_id=$1`, fx.cartID)
			if scenario == "versioned match" {
				exec(`UPDATE cart_items SET erp_confirmed_quantity=0 WHERE cart_id=$1`, fx.cartID)
			}
			seqBefore := seqDoProduto(t, fx.productID)
			provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "local-fixture"}, Logger: zap.NewNop(), StoreID: fx.storeID, IntegrationID: "local-reflection-confirmation"})
			if err != nil {
				t.Fatal(err)
			}
			reads, writes := 0, 0
			provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/pedidos/1") {
					writes++
					return nil, fmt.Errorf("unexpected ERP request during reflection")
				}
				reads++
				status, quantity, price, invoice := 200, 2, 10.0, 0
				switch scenario {
				case "older quantity":
					quantity = 1
				case "different price":
					price = 9.5
				case "newer local quantity":
					exec(`UPDATE cart_items SET quantity=3 WHERE cart_id=$1`, fx.cartID)
				case "invoiced order":
					invoice = 99
				case "read failure":
					status = 400
				}
				item := fmt.Sprintf(`{"produto":{"id":123},"quantidade":%d,"valorUnitario":%.2f}`, quantity, price)
				items := item
				if scenario == "missing item" {
					items = ""
				} else if scenario == "duplicate product" {
					items += "," + item
				}
				body := fmt.Sprintf(`{"id":1,"idNotaFiscal":%d,"itens":[%s]}`, invoice, items)
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			repo := tinyCheckoutProductionRepository(t)
			svc := &Service{repo: repo, logger: zap.NewNop()}
			flow := erp.NewService(erpRepoAdapter{repo}, &invoicedTinyCollaborator{Service: svc, provider: provider}, zap.NewNop())
			flow.SetCartSyncCollaborators(svc)
			_, err = flow.SyncCartFromERPOrder(t.Context(), fx.cartID, fx.storeID)
			wantErr := scenario == "read failure"
			if (err != nil) != wantErr {
				t.Fatalf("unexpected reflection error: %v", err)
			}
			var pending bool
			var confirmed, quantity, price int
			if err := testPool.QueryRow(t.Context(), `SELECT erp_pending_since IS NOT NULL,COALESCE(erp_confirmed_quantity,-1),quantity,unit_price FROM cart_items WHERE cart_id=$1`, fx.cartID).Scan(&pending, &confirmed, &quantity, &price); err != nil {
				t.Fatal(err)
			}
			match := scenario == "legacy match" || scenario == "versioned match" || scenario == "invoiced order"
			if pending == match || (match && confirmed != 2) {
				t.Fatalf("pending=%v confirmed=%d verified=%v", pending, confirmed, match)
			}
			wantQuantity := 2
			if scenario == "newer local quantity" {
				wantQuantity = 3
			}
			if quantity != wantQuantity || price != 1000 || writes != 0 || reads != 1 {
				t.Fatalf("reflection changed pending purchase or ERP: qty=%d price=%d reads=%d writes=%d", quantity, price, reads, writes)
			}
			if match {
				seq := seqDoProduto(t, fx.productID)
				if seq <= seqBefore {
					t.Fatal("acknowledgement did not invalidate in-flight stock snapshots")
				}
				if _, err := flow.SyncCartFromERPOrder(t.Context(), fx.cartID, fx.storeID); err != nil {
					t.Fatal(err)
				}
				if seqDoProduto(t, fx.productID) != seq || writes != 0 {
					t.Fatal("repeated confirmation was not idempotent")
				}
			}
		})
	}
}
