package events

import (
	"context"
	"testing"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"
)

// TestRegisterHandlers_LeavesCompositionRootFactsFree guards the Fatia E1 wiring
// contract: cart.cancelled (and cart.refunded) MUST be registered by the
// composition root with a domain reactor, NOT by the default logEvent registry.
// asynq's ServeMux panics on a duplicate pattern, so if RegisterHandlers wrongly
// re-added one of these, the composition-root registration below would panic —
// exactly the "silently not registered / double registered" regression AC4 forbids.
func TestRegisterHandlers_LeavesCompositionRootFactsFree(t *testing.T) {
	mux := asynq.NewServeMux()
	RegisterHandlers(mux, zap.NewNop())

	noop := func(context.Context, *asynq.Task) error { return nil }

	for _, name := range []Name{CartCancelled, CartRefunded} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RegisterHandlers already claimed %q (%v); it must be left for the composition root", name, r)
				}
			}()
			// A fresh mux per fact: proves each is unclaimed by RegisterHandlers.
			m := asynq.NewServeMux()
			RegisterHandlers(m, zap.NewNop())
			m.HandleFunc(string(name), noop)
		}()
	}
}
