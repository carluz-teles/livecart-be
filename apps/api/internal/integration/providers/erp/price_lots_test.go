package erp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyUpdateItemsPreservesDuplicateSKUPrices(t *testing.T) {
	var received struct {
		Items []struct {
			Product struct {
				ID int64 `json:"id"`
			} `json:"produto"`
			Quantity int     `json:"quantidade"`
			Price    float64 `json:"valorUnitario"`
		} `json:"itens"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/pedidos/1/itens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	provider := newTinyAgainst(t, server)
	if err := provider.UpdateOrderItems(t.Context(), "1", []providers.ERPOrderItem{
		{ProductID: "42", Quantity: 1, UnitPrice: 1000}, {ProductID: "42", Quantity: 2, UnitPrice: 1001},
	}); err != nil {
		t.Fatal(err)
	}
	if len(received.Items) != 2 || received.Items[0].Product.ID != 42 || received.Items[1].Product.ID != 42 || received.Items[0].Price != 10 || received.Items[1].Price != 10.01 || received.Items[1].Quantity != 2 {
		t.Fatalf("distinct agreed prices changed in payload: %+v", received)
	}
}

func TestBlingItemsAndTinyComparisonPreservePriceVector(t *testing.T) {
	lines := []providers.ERPOrderItem{{ProductID: "42", Quantity: 1, UnitPrice: 1000}, {ProductID: "42", Quantity: 2, UnitPrice: 1001}}
	converted := blingItens(lines)
	if len(converted) != 2 || converted[0].Valor != 10 || converted[1].Valor != 10.01 || converted[0].Produto.ID != converted[1].Produto.ID {
		t.Fatalf("Bling collapsed distinct prices: %+v", converted)
	}
	source := &tinyCheckoutOrder{}
	// Read the same wire shape used by Tiny's order GET.
	if err := json.Unmarshal([]byte(`{"itens":[{"produto":{"id":42},"quantidade":1,"valorUnitario":10},{"produto":{"id":42},"quantidade":2,"valorUnitario":10.01}]}`), source); err != nil {
		t.Fatal(err)
	}
	if !tinyCheckoutGridMatches(source, lines) {
		t.Fatal("matching multi-price grid rejected")
	}
	if tinyCheckoutGridMatches(source, []providers.ERPOrderItem{{ProductID: "42", Quantity: 3, UnitPrice: 1001}}) {
		t.Fatal("same quantity at a different price treated as matching")
	}
}
