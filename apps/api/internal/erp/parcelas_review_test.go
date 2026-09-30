package erp

import (
	"context"
	"errors"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

type verifyingInstallmentsProvider struct {
	*erpComParcelas
	checks int
}

func (p *verifyingInstallmentsProvider) OrderInstallmentsMatch(_ context.Context, _ string, installments []providers.ERPInstallment) (bool, error) {
	p.checks++
	return len(installments) == 1 && installments[0].AmountCents == 10000, nil
}

func TestEqualPaidTotalClearsReviewOnlyAfterCheckingInstallments(t *testing.T) {
	svc, repo, provider := montarParcelas(map[string]int{"ext-p1": 50})
	repo.criarCarrinho("cart-1", item("p1", 5))
	if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	pagar(t, svc, repo, "cart-1", 10000)
	recording := &reviewRecordingRepo{repoSimulado: repo, review: &PaymentReview{Reason: "installments_unverified"}}
	svc.repo = recording
	if split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1"); err != nil || split.Verified || recording.review == nil {
		t.Fatalf("equal total without installment evidence cleared review: %+v %v", split, err)
	}
	verifier := &verifyingInstallmentsProvider{erpComParcelas: provider}
	svc.collab.(*colabSimulado).erp = verifier
	split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
	if err != nil || !split.Verified || verifier.checks != 1 || provider.escritas != 0 || recording.review != nil {
		t.Fatalf("failed to verify existing schedule without rewriting: %+v %v", split, err)
	}
}

type reviewRecordingRepo struct {
	*repoSimulado
	review   *PaymentReview
	fail     error
	resolved int
}

func (r *reviewRecordingRepo) RecordERPFinancialReview(_ context.Context, _, _ string, review PaymentReview) error {
	if r.fail != nil {
		return r.fail
	}
	r.review = &review
	return nil
}

func (r *reviewRecordingRepo) ResolveERPFinancialReview(_ context.Context, _, _, _ string, _ time.Time) error {
	if r.fail != nil {
		return r.fail
	}
	r.resolved++
	r.review = nil
	return nil
}

func TestPaidOrderFinancialReviewPersistsWithoutChangingPayments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		invoiced bool
	}{
		{name: "open order"}, {name: "invoiced order", invoiced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, provider := montarParcelas(map[string]int{"ext-p1": 50})
			repo.criarCarrinho("cart-1", item("p1", 5))
			if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
				t.Fatal(err)
			}
			pagar(t, svc, repo, "cart-1", 10000)
			recording := &reviewRecordingRepo{repoSimulado: repo}
			svc.repo = recording
			provider.usarForcado, provider.totalForcado, provider.invoiced = true, 4000, tc.invoiced
			split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
			if err != nil {
				t.Fatal(err)
			}
			if recording.review == nil || recording.review.Reason != "total_below_paid" {
				t.Fatalf("missing review: %+v", recording.review)
			}
			if *recording.review.PaidCents != 10000 || *recording.review.OrderTotalCents != 4000 || split.Reescrito || provider.escritas != 0 {
				t.Fatalf("invalid observation or unsafe write: review=%+v split=%+v writes=%d", recording.review, split, provider.escritas)
			}
			ledger, err := repo.ListCartPayments(t.Context(), "cart-1")
			if err != nil || len(ledger) != 1 || ledger[0].AmountCents != 10000 {
				t.Fatalf("ledger changed: %+v %v", ledger, err)
			}
		})
	}
}

func TestFailedInstallmentRewriteRemainsVisibleUntilVerified(t *testing.T) {
	svc, repo, provider := montarParcelas(map[string]int{"ext-p1": 50})
	repo.criarCarrinho("cart-1", item("p1", 5))
	if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	pagar(t, svc, repo, "cart-1", 10000)
	recording := &reviewRecordingRepo{repoSimulado: repo}
	svc.repo = recording
	provider.usarForcado, provider.totalForcado = true, 12000
	refusal := errors.New("receivables do not match installments")
	provider.falharEscrita = refusal
	_, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
	if !errors.Is(err, refusal) || recording.review == nil || recording.review.Reason != "installments_unverified" {
		t.Fatalf("lost failure: review=%+v err=%v", recording.review, err)
	}
	provider.falharEscrita = nil
	provider.invoiced = true
	if _, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	if recording.review == nil || recording.resolved != 0 {
		t.Fatal("invoice alone cleared unverified receivables")
	}
	provider.invoiced = false
	split, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1")
	if err != nil || !split.Verified || recording.review != nil || recording.resolved != 1 {
		t.Fatalf("successful verification not recorded: split=%+v err=%v", split, err)
	}
}

func TestFinancialReviewPersistenceFailureIsReturned(t *testing.T) {
	svc, repo, provider := montarParcelas(map[string]int{"ext-p1": 50})
	repo.criarCarrinho("cart-1", item("p1", 5))
	if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
		t.Fatal(err)
	}
	pagar(t, svc, repo, "cart-1", 10000)
	failure := errors.New("review storage unavailable")
	svc.repo = &reviewRecordingRepo{repoSimulado: repo, fail: failure}
	provider.usarForcado, provider.totalForcado = true, 4000
	if _, err := svc.RecomporParcelasDoPedidoPago(t.Context(), "cart-1", "loja-1"); !errors.Is(err, failure) {
		t.Fatalf("persistence failure swallowed: %v", err)
	}
}
