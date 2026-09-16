package integration

import (
	"context"
	"encoding/json"
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

type invoicedTinyCollaborator struct {
	*Service
	provider providers.ERPProvider
}

func (c *invoicedTinyCollaborator) ResolveProvider(context.Context, *erp.Integration) (providers.ERPProvider, error) {
	return c.provider, nil
}

type invoicedTinyReadTransport func(*http.Request) (*http.Response, error)

func (f invoicedTinyReadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestTinyInvoicedManualScheduleClearsLocalFailureWithoutERPWrite(t *testing.T) {
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_id='123' WHERE id=$1`, fx.productID)
	exec(`UPDATE carts SET external_order_id='1',erp_order_state='open',customer_name='Comprador Teste' WHERE id=$1`, fx.cartID)
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at)
 VALUES($1,1000,1000,'credit_card','manual-card','2026-09-12T15:00:00Z')`, fx.cartID)
	exec(`UPDATE order_payments p SET erp_finalisation_status='failed',erp_last_error='conciliação pendente',
 gateway_snapshot='{"payment_id":"manual-card","installments":5}' FROM orders o WHERE p.order_id=o.id AND o.cart_id=$1`, fx.cartID)
	provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: "local-fixture"}, Logger: zap.NewNop(), StoreID: fx.storeID, IntegrationID: "local-invoiced-reconciliation"})
	if err != nil {
		t.Fatal(err)
	}
	reads, writes := 0, 0
	provider.HTTPClient.Transport = invoicedTinyReadTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			writes++
			return nil, fmt.Errorf("invoiced reconciliation attempted ERP write")
		}
		reads++
		var body string
		switch {
		case strings.HasSuffix(r.URL.Path, "/pedidos/1"):
			body = fmt.Sprintf(`{"id":1,"numeroPedido":101,"situacao":1,"idNotaFiscal":99,"numeroOrdemCompra":"lc-cart-%s",
 "valorTotalPedido":10,"valorFrete":0,"valorDesconto":0,"cliente":{"id":8,"nome":"Comprador Teste"},
 "itens":[{"produto":{"id":123},"quantidade":1,"valorUnitario":10}],
 "pagamento":{"parcelas":[
 {"valor":1.99,"data":"2026-10-13","formaRecebimento":{"id":7,"nome":"Cartão de crédito"}},
 {"valor":2.01,"data":"2026-11-13","formaRecebimento":{"id":7,"nome":"Cartão de crédito"}},
 {"valor":2,"data":"2026-12-14","formaRecebimento":{"id":7,"nome":"Cartão de crédito"}},
 {"valor":2,"data":"2027-01-14","formaRecebimento":{"id":7,"nome":"Cartão de crédito"}},
 {"valor":2,"data":"2027-02-14","formaRecebimento":{"id":7,"nome":"Cartão de crédito"}}]}}`, fx.cartID)
		case strings.HasSuffix(r.URL.Path, "/contas-receber") && r.URL.Query().Get("idVenda") == "1":
			body = `{"itens":[{"id":50,"situacao":"aberto","valor":10,"saldo":10,"dataVencimento":"2026-09-28"}]}`
		default:
			return nil, fmt.Errorf("unexpected Tiny request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	repo := tinyCheckoutProductionRepository(t)
	svc := &Service{repo: repo, logger: zap.NewNop()}
	flow := erp.NewService(erpRepoAdapter{repo}, &invoicedTinyCollaborator{Service: svc, provider: provider}, zap.NewNop())
	if err := flow.ConfirmERPOrderPayment(t.Context(), fx.cartID, fx.storeID, nil); err != nil {
		t.Fatal(err)
	}
	state, externalID, status, _ := cartERPState(t, fx.cartID)
	finalized, lastError, _, _, _ := cartFinalisationState(t, fx.cartID)
	if state != "confirmed" || externalID != "1" || status != "faturado" || finalized != "done" || lastError != "" {
		t.Fatalf("failure/status not reconciled: state=%s status=%s finalization=%s error=%s", state, status, finalized, lastError)
	}
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT progress FROM tiny_checkout_operations WHERE cart_id=$1 AND completed`, fx.cartID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var op providers.TinyCheckoutOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatal(err)
	}
	if !op.PreservedFinancialSchedule || op.Replace || len(op.Order.Checkout.Payments) != 5 || op.Order.Checkout.Payments[0].AmountCents != 200 {
		t.Fatal("journal lost reconciliation decision or original payment schedule")
	}
	priorReads := reads
	if err := flow.ConfirmERPOrderPayment(t.Context(), fx.cartID, fx.storeID, nil); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || reads != priorReads || reads < 4 {
		t.Fatalf("unexpected ERP activity: writes=%d reads=%d replay_reads=%d", writes, reads, reads-priorReads)
	}
}
