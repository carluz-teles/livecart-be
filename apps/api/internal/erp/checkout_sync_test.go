package erp

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

type checkoutCollaborator struct {
	*colabSimulado
	snapshot providers.ERPOrderCheckout
	err      error
}

func (c *checkoutCollaborator) LoadERPOrderCheckout(context.Context, string, string) (providers.ERPOrderCheckout, error) {
	return c.snapshot, c.err
}

type checkoutProvider struct {
	*erpSimulado
	syncs    int
	snapshot providers.ERPOrderCheckout
	err      error
}

func (p *checkoutProvider) SyncOrderCheckout(_ context.Context, _ string, snapshot providers.ERPOrderCheckout) error {
	p.syncs++
	p.snapshot = snapshot
	return p.err
}

func TestConfirmPaidCheckoutBeforeApproval(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		readFailure, writeFailure bool
	}{
		{name: "commercial snapshot and ledger replace legacy payment"},
		{name: "snapshot read failure blocks approval", readFailure: true},
		{name: "checkout verification failure blocks approval", writeFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, base, collab := montar(map[string]int{"ext-p1": 10})
			provider := &checkoutProvider{erpSimulado: base}
			loader := &checkoutCollaborator{colabSimulado: collab, snapshot: providers.ERPOrderCheckout{FreightCents: 2500, DiscountCents: 200, Payments: []providers.ERPInstallment{{AmountCents: 4300, Method: "pix"}}}}
			collab.erp = provider
			svc.collab = loader
			ctx := context.Background()
			repo.criarCarrinho("cart-1", item("p1", 1))
			if err := svc.EnsureERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("checkout unavailable")
			if tc.readFailure {
				loader.err = failure
			}
			if tc.writeFailure {
				provider.err = failure
			}
			err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", &providers.PaymentStatus{PaymentMethod: "pix", PaymentID: "pay-1"})
			if tc.readFailure || tc.writeFailure {
				if !errors.Is(err, failure) {
					t.Fatalf("missing failure: %v", err)
				}
				if base.situacoes != 0 || repo.carrinho("cart-1").state != OrderStateOpen || len(collab.falhasMarcadas) == 0 {
					t.Fatal("failed checkout was approved or claim/failure state was not restored")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if provider.syncs != 1 || provider.snapshot.FreightCents != 2500 || base.situacoes != 1 || base.pagamentos != 0 {
					t.Fatal("commercial snapshot discarded or legacy item-only payment overwrote checkout")
				}
				if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
					t.Fatal(err)
				}
				if provider.syncs != 1 {
					t.Fatal("confirmed redelivery duplicated checkout writes")
				}
			}
		})
	}
}

type commercialDiscountProvider struct {
	*erpComParcelas
	discount int64
}

func (p *commercialDiscountProvider) GetOrderCommercialDiscount(context.Context, string) (int64, error) {
	return p.discount, nil
}

func TestRecomposeDoesNotDuplicateBlingCommercialDiscount(t *testing.T) {
	svc, repo, base := montarParcelas(map[string]int{"ext-p1": 20})
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 2))
	if err := svc.EnsureERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	pagar(t, svc, repo, "cart-1", 4000)
	base.usarForcado = true
	base.totalForcado = 6000
	// The ledger's R$ 40 gross/R$ 38 paid is a R$ 2 discount already
	// included in Bling's total. Only R$ 22 remains due, not R$ 20 + a
	// fake discount receivable.
	repo.mu.Lock()
	repo.carrinhos["cart-1"].livro[0].AmountCents = 3800
	repo.mu.Unlock()
	svc.collab.(*colabSimulado).erp = &commercialDiscountProvider{erpComParcelas: base, discount: 200}
	split, err := svc.RecomporParcelasDoPedidoPago(ctx, "cart-1", "loja-1")
	if err != nil {
		t.Fatal(err)
	}
	if split.DescontoCents != 0 || split.SaldoCents != 2200 {
		t.Fatalf("discount counted twice: %+v", split)
	}
}

