package events

import (
	"context"
	"testing"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

// TestRegisterHandlers_LeavesCompositionRootFactsFree guards the wiring contract:
// cart.cancelled/cart.refunded (Fatia E1) and order.paid/order.refunded (Fatia
// 11b-2) MUST be registered by the composition root with a domain reactor, NOT by
// the default logEvent registry. asynq's ServeMux panics on a duplicate pattern, so
// if RegisterHandlers wrongly re-added one of these, the composition-root
// registration below would panic — exactly the "silently not registered / double
// registered" regression AC4 forbids. order.paid/order.refunded carried 11a no-op
// handlers here that 11b-2 removed; this guards against them coming back.
func TestRegisterHandlers_LeavesCompositionRootFactsFree(t *testing.T) {
	mux := asynq.NewServeMux()
	RegisterHandlers(mux, zap.NewNop(), nil)

	noop := func(context.Context, *asynq.Task) error { return nil }

	// event.ended entrou nesta lista na RN-32: ele tinha logEvent aqui e passou a
	// ser o gancho que arma a morte da fila não atendida. Se alguém devolver o
	// logEvent para o registro, o servidor inteiro passa a entrar em pânico no
	// boot por padrão duplicado — este teste é o que avisa antes.
	for _, name := range []Name{CartCancelled, CartRefunded, OrderPaid, OrderRefunded, EventEventEnded, EventWaitlistClose} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RegisterHandlers already claimed %q (%v); it must be left for the composition root", name, r)
				}
			}()
			// A fresh mux per fact: proves each is unclaimed by RegisterHandlers.
			m := asynq.NewServeMux()
			RegisterHandlers(m, zap.NewNop(), nil)
			m.HandleFunc(string(name), noop)
		}()
	}
}
