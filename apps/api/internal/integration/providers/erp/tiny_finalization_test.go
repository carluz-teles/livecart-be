package erp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

type checkoutTestJournal struct {
	failContactSave      bool
	failStockReverseSave bool
	failStockLaunchSave  bool
	raw                  []byte
	bindFailures         int
	bound                string
}

func (j *checkoutTestJournal) Save(ctx context.Context, op *providers.TinyCheckoutOperation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.failContactSave && op.Order.ContactID == "9" {
		j.failContactSave = false
		return errors.New("database unavailable before contact checkpoint")
	}
	if j.failStockReverseSave && op.StockReversed {
		j.failStockReverseSave = false
		return errors.New("database unavailable after stock reversal")
	}
	if j.failStockLaunchSave && op.StockLaunched {
		j.failStockLaunchSave = false
		return errors.New("database unavailable after stock launch")
	}
	var err error
	j.raw, err = json.Marshal(op)
	return err
}
func (j *checkoutTestJournal) Bind(ctx context.Context, op *providers.TinyCheckoutOperation) error {
	if j.bindFailures > 0 {
		j.bindFailures--
		return errors.New("database unavailable before bind")
	}
	j.bound = op.TargetID
	op.Completed = true
	return j.Save(ctx, op)
}
func (j *checkoutTestJournal) resume(t *testing.T) *providers.TinyCheckoutOperation {
	t.Helper()
	var op providers.TinyCheckoutOperation
	if err := json.Unmarshal(j.raw, &op); err != nil {
		t.Fatal(err)
	}
	return &op
}

type checkoutTestTiny struct {
	contacts                                                                                map[string]map[string]any
	contactUpdates                                                                          []string
	contactSearchStatus                                                                     int
	contactSearchItems                                                                      []map[string]any
	contactSearchTotal                                                                      int
	merchantChangeAfterCreate                                                               bool
	noReplacement, lostStockReverse, lostStockLaunch, rejectStockReverse, rejectStockLaunch bool
	shippingForms                                                                           []tinyCheckoutReference
	writes                                                                                  int
	rejectCreate                                                                            bool
	mu                                                                                      sync.Mutex
	orders                                                                                  map[string]*tinyCheckoutOrder
	accounts                                                                                map[string][]tinyReceivable
	posts, cancels, reversals                                                               int
	lostCreate, lostCancel, stockLocked, invoiceAfterCreate, received, readAccountsFailure  bool
	stockReversals, stockLaunches                                                           int
	targetStockLocked                                                                       bool
}

