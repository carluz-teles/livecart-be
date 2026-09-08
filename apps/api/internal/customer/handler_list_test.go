//go:build integration

package customer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCustomerListHTTP_FilterBeforePagination(t *testing.T) {
	requireCustDB(t)
	ctx := context.Background()
	seed := seedLojaComLive(t, "list-"+uuid.NewString())
	other := seedLojaComLive(t, "other-list-"+uuid.NewString())
	for i := 1; i <= 23; i++ {
		handle := fmt.Sprintf("cliente_%02d", i)
		_, err := custTestPool.Exec(ctx, `INSERT INTO customers (store_id, platform_user_id, platform_handle, email, last_order_at) VALUES ($1, $2::text, $2::text, $2::text || '@example.test', now() - $3::int * interval '1 minute')`, seed.storeID, handle, i)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := custTestPool.Exec(ctx, `INSERT INTO blocked_handles (store_id, platform_handle) VALUES ($1, 'cliente_23'), ($2, 'cliente_01')`, seed.storeID, other.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := custTestPool.Exec(ctx, `INSERT INTO blocked_handles (store_id, platform_handle, unblocked_at) VALUES ($1, 'cliente_22', now())`, seed.storeID); err != nil {
		t.Fatal(err)
	}
	if _, err := custTestPool.Exec(ctx, `UPDATE customers SET created_at = '2026-09-08T02:30:00Z' WHERE store_id = $1`, seed.storeID); err != nil {
		t.Fatal(err)
	}
	app := appDePerfis(t, seed.storeID)
	cases := []struct {
		name, query, first string
		count, total       int
		blocked            bool
	}{
		{name: "first page", query: "limit=20", first: "cliente_01", count: 20, total: 23},
		{name: "second page", query: "limit=20&page=2", first: "cliente_21", count: 3, total: 23},
		{name: "blocked from second page", query: "blockedOnly=true", first: "cliente_23", count: 1, total: 1, blocked: true},
		{name: "search with at sign", query: "search=%40cliente_22", first: "cliente_22", count: 1, total: 1},
		{name: "search email", query: "search=cliente_23%40example.test", first: "cliente_23", count: 1, total: 1, blocked: true},
		{name: "blocked and search", query: "blockedOnly=true&search=cliente_22", count: 0, total: 0},
		{name: "registration date in Brazil", query: "dateFrom=2026-09-07&dateTo=2026-09-07", first: "cliente_01", count: 20, total: 23},
		{name: "outside date range", query: "dateFrom=2026-09-08", count: 0, total: 0},
		{name: "no results", query: "search=missing", count: 0, total: 0},
		{name: "literal wildcard", query: "search=%25", count: 0, total: 0},
		{name: "out of range retains correct total", query: "search=cliente_22&page=3", count: 0, total: 1},
		{name: "ascending requested", query: "sortBy=last_order_at&sortOrder=asc", first: "cliente_23", count: 20, total: 23, blocked: true},
		{name: "only paid buyers", query: "hasOrders=true", count: 0, total: 0},
		{name: "without paid orders", query: "hasOrders=false&orderCountMax=0&totalSpentMax=0", first: "cliente_01", count: 20, total: 23},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := app.Test(httptest.NewRequest("GET", "/customers/?"+tc.query, nil), 5000)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var envelope struct {
				Data ListCustomersResponse `json:"data"`
			}
			if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 200 {
				t.Fatalf("status: %d", res.StatusCode)
			}
			got := envelope.Data
			if len(got.Data) != tc.count || got.Pagination.Total != tc.total {
				t.Fatalf("got %d rows/%d total, want %d/%d", len(got.Data), got.Pagination.Total, tc.count, tc.total)
			}
			if tc.count > 0 && (got.Data[0].Handle != tc.first || got.Data[0].Blocked == nil || *got.Data[0].Blocked != tc.blocked) {
				t.Fatalf("first customer: %+v", got.Data[0])
			}
		})
	}
}

func TestCustomerListHTTP_PaidAmountFilters(t *testing.T) {
	requireCustDB(t)
	seed := seedF4Cust(t)
	app := appDePerfis(t, seed.storeID)
	cases := []struct {
		name, query string
		count       int
	}{
		{"paid totals match", "hasOrders=true&orderCountMin=1&orderCountMax=1&totalSpentMin=30000&totalSpentMax=30000", 1},
		{"unpaid excluded", "totalSpentMin=30001", 0},
		{"maximum amount", "totalSpentMax=29999", 0},
		{"minimum count", "orderCountMin=2", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := app.Test(httptest.NewRequest("GET", "/customers/?"+tc.query, nil), 5000)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var envelope struct {
				Data ListCustomersResponse `json:"data"`
			}
			if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != 200 || len(envelope.Data.Data) != tc.count || envelope.Data.Pagination.Total != tc.count {
				t.Fatalf("status %d result %+v", res.StatusCode, envelope.Data)
			}
		})
	}
}

func TestCustomerListHTTP_InvalidFilters(t *testing.T) {
	requireCustDB(t)
	seed := seedLojaComLive(t, "invalid-list-"+uuid.NewString())
	app := appDePerfis(t, seed.storeID)
	for _, query := range []string{"dateFrom=invalid", "dateFrom=2026-09-08&dateTo=2026-09-07", "orderCountMin=2147483648"} {
		t.Run(query, func(t *testing.T) {
			res, err := app.Test(httptest.NewRequest("GET", "/customers/?"+query, nil), 5000)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 400 {
				t.Fatalf("status %d, expected 400", res.StatusCode)
			}
		})
	}
}
