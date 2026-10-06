package erp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestTinyItemUpdatePreservesFreeItemWithoutRejectedWrites(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	provider := newTinyAgainst(t, server)
	items := []providers.ERPOrderItem{{ProductID: "101", Quantity: 1, UnitPrice: 0, Note: "Brinde"}, {ProductID: "102", Quantity: 1, UnitPrice: 30990}}
	before := append([]providers.ERPOrderItem(nil), items...)
	for range 5 {
		if err := provider.UpdateOrderItems(t.Context(), "123", items); !errors.Is(err, providers.ErrOrderItemPriceInvalid) {
			t.Fatalf("expected actionable price error, got %v", err)
		}
	}
	if calls != 0 || !reflect.DeepEqual(items, before) {
		t.Fatalf("gift was changed or rejected grid dispatched: calls=%d items=%+v", calls, items)
	}
}

func TestTinyItemUpdateStillSendsValidPrices(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Items []struct {
				Price float64 `json:"valorUnitario"`
			} `json:"itens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Items) != 1 || body.Items[0].Price != 309.90 {
			t.Errorf("wrong grid: %+v err=%v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	provider := newTinyAgainst(t, server)
	if err := provider.UpdateOrderItems(t.Context(), "123", []providers.ERPOrderItem{{ProductID: "102", Quantity: 1, UnitPrice: 30990}}); err != nil || calls != 1 {
		t.Fatalf("valid update: calls=%d err=%v", calls, err)
	}
}
