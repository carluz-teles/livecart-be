package live

import (
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

func TestCartERPBoundaryPreservesClosedPurchase(t *testing.T) {
	requireDB(t)
	statuses := []string{"", "em_aberto", "aprovado", "preparando_envio", "faturado",
		"pronto_envio", "enviado", "entregue", "nao_entregue", "cancelado"}
	for _, vip := range []bool{false, true} {
		for _, status := range statuses {
			t.Run(status+map[bool]string{true: "_vip", false: "_regular"}[vip], func(t *testing.T) {
				storeID := seedStore(t)
				eventID := seedEventInStore(t, storeID)
				get := func() (*CartRow, bool) {
					if vip {
						return getOrCreateVip(t, storeID, eventID, "buyer")
					}
					return getOrCreate(t, eventID, "buyer")
				}
				old, _ := get()
				addItem(t, old.ID, 2, 8290)
				if _, err := testPool.Exec(t.Context(), `UPDATE carts SET erp_order_status=$2 WHERE id=$1`, old.ID, status); err != nil {
					t.Fatal(err)
				}
				fresh, created := get()
				closed := providers.ERPOrderStatus(status).FechadoParaNovosItens()
				if created != closed || (fresh.ID != old.ID) != closed {
					t.Fatalf("status=%q vip=%v closed=%v created=%v same=%v", status, vip, closed, created, fresh.ID == old.ID)
				}
				if itemCount(t, old.ID) != 1 {
					t.Fatal("previous purchase lost its items")
				}
				if again, newCart := get(); newCart || again.ID != fresh.ID {
					t.Fatal("subsequent comment did not reuse the new purchase")
				}
			})
		}
	}
}
