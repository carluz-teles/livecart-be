package erp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyCheckoutContactResolution(t *testing.T) {
	for _, tt := range []struct {
		name           string
		sourceDocument string
		searchStatus   int
		items          []map[string]any
		total          int
		document       string
		wantID         string
		wantError      string
	}{
		{name: "existing CPF formatted", document: "529.982.247-25", wantID: "9"},
		{name: "existing CPF digits", document: "52998224725", wantID: "9"},
		{name: "existing CNPJ digits", document: "11222333000181", items: []map[string]any{{"id": 9, "cpfCnpj": "11.222.333/0001-81", "situacao": "B"}}, wantID: "9"},
		{name: "new buyer enriches placeholder", document: "52998224725", items: []map[string]any{}, wantID: "8"},
		{name: "empty search verifies same document", document: "52998224725", items: []map[string]any{}, sourceDocument: "529.982.247-25", wantID: "8"},
		{name: "no document preserves current behavior", wantID: "8"},
		{name: "invalid document", document: "123", wantError: "inválido"},
		{name: "rate limit is not absence", document: "52998224725", searchStatus: 429, wantError: "429"},
		{name: "provider unavailable is not absence", document: "52998224725", searchStatus: 503, wantError: "503"},
		{name: "permission denied is not absence", document: "52998224725", searchStatus: 403, wantError: "403"},
		{name: "partial results", document: "52998224725", total: 11, wantError: "incompleta"},
		{name: "document mismatch", document: "52998224725", items: []map[string]any{{"id": 9, "cpfCnpj": "111.444.777-35", "situacao": "B"}}, wantError: "divergente"},
		{name: "missing document", document: "52998224725", items: []map[string]any{{"id": 9, "situacao": "B"}}, wantError: "divergente"},
		{name: "inactive contact", document: "52998224725", items: []map[string]any{{"id": 9, "cpfCnpj": "529.982.247-25", "situacao": "I"}}, wantError: "inativo"},
		{name: "deleted contact", document: "52998224725", items: []map[string]any{{"id": 9, "cpfCnpj": "529.982.247-25", "situacao": "E"}}, wantError: "excluído"},
		{name: "multiple identities", document: "52998224725", items: []map[string]any{{"id": 10, "cpfCnpj": "529.982.247-25", "situacao": "B"}, {"id": 9, "cpfCnpj": "529.982.247-25", "situacao": "A"}}, wantError: "mais de um"},
		{name: "cannot overwrite another buyer", document: "52998224725", items: []map[string]any{}, sourceDocument: "111.444.777-35", wantError: "cadastro original preservado"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider, fake, _ := checkoutFinalizationFixture(t)
			fake.contacts = map[string]map[string]any{
				"8": {"id": 8, "cpfCnpj": tt.sourceDocument, "situacao": "B"},
				"9": {"id": 9, "cpfCnpj": formatBrazilianDocument(tt.document), "situacao": "A"},
			}
			fake.contactSearchStatus, fake.contactSearchItems, fake.contactSearchTotal = tt.searchStatus, tt.items, tt.total
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := provider.resolveCheckoutContact(ctx, "8", providers.ERPContactInput{CpfCnpj: tt.document})
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("wanted %q, got %v", tt.wantError, err)
				}
			} else if err != nil || got != tt.wantID {
				t.Fatalf("wanted %s, got %s err=%v", tt.wantID, got, err)
			}
			if fake.writes != 0 {
				t.Fatal("contact resolution performed a write")
			}
		})
	}
}

