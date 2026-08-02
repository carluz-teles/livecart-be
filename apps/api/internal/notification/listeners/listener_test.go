package listeners_test

// Fatia A1 — o reactor de Notification. Testes puros (sem DB): provam que os
// listeners On<Fato> reagem ao fato delegando ao ReceiptSender e propagam o erro
// de readiness (ErrReceiptNotReady) para o asynq re-tentar. A lógica de guarda +
// idempotência vive e é testada em postcheckout (receipt_test.go, gated por DB);
// aqui travamos o fio do reactor.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification/listeners"
)

// fakeReceipts implementa listeners.ReceiptSender, gravando as chamadas e
// devolvendo um erro configurável por método.
type fakeReceipts struct {
	paidCalls   []string
	refundCalls []string
	paidErr     error
	refundErr   error
}

func (f *fakeReceipts) SendPaidReceipt(_ context.Context, cartID string) error {
	f.paidCalls = append(f.paidCalls, cartID)
	return f.paidErr
}
func (f *fakeReceipts) SendRefundEmail(_ context.Context, cartID string) error {
	f.refundCalls = append(f.refundCalls, cartID)
	return f.refundErr
}

func TestOnCartPaid_DelegatesToPaidReceipt(t *testing.T) {
	fake := &fakeReceipts{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-1"); err != nil {
		t.Fatalf("OnCartPaid: unexpected error: %v", err)
	}
	if len(fake.paidCalls) != 1 || fake.paidCalls[0] != "cart-1" {
		t.Fatalf("OnCartPaid should delegate to SendPaidReceipt(cart-1), got %v", fake.paidCalls)
	}
	if len(fake.refundCalls) != 0 {
		t.Errorf("OnCartPaid must not touch the refund path, got %v", fake.refundCalls)
	}
}

// A readiness (Order/token pendente) precisa PROPAGAR para o asynq re-tentar —
// o reactor não pode engolir o erro.
func TestOnCartPaid_PropagatesReadinessErrorForRetry(t *testing.T) {
	sentinel := errors.New("order/token pending")
	fake := &fakeReceipts{paidErr: sentinel}
	l := listeners.New(fake, zap.NewNop())

	err := l.OnCartPaid(context.Background(), "cart-2")
	if !errors.Is(err, sentinel) {
		t.Fatalf("OnCartPaid should propagate the sender error (retry), got %v", err)
	}
}

func TestOnCartRefunded_DelegatesToRefundEmail(t *testing.T) {
	fake := &fakeReceipts{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartRefunded(context.Background(), "cart-3"); err != nil {
		t.Fatalf("OnCartRefunded: unexpected error: %v", err)
	}
	if len(fake.refundCalls) != 1 || fake.refundCalls[0] != "cart-3" {
		t.Fatalf("OnCartRefunded should delegate to SendRefundEmail(cart-3), got %v", fake.refundCalls)
	}
	if len(fake.paidCalls) != 0 {
		t.Errorf("OnCartRefunded must not touch the paid path, got %v", fake.paidCalls)
	}
}

func TestOnCartRefunded_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &fakeReceipts{refundErr: sentinel}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartRefunded(context.Background(), "cart-4"); !errors.Is(err, sentinel) {
		t.Fatalf("OnCartRefunded should propagate the sender error, got %v", err)
	}
}