func (f *checkoutTestTiny) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.writes++
	}
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Error(err)
		}
	}
	if r.URL.Path == "/contatos" && r.Method == http.MethodGet {
		if f.contactSearchStatus != 0 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(f.contactSearchStatus)
			return
		}
		items := f.contactSearchItems
		if items == nil {
			items = []map[string]any{}
			for _, contact := range f.contacts {
				if contact["cpfCnpj"] == r.URL.Query().Get("cpfCnpj") {
					items = append(items, contact)
				}
			}
		}
		write(map[string]any{"itens": items, "paginacao": map[string]any{"total": max(len(items), f.contactSearchTotal)}})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/contatos/") && r.Method == http.MethodGet {
		contact := f.contacts[strings.TrimPrefix(r.URL.Path, "/contatos/")]
		if contact == nil {
			w.WriteHeader(404)
			return
		}
		write(contact)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/contatos/") && r.Method == http.MethodPut {
		id := strings.TrimPrefix(r.URL.Path, "/contatos/")
		f.contactUpdates = append(f.contactUpdates, id)
		if f.contacts != nil {
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			for otherID, contact := range f.contacts {
				if otherID != id && payload["cpfCnpj"] != nil && contact["cpfCnpj"] == payload["cpfCnpj"] {
					w.WriteHeader(400)
					write(map[string]any{"mensagem": "Ocorreram erros de validação", "detalhes": []any{map[string]any{"campo": "cnpj", "mensagem": "Contato com CNPJ já existe"}}})
					return
				}
			}
			for key, value := range payload {
				f.contacts[id][key] = value
			}
		}
		w.WriteHeader(204)
		return
	}
	if r.URL.Path == "/formas-recebimento" {
		write(map[string]any{"itens": []any{map[string]any{"id": 7, "nome": "Pix", "situacao": "1"}}})
		return
	}
	if r.URL.Path == "/formas-envio" {
		write(map[string]any{"itens": f.shippingForms})
		return
	}
	if r.URL.Path == "/contas-receber" {
		if f.readAccountsFailure {
			w.WriteHeader(503)
			return
		}
		accounts := f.accounts[r.URL.Query().Get("idVenda")]
		if accounts == nil {
			accounts = []tinyReceivable{}
		}
		write(map[string]any{"itens": accounts})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/recebimentos") {
		if f.received {
			write([]any{map[string]any{"id": 55, "valorPago": 1}})
		} else {
			write([]any{})
		}
		return
	}
	if r.URL.Path == "/pedidos" && r.Method == http.MethodGet {
		items := []any{}
		for _, o := range f.orders {
			items = append(items, map[string]any{"id": o.ID, "numeroOrdemCompra": o.Anchor})
		}
		write(map[string]any{"itens": items})
		return
	}
	if r.URL.Path == "/pedidos" && r.Method == http.MethodPost {
		if f.rejectCreate {
			f.rejectCreate = false
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		f.posts++
		var data map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		order := &tinyCheckoutOrder{ID: 2, Number: "102", Status: 0}
		body, _ := json.Marshal(data)
		if err := json.Unmarshal(body, order); err != nil {
			t.Fatal(err)
		}
		order.Customer = f.orders["1"].Customer
		if f.contacts != nil {
			var id int64
			if err := json.Unmarshal(data["idContato"], &id); err != nil {
				t.Error(err)
			}
			contact, _ := json.Marshal(f.contacts[strconv.FormatInt(id, 10)])
			if err := json.Unmarshal(contact, &order.Customer); err != nil {
				t.Error(err)
			}
		}
		order.Total = 49.90 + order.Freight - order.Discount
		for i := range order.Payment.Installments {
			order.Payment.Installments[i].Method.Name = "Pix"
		}
		f.orders["2"] = order
		if f.invoiceAfterCreate {
			f.orders["1"].InvoiceID = 333
		}
		if f.merchantChangeAfterCreate {
			f.orders["1"].Items[0].Quantity++
		}
		if f.lostCreate {
			f.lostCreate = false
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(201)
		write(map[string]any{"id": 2, "numeroPedido": "102"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "pedidos" {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
		return
	}
	id := parts[1]
	order := f.orders[id]
	if order == nil {
		w.WriteHeader(404)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodGet {
		write(order)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPut {
		var update tinyCheckoutOrder
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Fatal(err)
		}
		order.Payment = update.Payment
		w.WriteHeader(204)
		return
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "itens":
			if (id == "1" && f.stockLocked) || (id == "2" && f.targetStockLocked) || order.InvoiceID != 0 {
				w.WriteHeader(400)
				write(map[string]any{"detalhes": []any{map[string]any{"campo": "pedido.motivosBloqueio[0]", "mensagem": "estoque lançado"}}})
				return
			}
			if len(f.accounts[id]) > 0 {
				w.WriteHeader(400)
				write(map[string]any{"detalhes": []any{map[string]any{"campo": "pedido.motivosBloqueio[0]", "mensagem": "contas lançadas"}}})
				return
			}
		case "situacao":
			var in struct {
				Status int `json:"situacao"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Fatal(err)
			}
			if in.Status == 2 {
				f.cancels++
				if f.orders["2"] == nil {
					t.Error("old reservation cancelled before replacement existed")
				}
			}
			order.Status = in.Status
			if f.lostCancel {
				f.lostCancel = false
				w.WriteHeader(502)
				return
			}
		case "estornar-contas":
			f.reversals++
			f.accounts[id] = nil
		case "estornar-estoque":
			if id != "1" || !f.stockLocked {
				t.Error("stock reversed without a launched source")
			}
			if f.orders["2"] == nil && !f.noReplacement {
				t.Error("stock released before verified replacement exists")
			}
			if f.rejectStockReverse {
				f.rejectStockReverse = false
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(429)
				return
			}
			f.stockReversals++
			f.stockLocked = false
			if f.lostStockReverse {
				f.lostStockReverse = false
				w.WriteHeader(502)
				return
			}
		case "lancar-estoque":
			if !f.noReplacement && (id != "2" || f.orders["1"].Status != 2) {
				t.Error("stock launched before replacement completed")
			}
			if f.rejectStockLaunch {
				f.rejectStockLaunch = false
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(429)
				return
			}
			if f.targetStockLocked {
				w.WriteHeader(400)
				write(map[string]any{"mensagem": "Estoque já lançado."})
				return
			}
			f.stockLaunches++
			f.targetStockLocked = true
			if f.lostStockLaunch {
				f.lostStockLaunch = false
				w.WriteHeader(502)
				return
			}
		case "lancar-contas":
			for i, p := range order.Payment.Installments {
				f.accounts[id] = append(f.accounts[id], tinyReceivable{ID: int64(100 + i), Status: "aberto", DueDate: p.Date, Value: p.Value, Balance: p.Value})
			}
		default:
			t.Errorf("unexpected mutation %s", r.URL.Path)
		}
		w.WriteHeader(204)
		return
	}
	w.WriteHeader(404)
}

type checkoutQuotaFailure struct {
	ratelimit.RateLimiter
	fail   bool
	cancel context.CancelFunc
}

func (q *checkoutQuotaFailure) WaitRequest(_ context.Context, stringMethod string) error {
	if q.fail && stringMethod == http.MethodPost {
		q.fail = false
		if q.cancel != nil {
			q.cancel()
		}
		return context.DeadlineExceeded
	}
	return nil
}
func (q *checkoutQuotaFailure) UpdateFromHeaders(int, int) {}

func TestTinyFinalizationRetriesKnownRejectionsWithoutAmbiguousCreate(t *testing.T) {
	for _, scenario := range []string{"quota before dispatch", "cancelled during quota", "HTTP 429"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			journal := &checkoutTestJournal{}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if scenario == "HTTP 429" {
				fake.rejectCreate = true
			} else {
				quota := &checkoutQuotaFailure{fail: true}
				if scenario == "cancelled during quota" {
					quota.cancel = cancel
				}
				provider.RateLimiter = quota
			}
			if _, err := provider.FinalizePaidCheckout(ctx, op, journal); err == nil {
				t.Fatal("expected rejection")
			}
			restored := journal.resume(t)
			if restored.CreateStarted || fake.posts != 0 {
				t.Fatal("rejected request left an ambiguous creation")
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), restored, journal); err != nil {
				t.Fatal(err)
			}
			if fake.posts != 1 || fake.cancels != 1 {
				t.Fatal("recovery duplicated order or cancellation")
			}
		})
	}
}

func checkoutFinalizationFixture(t *testing.T) (*Tiny, *checkoutTestTiny, *providers.TinyCheckoutOperation) {
	t.Helper()
	checkout := &providers.ERPOrderCheckout{Customer: providers.ERPContactInput{Name: "Comprador Teste"}, FreightCents: 1859, DiscountCents: 250,
		Payments: []providers.ERPInstallment{{AmountCents: 6599, DueDate: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), Method: "pix", Note: "Pago teste"}}}
	op := &providers.TinyCheckoutOperation{ID: "operation-1", CartID: "cart-1", SourceID: "1", StartedAt: time.Now(), Order: providers.ERPOrder{
		ExternalID: "cart-1-paid-operation-1", ContactID: "8", Checkout: checkout, TotalAmount: 6599, Items: []providers.ERPOrderItem{{ProductID: "20", Quantity: 1, UnitPrice: 4990}}}}
	source := &tinyCheckoutOrder{ID: 1, Number: "101", Anchor: "lc-cart-cart-1", Total: 49.90}
	source.Customer.Name = "Comprador Teste"
	body := []byte(`{"itens":[{"produto":{"id":20},"quantidade":1,"valorUnitario":49.9}]}`)
	if err := json.Unmarshal(body, source); err != nil {
		t.Fatal(err)
	}
	fake := &checkoutTestTiny{orders: map[string]*tinyCheckoutOrder{"1": source}, accounts: map[string][]tinyReceivable{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fake.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	return newTinyAgainst(t, srv), fake, op
}

func TestTinyFinalizationReusesCheckoutDocumentContact(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		prepared, lostCreate bool
	}{
		{name: "new finalization"},
		{name: "resume existing checkpoint", prepared: true},
		{name: "lost creation response", prepared: true, lostCreate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			fake.contacts = map[string]map[string]any{
				"8": {"id": 8, "nome": "instagram_reserva", "cpfCnpj": "", "situacao": "B"},
				"9": {"id": 9, "nome": "Comprador Teste", "cpfCnpj": "529.982.247-25", "situacao": "A"},
			}
			op.Order.Checkout.Customer.CpfCnpj = "52998224725"
			fake.orders["1"].Customer.ID = 8
			fake.orders["1"].Customer.Name = "instagram_reserva"
			fake.stockLocked = true
			op.Prepared, op.Replace, op.SourceStockLaunched = tt.prepared, tt.prepared, tt.prepared
			journal := &checkoutTestJournal{bindFailures: 1}
			wantError := "database unavailable before bind"
			if tt.lostCreate {
				fake.lostCreate, journal.bindFailures, wantError = true, 0, "502"
			}
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("expected %s, got %v", wantError, err)
			}
			restored := journal.resume(t)
			if restored.Order.ContactID != "9" || fake.orders["2"].Customer.ID != 9 {
				t.Fatal("resolved checkout contact was not persisted/used")
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), restored, journal); err != nil {
				t.Fatal(err)
			}
			if fake.posts != 1 || fake.cancels != 1 || fake.stockReversals != 1 || fake.stockLaunches != 1 {
				t.Fatal("resuming duplicated the order or stock movements")
			}
			if len(fake.contactUpdates) != 1 || fake.contactUpdates[0] != "9" || fake.contacts["8"]["cpfCnpj"] != "" {
				t.Fatal("reservation contact was overwritten or update repeated after creation")
			}
		})
	}
}

func TestTinyFinalizationResumesWithoutDuplicateOrReservationGap(t *testing.T) {
	for _, failure := range []string{"none", "lost create response", "lost cancel response", "database bind"} {
		t.Run(failure, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			journal := &checkoutTestJournal{}
			fake.lostCreate = failure == "lost create response"
			fake.lostCancel = failure == "lost cancel response"
			if failure == "database bind" {
				journal.bindFailures = 1
			}
			result, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if failure != "none" {
				if err == nil {
					t.Fatal("failure was not propagated")
				}
				result, err = provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.OrderID != "2" || journal.bound != "2" {
				t.Fatalf("wrong binding: %+v", result)
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.posts != 1 || fake.cancels != 1 || fake.orders["1"].Status != 2 {
				t.Fatalf("posts=%d cancels=%d", fake.posts, fake.cancels)
			}
		})
	}
}

func TestTinyFinalizationProtectsFiscalAndReceivedAccounts(t *testing.T) {
	for _, scenario := range []string{"invoice", "partially received", "receipt despite full balance", "invoice races creation", "financial read unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			journal := &checkoutTestJournal{}
			switch scenario {
			case "invoice":
				fake.orders["1"].InvoiceID = 99
			case "partially received":
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "parcial", Value: 49.9, Balance: 20}}
			case "receipt despite full balance":
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 49.9, Balance: 49.9}}
				fake.received = true
			case "invoice races creation":
				fake.invoiceAfterCreate = true
			case "financial read unavailable":
				fake.readAccountsFailure = true
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err == nil {
				t.Fatal("protected order accepted")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.cancels != 0 || fake.reversals != 0 || journal.bound != "" {
				t.Fatal("protected order mutated or bound")
			}
		})
	}
}

func TestTinyFinalizationRestoresLaunchedStockOnPaidReplacement(t *testing.T) {
	for _, scenario := range []string{"stock only", "stock and open accounts", "bind failure"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			fake.stockLocked = true
			journal := &checkoutTestJournal{}
			if scenario == "stock and open accounts" {
				fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", Value: 49.9, Balance: 49.9}}
			}
			if scenario == "bind failure" {
				journal.bindFailures = 1
			}
			result, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if scenario == "bind failure" {
				if err == nil {
					t.Fatal("bind failure was not propagated")
				}
				result, err = provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal)
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.OrderID != "2" || journal.bound != "2" {
				t.Fatalf("unexpected result: %+v", result)
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
				t.Fatal(err)
			}
			if fake.stockReversals != 1 || fake.stockLaunches != 1 || fake.posts != 1 || fake.cancels != 1 {
				t.Fatalf("reversals=%d launches=%d creates=%d cancels=%d", fake.stockReversals, fake.stockLaunches, fake.posts, fake.cancels)
			}
			if !fake.targetStockLocked || fake.stockLocked {
				t.Fatal("stock was not transferred to final order")
			}
			if len(tinyCheckoutDifferences(fake.orders["2"], *op.Order.Checkout)) != 0 {
				t.Fatal("final checkout differs")
			}
		})
	}
}

func TestTinyFinalizationRebuildsOnlyOpenReceivables(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	journal := &checkoutTestJournal{bindFailures: 1}
	fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", DueDate: "2026-09-14", Value: 49.9, Balance: 49.9}}
	if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err == nil {
		t.Fatal("bind failure missing")
	}
	if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.reversals != 1 || len(fake.accounts["1"]) != 0 || !tinyReceivablesMatch(fake.accounts["2"], op.Order.Checkout.Payments) {
		t.Fatalf("wrong receivables: %+v", fake.accounts)
	}
}

func TestTinyPaidCardSchedulePreservesTotalAndReleaseDate(t *testing.T) {
	paid := time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)
	release := paid.AddDate(0, 0, 2)
	got, err := TinyPaidInstallments(&providers.ERPOrderPayment{Method: "credit_card", Amount: 6599, Installments: 2, PaidAt: paid, MoneyReleaseDate: &release, PaymentID: "card-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].AmountCents != 3299 || got[1].AmountCents != 3300 || got[0].DueDate.Format("2006-01-02") != "2026-09-16" || !strings.Contains(got[1].Note, "card-test") {
		t.Fatalf("schedule %+v", got)
	}
}

func TestTinyReceivableMismatchCannotReportSuccessfulCheckout(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	fake.accounts["1"] = []tinyReceivable{{ID: 50, Status: "aberto", DueDate: "2026-09-14", Value: 49.9, Balance: 49.9}}
	if err := provider.SetOrderInstallments(t.Context(), "1", op.Order.Checkout.Payments); err == nil {
		t.Fatal("stale financial title ignored")
	}
}

func TestTinyFinalizationResumesReplacementUsingCustomerDeliveryAddress(t *testing.T) {
	for _, explicitMismatch := range []bool{false, true} {
		name := "missing separate address uses matching customer address"
		if explicitMismatch {
			name = "explicit delivery mismatch is preserved"
		}
		t.Run(name, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			journal := &checkoutTestJournal{}
			if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err != nil {
				t.Fatal(err)
			}
			// Reproduce the saved operation after a replacement was created but its
			// address verification stopped before any source cancellation/reversal.
			op.Completed, op.SourceCancelled = false, false
			op.SourceStockLaunched = true
			op.StockReversed, op.StockLaunched = false, false
			fake.stockLocked = true
			fake.orders["1"].Status = 0
			op.Order.Checkout.Address = &providers.ERPShippingAddress{Street: "Rua de Testes", Number: "42", Neighborhood: "Centro", City: "Sao Paulo", State: "SP", ZipCode: "01001000"}
			target := fake.orders["2"]
			target.Customer.Address = &tinyCheckoutAddress{Street: "Rua de Testes", Number: "42", Neighborhood: "Centro", City: "Sao Paulo", State: "SP", Zip: "01001-000"}
			target.Status = 0
			target.Address = nil
			if explicitMismatch {
				target.Address = &tinyCheckoutAddress{Street: "Outro destino"}
			}
			priorPosts, priorCancels := fake.posts, fake.cancels
			if err := journal.Save(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			op = journal.resume(t)
			_, err := provider.FinalizePaidCheckout(t.Context(), op, journal)
			if explicitMismatch {
				var conflict *providers.TinyCheckoutReconciliationError
				if !errors.As(err, &conflict) || conflict.OrderID != "2" || fake.cancels != priorCancels || fake.posts != priorPosts || fake.stockReversals != 0 {
					t.Fatalf("different delivery destination was approved: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !op.Completed || fake.posts != priorPosts || fake.cancels != priorCancels+1 || fake.stockReversals != 1 || fake.stockLaunches != 1 {
				t.Fatal("saved replacement was not reused")
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
				t.Fatal(err)
			}
			if fake.posts != priorPosts || fake.cancels != priorCancels+1 || fake.stockReversals != 1 || fake.stockLaunches != 1 {
				t.Fatal("completed retry repeated replacement or stock movements")
			}
		})
	}
}