func TestTinyCheckoutContactRejectsUnverifiableResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body, detail string
		wantError          string
	}{
		{name: "missing list", body: `{}`, wantError: "sem lista verificável"},
		{name: "null list", body: `{"itens":null}`, wantError: "sem lista verificável"},
		{name: "malformed response", body: `invalid`, wantError: "decoding"},
		{name: "contact changed after lookup", body: `{"itens":[{"id":9,"cpfCnpj":"529.982.247-25","situacao":"B"}]}`, detail: `{"id":9,"cpfCnpj":"111.444.777-35","situacao":"B"}`, wantError: "cadastro original preservado"},
		{name: "inactive fallback", body: `{"itens":[]}`, detail: `{"id":8,"situacao":"I"}`, wantError: "inativo"},
		{name: "204 absence verifies placeholder", detail: `{"id":8,"situacao":"B"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation %s", r.Method)
				}
				if r.URL.Path == "/contatos" {
					if tt.body == "" {
						w.WriteHeader(204)
						return
					}
					_, _ = w.Write([]byte(tt.body))
					return
				}
				_, _ = w.Write([]byte(tt.detail))
			}))
			defer srv.Close()
			provider := newTinyAgainst(t, srv)
			_, err := provider.resolveCheckoutContact(t.Context(), "8", providers.ERPContactInput{CpfCnpj: "52998224725"})
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("wanted %s, got %v", tt.wantError, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTinyFinalizationContactFailurePreservesSource(t *testing.T) {
	for _, failure := range []string{"lookup", "checkpoint"} {
		t.Run(failure, func(t *testing.T) {
			provider, fake, op := checkoutFinalizationFixture(t)
			fake.contacts = map[string]map[string]any{
				"8": {"id": 8, "nome": "instagram_reserva", "situacao": "B"},
				"9": {"id": 9, "nome": "Comprador Teste", "cpfCnpj": "529.982.247-25", "situacao": "B"},
			}
			fake.orders["1"].Customer.ID = 8
			fake.orders["1"].Customer.Name = "instagram_reserva"
			op.Order.Checkout.Customer.CpfCnpj = "52998224725"
			// A checkpoint from the old code can already require account reversal.
			op.Prepared, op.Replace, op.AccountsRequired, op.SourceStockLaunched = true, true, true, true
			fake.stockLocked = true
			fake.accounts["1"] = []tinyReceivable{{ID: 44, Value: 49.9, Balance: 49.9, Status: "aberto"}}
			journal := &checkoutTestJournal{}
			if err := journal.Save(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			fake.contactSearchStatus = 0
			if failure == "lookup" {
				fake.contactSearchStatus = 503
			} else {
				journal.failContactSave = true
			}
			if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err == nil {
				t.Fatal("expected contact failure")
			}
			if fake.posts != 0 || fake.cancels != 0 || fake.reversals != 0 || fake.stockReversals != 0 || len(fake.contactUpdates) != 0 {
				t.Fatal("failed lookup/checkpoint modified source, contacts, finances or stock")
			}
			restored := journal.resume(t)
			if restored.CreateStarted || restored.Order.ContactID != "8" {
				t.Fatal("failed contact checkpoint advanced creation")
			}
			fake.contactSearchStatus = 0
			if _, err := provider.FinalizePaidCheckout(t.Context(), restored, journal); err != nil {
				t.Fatal(err)
			}
			if fake.posts != 1 || fake.orders["2"].Customer.ID != 9 {
				t.Fatal("retry failed to resolve and create correctly")
			}
			var persisted providers.TinyCheckoutOperation
			if err := json.Unmarshal(journal.raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if !persisted.Completed || persisted.Order.ContactID != "9" {
				t.Fatal("resolved contact not durable")
			}
		})
	}
}

func TestTinyCheckoutContactIgnoresDeletedAndReusesEquivalentActive(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	customer := providers.ERPContactInput{Name: "Compradóra Teste", CpfCnpj: "52998224725", Email: "buyer@example.invalid", Phone: "11900000000"}
	fake.contacts = map[string]map[string]any{
		"8":  {"id": 8, "nome": "instagram_reserva", "cpfCnpj": "", "situacao": "B"},
		"9":  {"id": 9, "nome": "Compradora Teste", "cpfCnpj": "529.982.247-25", "situacao": "A", "email": customer.Email, "celular": customer.Phone},
		"10": {"id": 10, "nome": "Compradora Teste", "cpfCnpj": "529.982.247-25", "situacao": "A", "email": customer.Email, "celular": customer.Phone},
		"11": {"id": 11, "nome": "Compradora Teste", "cpfCnpj": "529.982.247-25", "situacao": "E"},
	}
	fake.orders["1"].Customer.ID = 8
	op.Order.Checkout.Customer = customer
	fake.contactSearchItems = []map[string]any{fake.contacts["10"], fake.contacts["11"], fake.contacts["9"]}
	journal := &checkoutTestJournal{}
	if _, err := provider.FinalizePaidCheckout(t.Context(), op, journal); err != nil {
		t.Fatal(err)
	}
	if fake.orders["2"].Customer.ID != 9 || fake.posts != 1 || !op.Completed {
		t.Fatal("equivalent active contact not reused")
	}
	if len(fake.contactUpdates) != 1 || fake.contactUpdates[0] != "9" {
		t.Fatal("wrong contact updated")
	}
	for _, id := range []string{"10", "11"} {
		if fake.contacts[id]["nome"] != "Compradora Teste" {
			t.Fatal("another contact was modified")
		}
	}
	if _, err := provider.FinalizePaidCheckout(t.Context(), journal.resume(t), journal); err != nil {
		t.Fatal(err)
	}
	if fake.posts != 1 || len(fake.contactUpdates) != 1 {
		t.Fatal("completed retry duplicated writes")
	}
}

func TestTinyCheckoutContactDuplicateSelection(t *testing.T) {
	for _, scenario := range []string{"deleted first", "current binding", "reordered candidates", "distinct buyers", "current changed after lookup", "only deleted"} {
		t.Run(scenario, func(t *testing.T) {
			provider, fake, _ := checkoutFinalizationFixture(t)
			customer := providers.ERPContactInput{Name: "Compradora Teste", CpfCnpj: "52998224725", Email: "buyer@example.invalid", Phone: "11900000000"}
			fake.contacts = map[string]map[string]any{
				"8":  {"id": 8, "nome": "reserva", "situacao": "B"},
				"9":  {"id": 9, "nome": customer.Name, "cpfCnpj": "529.982.247-25", "situacao": "B", "email": customer.Email, "celular": customer.Phone},
				"10": {"id": 10, "nome": customer.Name, "cpfCnpj": "529.982.247-25", "situacao": "A", "email": customer.Email, "telefone": customer.Phone},
				"11": {"id": 11, "cpfCnpj": "529.982.247-25", "situacao": "E"},
			}
			fake.contactSearchItems = []map[string]any{fake.contacts["11"], fake.contacts["9"], fake.contacts["10"]}
			current, wanted := "8", "9"
			switch scenario {
			case "current binding":
				current, wanted = "10", "10"
			case "reordered candidates":
				fake.contactSearchItems = []map[string]any{fake.contacts["10"], fake.contacts["9"], fake.contacts["11"]}
			case "distinct buyers":
				fake.contacts["9"]["email"] = "different@example.invalid"
				fake.contacts["10"]["telefone"] = "11888888888"
			case "current changed after lookup":
				stale := map[string]any{}
				for k, v := range fake.contacts["9"] {
					stale[k] = v
				}
				fake.contactSearchItems = []map[string]any{stale, fake.contacts["10"]}
				fake.contacts["9"]["email"] = "changed@example.invalid"
			case "only deleted":
				fake.contactSearchItems = []map[string]any{fake.contacts["11"]}
			}
			got, err := provider.resolveCheckoutContact(t.Context(), current, customer)
			conflictExpected := scenario == "distinct buyers" || scenario == "current changed after lookup" || scenario == "only deleted"
			if conflictExpected {
				var conflict *tinyCheckoutContactConflict
				if !errors.As(err, &conflict) {
					t.Fatalf("expected typed conflict, got %v", err)
				}
			} else if err != nil || got != wanted {
				t.Fatalf("selected %s, wanted %s, error=%v", got, wanted, err)
			}
			if fake.writes != 0 {
				t.Fatal("selection wrote to Tiny")
			}
		})
	}
}

func TestTinyFinalizationContactConflictIsReconciliation(t *testing.T) {
	provider, fake, op := checkoutFinalizationFixture(t)
	op.Order.Checkout.Customer.CpfCnpj = "52998224725"
	fake.contactSearchItems = []map[string]any{{"id": 9, "cpfCnpj": "529.982.247-25", "situacao": "E"}}
	_, err := provider.FinalizePaidCheckout(t.Context(), op, &checkoutTestJournal{})
	var conflict *providers.TinyCheckoutReconciliationError
	if !errors.As(err, &conflict) || conflict.OrderID != "1" {
		t.Fatalf("expected user-facing reconciliation, got %v", err)
	}
	if fake.posts != 0 || fake.cancels != 0 || len(fake.contactUpdates) != 0 {
		t.Fatal("conflict modified the reservation")
	}
}
