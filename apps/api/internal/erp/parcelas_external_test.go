package erp

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"livecart/apps/api/internal/integration/providers"
)

type recordedPaymentsProvider struct {
	*erpComParcelas
	checks int
	fail   error
}

func (p *recordedPaymentsProvider) Name() providers.ProviderName { return providers.ProviderTiny }

func (p *recordedPaymentsProvider) VerifyRecordedOrderPayment(_ context.Context, _ string, amount int64) error {
	p.checks++
	if amount <= 0 {
		return errors.New("invalid amount")
	}
	return p.fail
}

func TestTinyExternalPaymentReviewNeverRewritesMerchantInstallments(t *testing.T) {
	for _, name := range []string{
		"verified receipts", "verified invoiced receipts", "commercial discount already in total",
		"unreceived titles", "invoiced without receipts", "ledger exceeds total", "new unpaid items",
		"mixed payment sources", "different ERP order", "invalid ledger", "ledger overflow",
		"review storage unavailable", "retry after settlement",
	} {
		t.Run(name, func(t *testing.T) {
			svc, repo, base := montarParcelas(map[string]int{"ext-p1": 50})
			repo.criarCarrinho("cart-1", item("p1", 5))
			if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
				t.Fatal(err)
			}
			pagar(t, svc, repo, "cart-1", 10000)
			cart := repo.carrinho("cart-1")
			cart.livro[0].Method = providers.PaymentMethodERPManual
			cart.livro[0].CheckoutID = "erp-" + cart.externalOrderID
			provider := &recordedPaymentsProvider{erpComParcelas: base}
			svc.collab.(*colabSimulado).erp = provider
			recorder := &reviewRecordingRepo{repoSimulado: repo, review: &PaymentReview{Reason: "installments_unverified"}}
			svc.repo = recorder
			wantVerified, wantErr := false, true
			failure := errors.New("receipts unavailable")
			switch name {
			case "verified receipts":
				wantVerified, wantErr = true, false
			case "verified invoiced receipts":
				base.usarForcado, base.invoiced, base.totalForcado = true, true, 10000
				wantVerified, wantErr = true, false
			case "commercial discount already in total":
				cart.livro[0].GrossCoveredCents = 12000
				wantVerified, wantErr = true, false
			case "unreceived titles", "retry after settlement":
				provider.fail = failure
			case "invoiced without receipts":
				base.usarForcado, base.invoiced, base.totalForcado = true, true, 10000
				provider.fail = failure
			case "ledger exceeds total":
				base.usarForcado, base.totalForcado = true, 9766
				wantErr = false
			case "new unpaid items":
				base.usarForcado, base.totalForcado = true, 11000
			case "mixed payment sources":
				cart.livro[0].AmountCents = 9000
				cart.livro = append(cart.livro, CartPayment{AmountCents: 1000, Method: "pix", CheckoutID: "gateway"})
			case "different ERP order":
				cart.livro[0].CheckoutID = "erp-other-order"
			case "invalid ledger":
				cart.livro[0].AmountCents = -100
			case "ledger overflow":
				cart.livro[0].AmountCents = math.MaxInt64
				cart.livro = append(cart.livro, CartPayment{AmountCents: 1})
			case "review storage unavailable":
				recorder.fail = failure
				wantVerified = true
			}
			repo.mu.Lock()
			repo.carrinhos["cart-1"].livro = cart.livro
			repo.mu.Unlock()
			before, err := repo.ListCartPayments(t.Context(), "cart-1")
			if err != nil {
				t.Fatal(err)
			}
			split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
			if (err != nil) != wantErr {
				t.Fatalf("unexpected result: split=%+v err=%v", split, err)
			}
			if split != nil && (split.Verified != wantVerified || split.Reescrito) {
				t.Fatalf("unexpected verification: %+v", split)
			}
			if wantVerified && split == nil {
				t.Fatal("missing verified reconciliation")
			}
			if base.escritas != 0 {
				t.Fatal("merchant installments were overwritten")
			}
			if provider.fail != nil && !errors.Is(err, failure) {
				t.Fatal("receipt verification error lost its cause")
			}
			if wantVerified && !wantErr {
				if provider.checks != 1 || recorder.review != nil || recorder.resolved != 1 {
					t.Fatal("verified receipts did not resolve the review")
				}
			} else if recorder.review == nil || recorder.resolved != 0 {
				t.Fatal("unverified financial review was cleared")
			}
			if name == "ledger exceeds total" && recorder.review.Reason != "total_below_paid" {
				t.Fatal("real value discrepancy lost its review reason")
			}
			if name == "retry after settlement" {
				provider.fail = nil
				split, err = svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
				if err != nil || !split.Verified || recorder.review != nil || base.escritas != 0 {
					t.Fatalf("settlement retry failed: %+v %v", split, err)
				}
			}
			after, err := repo.ListCartPayments(t.Context(), "cart-1")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("reconciliation changed the payment ledger")
			}
		})
	}
}
