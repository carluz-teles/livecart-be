package cartpricing

import (
	"reflect"
	"testing"
)

func TestDecode(t *testing.T) {
	lots, err := Decode([]byte(`[{"quantity":3,"waitlistedQuantity":2,"unitPrice":1001,"totalPrice":1001}]`))
	if err != nil || len(lots) != 1 || lots[0].UnitPrice != 1001 || lots[0].WaitlistedQuantity != 2 {
		t.Fatalf("decoded price agreement: %+v, %v", lots, err)
	}
	if _, err := Decode([]byte(`{"quantity":3}`)); err == nil {
		t.Fatal("invalid ledger shape accepted")
	}
}

func TestAvailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		lots []Lot
		want int64
	}{
		{name: "legacy excludes waiting", want: 9999},
		{name: "partial promotion retains exact cents", lots: []Lot{
			{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001},
			{Quantity: 2, UnitPrice: 2002},
		}, want: 5005},
		{name: "all units still waiting", lots: []Lot{{Quantity: 3, WaitlistedQuantity: 3, UnitPrice: 1001}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Available(tc.lots, 3, 2, 9999); got != tc.want {
				t.Fatalf("available=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestReserved(t *testing.T) {
	lots := []Lot{
		{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001},
		{Quantity: 2, UnitPrice: 2002},
		{Quantity: 1, UnitPrice: 1001},
	}
	want := []Lot{{Quantity: 2, UnitPrice: 1001, TotalPrice: 2002}, {Quantity: 2, UnitPrice: 2002, TotalPrice: 4004}}
	if got := Reserved(lots, 6, 2, 9999); !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway lines=%+v want=%+v", got, want)
	}
	if lots[0].Quantity != 3 || lots[0].WaitlistedQuantity != 2 {
		t.Fatal("projection changed the source ledger")
	}
}

func TestCovered(t *testing.T) {
	for _, tc := range []struct {
		name string
		lots []Lot
		paid int
		want int64
	}{
		{name: "legacy excludes waiting", paid: 3, want: 1001},
		{name: "oldest available units first", paid: 2, want: 3003, lots: []Lot{
			{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001},
			{Quantity: 2, UnitPrice: 2002},
		}},
		{name: "pending older lot consumes no coverage", paid: 1, want: 2002, lots: []Lot{
			{Quantity: 3, WaitlistedQuantity: 3, UnitPrice: 1001},
			{Quantity: 2, UnitPrice: 2002},
		}},
		{name: "coverage cannot exceed available units", paid: 4, want: 5005, lots: []Lot{
			{Quantity: 3, WaitlistedQuantity: 2, UnitPrice: 1001},
			{Quantity: 2, UnitPrice: 2002},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Covered(tc.lots, 3, 2, 1001, tc.paid); got != tc.want {
				t.Fatalf("covered=%d want=%d", got, tc.want)
			}
		})
	}
}
