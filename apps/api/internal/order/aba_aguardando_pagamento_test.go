package order_test

// A aba "Aguardando pagamento" contra o NULL do LEFT JOIN.
//
// O matcher de "precisa de atenção" compara op.erp_finalisation_status, e `op`
// vem de LEFT JOIN: carrinho aguardando pagamento não tem Order, então a coluna
// chega NULL. No ramo NEGADO (needsAttention=false, usado por TODAS as abas de
// pipeline), a álgebra de três valores do SQL fazia o estrago em silêncio:
//
//	NOT (NULL OR false OR false)  =  NOT NULL  =  NULL  →  linha EXCLUÍDA
//
// Medido em produção em 19/08: a aba mostrava 0 de 121 carrinhos aguardando
// pagamento. O pipeline pré-pagamento inteiro, invisível, sem erro nenhum.

import (
	"context"
	"testing"

	"livecart/apps/api/internal/order"
	"livecart/apps/api/lib/query"
)

func listar(t *testing.T, storeID string, filters order.OrderFilters) []order.OrderRow {
	t.Helper()
	res, err := order.NewRepository(testPool).List(context.Background(), order.ListOrdersParams{
		StoreID:    storeID,
		Filters:    filters,
		Pagination: query.Pagination{Page: 1, Limit: 100},
		Sorting:    query.Sorting{SortBy: "created_at", SortOrder: "desc"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return res.Orders
}

func TestAbaAguardandoPagamentoMostraCarrinhoSemOrder(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "AguardaPg")
	cartID := insertCart(t, eventID, "compradora", "tok-aguarda", 9001, "pending", nil)

	f := false
	rows := listar(t, storeID, order.OrderFilters{
		Status:         []string{"active", "checkout"},
		PaymentStatus:  []string{"pending"},
		NeedsAttention: &f,
	})

	if len(rows) != 1 || rows[0].ID != cartID {
		t.Fatalf("aba devolveu %d linha(s) (%v); o carrinho pendente tem que "+
			"aparecer — sem Order não há op row, e o NULL do LEFT JOIN dentro do "+
			"NOT excluía o pipeline inteiro", len(rows), rows)
	}
}

// O outro lado: needsAttention=true continua NÃO pegando o carrinho saudável.
func TestCarrinhoSaudavelNaoEntraNaTriagem(t *testing.T) {
	requireDB(t)
	storeID, eventID := seedIsolatedStore(t, "SemTriagem")
	insertCart(t, eventID, "compradora", "tok-saudavel", 9002, "pending", nil)

	v := true
	rows := listar(t, storeID, order.OrderFilters{NeedsAttention: &v})
	if len(rows) != 0 {
		t.Fatalf("triagem pegou carrinho saudável: %v", rows)
	}
}
