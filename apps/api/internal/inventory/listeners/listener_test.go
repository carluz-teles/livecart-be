package listeners_test

// Fatia A2 — o reactor de Inventory. Testes puros (sem DB): provam que o listener
// On<Fato> reage ao fato cart.paid delegando ao WaitlistFulfiller e propaga o erro
// (para o asynq re-tentar). A semântica do UPDATE ('notified'→'fulfilled', 'waiting'
// intocado) e a não-reemissão na 2ª chamada são garantidas por construção da query
// canônica reusada (`WHERE cart_id=$1 AND status='notified'` → 0 rows numa
// redelivery → 0 emissões) e documentadas em repository.go; aqui travamos o fio do
// reactor. Espelha notification/listeners/listener_test.go.

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"livecart/apps/api/internal/inventory/listeners"
)

// fakeFulfiller implementa listeners.WaitlistFulfiller, gravando as chamadas e
// devolvendo um erro configurável.
type fakeFulfiller struct {
	calls []string
	err   error
}

func (f *fakeFulfiller) MarkWaitlistFulfilledByCart(_ context.Context, cartID string) error {
	f.calls = append(f.calls, cartID)
	return f.err
}

func TestOnCartPaid_FulfillsWaitlist(t *testing.T) {
	fake := &fakeFulfiller{}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-1"); err != nil {
		t.Fatalf("OnCartPaid: unexpected error: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "cart-1" {
		t.Fatalf("OnCartPaid should delegate to MarkWaitlistFulfilledByCart(cart-1), got %v", fake.calls)
	}
}

// O erro do fulfill precisa PROPAGAR para o asynq re-tentar — o reactor não pode
// engolir o erro (é idempotente por construção, então re-tentar é seguro).
func TestOnCartPaid_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &fakeFulfiller{err: sentinel}
	l := listeners.New(fake, zap.NewNop())

	if err := l.OnCartPaid(context.Background(), "cart-2"); !errors.Is(err, sentinel) {
		t.Fatalf("OnCartPaid should propagate the fulfiller error (retry), got %v", err)
	}
}
