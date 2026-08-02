package listeners_test

// O reactor de Coupon. Testes puros (sem DB): provam que o listener On<Fato> reage
// aos fatos cart.paid / cart.refunded delegando ao RedemptionConfirmer e propaga o
// erro (para o asynq re-tentar). A idempotência real (ConfirmRedemption no-op se
// status!='reserved'; RefundRedemption no-op se nil / já 'refunded'; cart sem cupom
// = no-op) é garantida por construção no coupon.Service e coberta pelos testes do
// serviço (ConfirmRedemption/RefundRedemption em service.go); aqui travamos o fio do
// reactor. Espelha inventory/listeners/listener_test.go.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/coupon/listeners"
)

// fakeConfirmer implementa listeners.RedemptionConfirmer, gravando as chamadas de
// cada método e devolvendo erros configuráveis.
type fakeConfirmer struct {
	confirmCalls []string
	refundCalls  []string
	confirmErr   error
	refundErr    error
}

func (f *fakeConfirmer) ConfirmRedemption(_ context.Context, cartID string) error {
	f.confirmCalls = append(f.confirmCalls, cartID)
	return f.confirmErr
}

func (f *fakeConfirmer) RefundRedemption(_ context.Context, cartID string) error {
	f.refundCalls = append(f.refundCalls, cartID)
	return f.refundErr
}

// AC1: cart com redemption 'reserved' → OnCartPaid confirma (1×), nil.
func TestOnCartPaid_ConfirmsRedemption(t *testing.T) {
	fake := &fakeConfirmer{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-1"); err != nil {
		t.Fatalf("OnCartPaid: unexpected error: %v", err)
	}
	if len(fake.confirmCalls) != 1 || fake.confirmCalls[0] != "cart-1" {
		t.Fatalf("OnCartPaid should delegate to ConfirmRedemption(cart-1), got %v", fake.confirmCalls)
	}
	if len(fake.refundCalls) != 0 {
		t.Fatalf("OnCartPaid must not touch RefundRedemption, got %v", fake.refundCalls)
	}
}

// AC2: cart sem cupom → OnCartPaid no-op, nil. O reactor delega mesmo assim; o
// no-op é garantido pelo ConfirmRedemption do serviço (nil redemption → nil sem
// write), aqui representado pelo fake devolvendo nil.
func TestOnCartPaid_NoCoupon_NoError(t *testing.T) {
	fake := &fakeConfirmer{} // no error → serviço no-op para cart sem redemption
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-no-coupon"); err != nil {
		t.Fatalf("OnCartPaid without coupon should return nil, got %v", err)
	}
}

// AC3: ConfirmRedemption erro → OnCartPaid propaga (para o asynq re-tentar); o
// reactor não pode engolir o erro (é idempotente por construção, então re-tentar
// é seguro).
func TestOnCartPaid_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &fakeConfirmer{confirmErr: sentinel}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-2"); !errors.Is(err, sentinel) {
		t.Fatalf("OnCartPaid should propagate the confirm error (retry), got %v", err)
	}
}

// AC4: redemption já 'confirmed' → 2ª chamada nil (sem double-write). O reactor
// delega as duas vezes; a proteção contra double-write vive no serviço
// (ConfirmRedemption no-op quando status!='reserved').
func TestOnCartPaid_Idempotent_SecondCallNoError(t *testing.T) {
	fake := &fakeConfirmer{} // serviço já-confirmado → no-op → nil
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-3"); err != nil {
		t.Fatalf("OnCartPaid #1: unexpected error: %v", err)
	}
	if err := l.OnCartPaid(context.Background(), "cart-3"); err != nil {
		t.Fatalf("OnCartPaid #2 (already confirmed) should be nil, got %v", err)
	}
	if len(fake.confirmCalls) != 2 {
		t.Fatalf("both deliveries should delegate to ConfirmRedemption, got %v", fake.confirmCalls)
	}
}

// AC5: OnCartRefunded com redemption 'confirmed' → RefundRedemption chamado (1×),
// nil. A transição real 'confirmed'→'refunded' + used_count-- é do coupon.Service
// (RefundRedemption em service.go, sob row lock); aqui travamos a delegação.
func TestOnCartRefunded_RefundsRedemption(t *testing.T) {
	fake := &fakeConfirmer{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartRefunded(context.Background(), "cart-4"); err != nil {
		t.Fatalf("OnCartRefunded: unexpected error: %v", err)
	}
	if len(fake.refundCalls) != 1 || fake.refundCalls[0] != "cart-4" {
		t.Fatalf("OnCartRefunded should delegate to RefundRedemption(cart-4), got %v", fake.refundCalls)
	}
	if len(fake.confirmCalls) != 0 {
		t.Fatalf("OnCartRefunded must not touch ConfirmRedemption, got %v", fake.confirmCalls)
	}
}

// AC6: OnCartRefunded sem cupom → nil sem writes. O reactor delega; o no-op é do
// RefundRedemption (nil redemption → nil), representado pelo fake devolvendo nil.
func TestOnCartRefunded_NoCoupon_NoError(t *testing.T) {
	fake := &fakeConfirmer{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartRefunded(context.Background(), "cart-no-coupon"); err != nil {
		t.Fatalf("OnCartRefunded without coupon should return nil, got %v", err)
	}
}

// OnCartRefunded também propaga o erro do refund (→ asynq retry).
func TestOnCartRefunded_PropagatesError(t *testing.T) {
	sentinel := errors.New("kaboom")
	fake := &fakeConfirmer{refundErr: sentinel}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartRefunded(context.Background(), "cart-5"); !errors.Is(err, sentinel) {
		t.Fatalf("OnCartRefunded should propagate the refund error (retry), got %v", err)
	}
}