func TestAdditionalBlingPaymentReconcilesConfirmedCheckoutWithoutApproval(t *testing.T) {
	svc, repo, base, collab := montar(map[string]int{"ext-p1": 10})
	repo.provider = "bling"
	provider := &checkoutProvider{erpSimulado: base}
	loader := &checkoutCollaborator{colabSimulado: collab, snapshot: providers.ERPOrderCheckout{Payments: []providers.ERPInstallment{{AmountCents: 2000, Method: "pix", DueDate: time.Now()}}}}
	collab.erp = provider
	svc.collab = loader
	ctx := context.Background()
	repo.criarCarrinho("cart-1", item("p1", 1))
	if err := svc.EnsureERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmERPOrderPayment(ctx, "cart-1", "loja-1", nil); err != nil {
		t.Fatal(err)
	}
	orderID := repo.carrinho("cart-1").externalOrderID
	loader.snapshot.FreightCents = 900
	loader.snapshot.Payments = append(loader.snapshot.Payments, providers.ERPInstallment{AmountCents: 900, Method: "pix", DueDate: time.Now()})
	if err := svc.OnCartPaidBlingCheckout(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	if provider.syncs != 2 || len(provider.snapshot.Payments) != 2 || provider.snapshot.FreightCents != 900 {
		t.Fatal("supplemental freight/payment did not reach provider")
	}
	if base.situacoes != 1 || repo.carrinho("cart-1").externalOrderID != orderID || repo.carrinho("cart-1").state != OrderStateConfirmed {
		t.Fatal("supplemental payment recreated/reapproved the order or failed to restore confirmed state")
	}
	provider.err = errors.New("ERP unavailable")
	if err := svc.OnCartPaidBlingCheckout(ctx, "cart-1", "loja-1"); !errors.Is(err, provider.err) {
		t.Fatalf("missing failure: %v", err)
	}
	if repo.carrinho("cart-1").state != OrderStateConfirmed || len(collab.falhasMarcadas) == 0 {
		t.Fatal("failed supplemental sync lost state or failure diagnosis")
	}
	provider.err = nil
	if err := svc.RetryERPFinalisation(ctx, "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	if base.situacoes != 1 {
		t.Fatal("retry approved order twice")
	}
}

func TestAdditionalBlingPaymentRespectsFirstPaymentAndBusyOrInvoicedOrders(t *testing.T) {
	for _, tc := range []struct {
		name, state, status, provider string
		locked, wantBusy, wantInvoice bool
	}{
		{name: "first payment not yet converted", state: OrderStateNone, provider: "bling"},
		{name: "first payment order open", state: OrderStateOpen, provider: "bling"},
		{name: "first payment creation running retries", state: OrderStateConverting, provider: "bling", wantBusy: true},
		{name: "mutation in flight retries", state: OrderStateMutating, provider: "bling", wantBusy: true},
		{name: "reflection in flight retries", state: OrderStateReflecting, provider: "bling", wantBusy: true},
		{name: "cart lock held retries", state: OrderStateConfirmed, provider: "bling", locked: true, wantBusy: true},
		{name: "invoiced order blocked", state: OrderStateConfirmed, status: "faturado", provider: "bling", wantInvoice: true},
		{name: "Tiny untouched", state: OrderStateConfirmed, provider: "tiny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, base, collab := montar(map[string]int{"ext-p1": 10})
			repo.provider = tc.provider
			provider := &checkoutProvider{erpSimulado: base}
			collab.erp = provider
			svc.collab = &checkoutCollaborator{colabSimulado: collab}
			repo.criarCarrinho("cart-1", item("p1", 1))
			repo.mu.Lock()
			repo.carrinhos["cart-1"].state = tc.state
			repo.carrinhos["cart-1"].statusERP = tc.status
			repo.carrinhos["cart-1"].externalOrderID = "42"
			repo.travados["cart-1"] = tc.locked
			repo.mu.Unlock()
			err := svc.OnCartPaidBlingCheckout(context.Background(), "cart-1", "loja-1")
			if tc.wantBusy && !errors.Is(err, ErrOrderBusy) {
				t.Fatalf("expected retry: %v", err)
			}
			if tc.wantInvoice && !errors.Is(err, ErrPedidoFaturado) {
				t.Fatalf("expected invoice protection: %v", err)
			}
			if !tc.wantBusy && !tc.wantInvoice && err != nil {
				t.Fatal(err)
			}
			if provider.syncs != 0 || base.situacoes != 0 || base.pagamentos != 0 || repo.carrinho("cart-1").state != tc.state {
				t.Fatal("unrelated/first/in-flight order was mutated")
			}
		})
	}
}
