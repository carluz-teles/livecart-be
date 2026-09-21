package erp

import (
	"context"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

type reflectionPriceVectorProvider struct {
	providers.ERPProvider
	lines []providers.ERPOrderItem
}

func (p *reflectionPriceVectorProvider) GetOrderItems(context.Context, string) ([]providers.ERPOrderItem, error) {
	return append([]providers.ERPOrderItem(nil), p.lines...), nil
}

func TestReflectionIdenticalPriceLotsDoesNotOverwriteSameProduct(t *testing.T) {
	svc, repo, provider, syncer := montarReflexo(map[string]int{"ext-p1": 20})
	syncer.catalogo["ext-p1"] = "p1"
	repo.criarCarrinho("cart-1", item("p1", 1))
	if err := svc.ReserveStockInERP(t.Context(), "loja-1", "cart-1", "ev-1", "p1", 1, 2000, "buyer"); err != nil {
		t.Fatal(err)
	}
	first, second := item("p1", 1), item("p1", 2)
	first.UnitPrice, second.UnitPrice = 1000, 1001
	repo.definirItens("cart-1", first, second)
	svc.collab.(*colabSimulado).erp = &reflectionPriceVectorProvider{
		ERPProvider: provider,
		lines: []providers.ERPOrderItem{
			{ProductID: "ext-p1", Quantity: 2, UnitPrice: 1001},
			{ProductID: "ext-p1", Quantity: 1, UnitPrice: 1000},
		},
	}
	report, err := svc.SyncCartFromERPOrder(t.Context(), "cart-1", "loja-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) != 0 {
		t.Fatalf("identical ERP echo rewrote agreed price lots: %+v", report)
	}
	actual := repo.carrinho("cart-1").itens
	if len(actual) != 2 || actual[0].UnitPrice != 1000 || actual[1].UnitPrice != 1001 || actual[0].Quantity != 1 || actual[1].Quantity != 2 {
		t.Fatalf("same-product price lots collapsed: %+v", actual)
	}
}
