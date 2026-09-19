package erp

import (
	"context"
	"errors"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

type paymentProjection struct {
	CartReopener
	calls  int
	amount int64
	fail   bool
}

func (p *paymentProjection) MarkCartPaidFromERP(_ context.Context, _, _ string, amount int64) (bool, error) {
	p.calls++
	p.amount = amount
	if p.fail {
		return false, errors.New("temporary database failure")
	}
	return true, nil
}

func TestApprovedPaymentRetriesWithoutDuplicatingStatusHistory(t *testing.T) {
	svc, repo, provider := montarParcelas(map[string]int{"ext-p1": 20})
	repo.criarCarrinho("cart-1", item("p1", 1))
	if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	orderID := repo.carrinho("cart-1").externalOrderID
	projection := &paymentProjection{fail: true}
	svc.SetCartReopener(projection)
	observe := func() error {
		return svc.ObserveOrderStatus(t.Context(), "loja-1", orderID, "1", providers.ERPOrderStatusAprovado, StatusSourceWebhook, nil)
	}
	if err := observe(); err == nil {
		t.Fatal("failed payment projection must remain retryable")
	}
	history := len(repo.statusEventos)
	projection.fail = false
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	if projection.calls != 2 || projection.amount != 2000 {
		t.Fatalf("payment calls=%d amount=%d", projection.calls, projection.amount)
	}
	if len(repo.statusEventos) != history {
		t.Fatal("redelivery duplicated status history")
	}
	provider.usarForcado = true
	provider.totalForcado = 0
	if err := observe(); err == nil {
		t.Fatal("unverified zero payment must not be recorded")
	}
	if projection.calls != 2 {
		t.Fatal("recorded payment with unknown amount")
	}
}

func TestInvoiceStatusAloneDoesNotCreatePayment(t *testing.T) {
	svc, repo, _ := montarParcelas(map[string]int{"ext-p1": 20})
	repo.criarCarrinho("cart-1", item("p1", 1))
	if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	projection := &paymentProjection{}
	svc.SetCartReopener(projection)
	if err := svc.ObserveOrderStatus(t.Context(), "loja-1", repo.carrinho("cart-1").externalOrderID, "1", providers.ERPOrderStatusFaturado, StatusSourceWebhook, nil); err != nil {
		t.Fatal(err)
	}
	if projection.calls != 0 {
		t.Fatal("invoicing was treated as receipt")
	}
}
