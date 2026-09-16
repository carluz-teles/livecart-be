//go:build tiny_checkout_e2e

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
)

type tinyDemoCollaborator struct {
	*Service
	provider providers.ERPProvider
}

func (c *tinyDemoCollaborator) ResolveProvider(context.Context, *erp.Integration) (providers.ERPProvider, error) {
	return c.provider, nil
}

// TestE2ETinyFinalizationDatabase exercises the actual finalisation service,
// isolated Postgres journal and Tiny API. Only newly created, marked demo orders
// can be changed by the transport. No gateway charge or fiscal endpoint exists.
func TestE2ETinyFinalizationDatabase(t *testing.T) {
	token, path := os.Getenv("TINY_E2E_TOKEN"), os.Getenv("TINY_E2E_CHECKOUT_FIXTURE")
	if token == "" || path == "" {
		t.Skip("requires explicit Tiny demo token and fixture")
	}
	requireDB(t)
	var fixture struct {
		Run     string `json:"run"`
		Records map[string]struct {
			ID int64 `json:"id"`
		} `json:"records"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fixture.Run, "LC-TINY-TEST-") {
		t.Fatal("unexpected fixture")
	}
	pid, cid := fixture.Records["product"].ID, fixture.Records["checkout_contact"].ID
	if pid <= 0 || cid <= 0 {
		t.Fatal("missing demo product/contact")
	}
	fx := seedPaidCart(t, 1, 0)
	guard := &tinyDemoCheckoutTransport{base: http.DefaultTransport, cartID: fx.cartID, productID: pid, contactID: cid, orders: map[string]bool{}}
	guard.trace = func(method, path string, status int) { t.Logf("Tiny demo %s %s -> %d", method, path, status) }
	provider, err := providererp.NewTiny(providererp.TinyConfig{Credentials: &providers.Credentials{AccessToken: token}, Logger: zap.NewNop(), StoreID: fx.storeID, IntegrationID: "isolated-tiny-finalization"})
	if err != nil {
		t.Fatal(err)
	}
	provider.HTTPClient.Transport = guard
	provider.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	info, err := provider.TestConnection(ctx)
	if err != nil || !info.Success || info.AccountInfo["empresa"] != "ADABYTE LTDA" {
		t.Fatalf("unexpected demo account: err=%v", err)
	}
	guard.verified = true
	withCustomerDelivery := os.Getenv("TINY_E2E_CUSTOMER_DELIVERY") == "1"
	if withCustomerDelivery && os.Getenv("TINY_E2E_EXISTING_CONTACT") != "1" {
		t.Fatal("customer delivery scenario requires the separate demo checkout contact")
	}
	var checkoutDocument string
	sourceContactID := cid
	if os.Getenv("TINY_E2E_EXISTING_CONTACT") == "1" {
		var checkoutContactName string
		var originalAddress map[string]any
		sourceContactID = fixture.Records["reservation_contact"].ID
		if sourceContactID <= 0 || sourceContactID == cid {
			t.Fatal("missing separate demo reservation contact")
		}
		for _, id := range []int64{sourceContactID, cid} {
			var contact struct {
				Name     string         `json:"nome"`
				Document string         `json:"cpfCnpj"`
				Address  map[string]any `json:"endereco"`
			}
			if err := tinyDemoAPI(ctx, provider, http.MethodGet, "/contatos/"+strconv.FormatInt(id, 10), nil, &contact); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(contact.Name, fixture.Run) || contact.Document != "" {
				t.Fatal("expected owned demo contacts without documents")
			}
			if id == cid {
				checkoutContactName = contact.Name
				originalAddress = contact.Address
				if originalAddress == nil {
					originalAddress = map[string]any{"endereco": "", "numero": "", "complemento": "", "bairro": "", "municipio": "", "uf": "", "cep": "", "pais": ""}
				}
			}
		}
		checkoutDocument = os.Getenv("TINY_E2E_CONTACT_DOCUMENT")
		if len(checkoutDocument) != 11 {
			t.Fatal("requires an explicit synthetic demo CPF")
		}
		matches, err := provider.SearchContacts(ctx, providers.SearchContactsParams{CpfCnpj: checkoutDocument})
		if err != nil || len(matches) != 0 {
			t.Fatalf("demo document already in use or lookup failed: %v", err)
		}
		if err := provider.UpdateContact(ctx, strconv.FormatInt(cid, 10), providers.ERPContactInput{Name: checkoutContactName, CpfCnpj: checkoutDocument}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			payload := map[string]any{"nome": checkoutContactName, "cpfCnpj": ""}
			if withCustomerDelivery {
				payload["endereco"] = originalAddress
			}
			if err := tinyDemoAPI(cleanup, provider, http.MethodPut, "/contatos/"+strconv.FormatInt(cid, 10), payload, nil); err != nil {
				t.Error(err)
			}
		})
		if withCustomerDelivery {
			address := map[string]any{"endereco": "Praca da Se", "numero": "42", "complemento": "", "bairro": "Se", "municipio": "Sao Paulo", "uf": "SP", "cep": "01001000", "pais": "Brasil"}
			if err := tinyDemoAPI(ctx, provider, http.MethodPut, "/contatos/"+strconv.FormatInt(cid, 10), map[string]any{"nome": checkoutContactName, "endereco": address}, nil); err != nil {
				t.Fatal(err)
			}
		}
		guard.reservationContactID = sourceContactID
	}
	withStock := os.Getenv("TINY_E2E_LAUNCHED_STOCK") == "1"
	old, err := provider.CreateOrder(ctx, providers.ERPOrder{ExternalID: fx.cartID, ContactID: strconv.FormatInt(sourceContactID, 10), Items: []providers.ERPOrderItem{{ProductID: strconv.FormatInt(pid, 10), Quantity: 1, UnitPrice: 4990}}, Observation: "LC-TINY-TEST FINALIZATION - NAO FATURAR"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("demo source order=%s number=%s cart=%s", old.OrderID, old.OrderNumber, fx.cartID)
	// Cleanup is restricted to orders the transport itself observed being created.
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 3*time.Minute)
		defer done()
		for id := range guard.orders {
			var order struct {
				Invoice     int64  `json:"idNotaFiscal"`
				Status      int    `json:"situacao"`
				Anchor      string `json:"numeroOrdemCompra"`
				Observation string `json:"observacoes"`
			}
			if err := tinyDemoAPI(cleanup, provider, http.MethodGet, "/pedidos/"+id, nil, &order); err != nil {
				t.Error(err)
				continue
			}
			if order.Invoice != 0 || (order.Anchor != "lc-cart-"+fx.cartID && !strings.Contains(order.Observation, "Carrinho "+fx.cartID)) {
				t.Error("cleanup ownership/invoice guard")
				continue
			}
			var accounts struct {
				Items []struct {
					Status  string  `json:"situacao"`
					Value   float64 `json:"valor"`
					Balance float64 `json:"saldo"`
				} `json:"itens"`
			}
			if err := tinyDemoAPI(cleanup, provider, http.MethodGet, "/contas-receber?idVenda="+id, nil, &accounts); err != nil {
				t.Error(err)
				continue
			}
			protected := false
			for _, a := range accounts.Items {
				if a.Status != "aberto" || a.Value != a.Balance {
					protected = true
				}
			}
			if protected {
				t.Error("cleanup received-account guard")
				continue
			}
			if len(accounts.Items) > 0 {
				if err := tinyDemoAPI(cleanup, provider, http.MethodPost, "/pedidos/"+id+"/estornar-contas", nil, nil); err != nil {
					t.Error(err)
					continue
				}
			}
			// Only undo launches observed by this test transport. Never send a
			// speculative reversal to a reservation or an unrelated demo order.
			if guard.stockLaunched[id] {
				if err := provider.ReverseOrderStock(cleanup, id); err != nil {
					t.Error(err)
					continue
				}
			}
			if order.Status != 2 {
				if err := provider.SetOrderSituacao(cleanup, id, 2); err != nil {
					t.Error(err)
				}
			}
		}
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE products SET external_id=$1,price=4990 WHERE id=$2`, strconv.FormatInt(pid, 10), fx.productID)
	exec(`UPDATE cart_items SET unit_price=4990 WHERE cart_id=$1`, fx.cartID)
	exec(`UPDATE carts SET external_order_id=$1,erp_order_state='open',shipping_cost_cents=1859,
 shipping_cost_real_cents=2700,shipping_carrier='Correios',shipping_service_name='PAC',
 customer_name=$2,customer_email='livecart-tiny-test@example.invalid',customer_phone='11900000000',
 shipping_address='{"street":"Praca da Se","number":"42","neighborhood":"Se","city":"Sao Paulo","state":"SP","zipCode":"01001000"}' WHERE id=$3`, old.OrderID, fixture.Run+" Comprador completo", fx.cartID)
	if checkoutDocument != "" {
		exec(`UPDATE carts SET customer_document=$1 WHERE id=$2`, checkoutDocument, fx.cartID)
	}
	cardPaid, cardGross := 6599, 6849
	if os.Getenv("TINY_E2E_MIXED_PAYMENTS") == "1" {
		cardPaid, cardGross = 4740, 4990
		exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1::uuid,1859,1859,'pix','tiny-demo-freight-'||$1::text,'2026-09-14T16:00:00Z')`, fx.cartID)
	}
	exec(`INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at)
 VALUES($1::uuid,$2,$3,'credit_card','tiny-demo-card-'||$1::text,'2026-09-14T15:00:00Z')`, fx.cartID, cardPaid, cardGross)
	exec(`UPDATE order_payments p SET gateway_snapshot=jsonb_build_object('payment_id','tiny-demo-card-'||$1::text,'installments',2)
 FROM orders o WHERE o.id=p.order_id AND o.cart_id=$1::uuid`, fx.cartID)
	if err := provider.SetOrderInstallments(ctx, old.OrderID, []providers.ERPInstallment{{AmountCents: 4990, DueDate: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), Note: "RESERVA TESTE"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetOrderSituacao(ctx, old.OrderID, 3); err != nil {
		t.Fatal(err)
	}
	if err := tinyDemoAPI(ctx, provider, http.MethodPost, "/pedidos/"+old.OrderID+"/lancar-contas", nil, nil); err != nil {
		t.Fatal(err)
	}
	if withStock {
		if err := tinyDemoAPI(ctx, provider, http.MethodPost, "/pedidos/"+old.OrderID+"/lancar-estoque", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	productionRepo := tinyCheckoutProductionRepository(t)
	svc := &Service{repo: productionRepo, logger: zap.NewNop()}
	flow := erp.NewService(erpRepoAdapter{productionRepo}, &tinyDemoCollaborator{Service: svc, provider: provider}, zap.NewNop())
	if err := flow.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, nil); err != nil {
		t.Fatal(err)
	}
	state, target, _, _ := cartERPState(t, fx.cartID)
	if state != erp.OrderStateConfirmed || target == old.OrderID || target == "" {
		t.Fatalf("wrong finalization %s %s", state, target)
	}
	var progress []byte
	if err := testPool.QueryRow(ctx, `SELECT progress FROM tiny_checkout_operations WHERE cart_id=$1 AND completed`, fx.cartID).Scan(&progress); err != nil {
		t.Fatal(err)
	}
	if withStock {
		var op providers.TinyCheckoutOperation
		if err := json.Unmarshal(progress, &op); err != nil {
			t.Fatal(err)
		}
		if !op.StockReversed || !op.StockLaunched || guard.stockLaunched[old.OrderID] || !guard.stockLaunched[target] {
			t.Fatal("source launch was not restored on the final order")
		}
		var launched bool
		if err := testPool.QueryRow(ctx, `SELECT erp_stock_launched FROM carts WHERE id=$1`, fx.cartID).Scan(&launched); err != nil || !launched {
			t.Fatalf("local launch not recorded: %v", err)
		}
	}
	if checkoutDocument != "" {
		var op providers.TinyCheckoutOperation
		if err := json.Unmarshal(progress, &op); err != nil {
			t.Fatal(err)
		}
		if op.Order.ContactID != strconv.FormatInt(cid, 10) {
			t.Fatal("existing document contact was not bound")
		}
		var sourceContact struct {
			Document string `json:"cpfCnpj"`
		}
		if err := tinyDemoAPI(ctx, provider, http.MethodGet, "/contatos/"+strconv.FormatInt(sourceContactID, 10), nil, &sourceContact); err != nil {
			t.Fatal(err)
		}
		if sourceContact.Document != "" {
			t.Fatal("reservation contact was overwritten")
		}
		t.Logf("existing document contact=%d reused; reservation contact=%d preserved", cid, sourceContactID)
	}
	if withCustomerDelivery {
		var actual struct {
			Address *json.RawMessage `json:"enderecoEntrega"`
		}
		if err := tinyDemoAPI(ctx, provider, http.MethodGet, "/pedidos/"+target, nil, &actual); err != nil {
			t.Fatal(err)
		}
		if actual.Address != nil {
			t.Fatal("Tiny did not return the expected customer-address representation")
		}
		t.Log("Tiny returned null separate delivery address; matching customer address verified")
	}
	if report := os.Getenv("TINY_E2E_REPORT_FILE"); report != "" {
		if err := os.WriteFile(report, progress, 0600); err != nil {
			t.Fatal(err)
		}
	}
	created := guard.created
	if err := flow.OnCartPaidTinyCheckout(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatal(err)
	}
	if guard.created != created {
		t.Fatal("redelivery created another order")
	}
	if situation, err := provider.GetOrderSituacao(ctx, target); err != nil || situation != 3 {
		t.Fatalf("final approval=%d err=%v", situation, err)
	}
	t.Logf("demo final order=%s confirmed; original=%s cancelled; replay created=0; accounts rebuilt", target, old.OrderID)
	if withStock {
		t.Log("launched source stock reversed and restored on final order")
	}
}

func tinyDemoAPI(ctx context.Context, p *providererp.Tiny, method, path string, payload, target any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, "https://api.tiny.com.br/public-api/v3"+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+os.Getenv("TINY_E2E_TOKEN"))
	r.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1000))
		return fmt.Errorf("demo %s %s status %d: %s", method, path, resp.StatusCode, raw)
	}
	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}

type tinyDemoCheckoutTransport struct {
	reservationContactID int64
	stockLaunched        map[string]bool
	base                 http.RoundTripper
	mu                   sync.Mutex
	next                 time.Time
	verified             bool
	cartID               string
	productID, contactID int64
	orders               map[string]bool
	created              int
	trace                func(string, string, int)
}

func (g *tinyDemoCheckoutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Scheme != "https" || r.URL.Host != "api.tiny.com.br" {
		return nil, fmt.Errorf("demo host rejected")
	}
	path := strings.TrimPrefix(r.URL.Path, "/public-api/v3")
	if r.Method != http.MethodGet {
		if !g.verified {
			return nil, fmt.Errorf("demo account not verified")
		}
		allowed := path == "/contatos/"+strconv.FormatInt(g.contactID, 10) && r.Method == http.MethodPut
		if path == "/pedidos" && r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var p struct {
				Contact     int64  `json:"idContato"`
				Anchor      string `json:"numeroOrdemCompra"`
				Observation string `json:"observacoes"`
				Items       []struct {
					Product struct {
						ID int64 `json:"id"`
					} `json:"produto"`
					Quantity int `json:"quantidade"`
				} `json:"itens"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			owned := p.Anchor == "lc-cart-"+g.cartID || (strings.HasPrefix(p.Anchor, "lc-cart-paid-") && strings.Contains(p.Observation, "Carrinho "+g.cartID))
			allowedContact := p.Contact == g.contactID || (g.reservationContactID > 0 && p.Contact == g.reservationContactID)
			allowed = allowedContact && owned && len(p.Anchor) <= 50 && len(p.Items) == 1 && p.Items[0].Product.ID == g.productID && p.Items[0].Quantity == 1
		} else {
			for id := range g.orders {
				if path == "/pedidos/"+id || path == "/pedidos/"+id+"/itens" || path == "/pedidos/"+id+"/situacao" || path == "/pedidos/"+id+"/estornar-contas" || path == "/pedidos/"+id+"/lancar-contas" || path == "/pedidos/"+id+"/lancar-estoque" || path == "/pedidos/"+id+"/estornar-estoque" {
					allowed = true
				}
			}
		}
		if !allowed {
			return nil, fmt.Errorf("demo mutation rejected: %s %s", r.Method, path)
		}
	}
	if wait := time.Until(g.next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-timer.C:
		}
	}
	g.next = time.Now().Add(3100 * time.Millisecond)
	resp, err := g.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if g.trace != nil {
		g.trace(r.Method, path, resp.StatusCode)
	}
	if r.Method == http.MethodPost && resp.StatusCode == http.StatusNoContent {
		for id := range g.orders {
			if g.stockLaunched == nil {
				g.stockLaunched = map[string]bool{}
			}
			if path == "/pedidos/"+id+"/lancar-estoque" {
				g.stockLaunched[id] = true
			}
			if path == "/pedidos/"+id+"/estornar-estoque" {
				g.stockLaunched[id] = false
			}
		}
	}
	if path == "/pedidos" && r.Method == http.MethodPost && resp.StatusCode == 201 {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		var created struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &created); err != nil {
			return nil, err
		}
		g.orders[strconv.FormatInt(created.ID, 10)] = true
		g.created++
	}
	return resp, nil
}
