package erp

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/ratelimit"
)

type statusBudgetProvider struct {
	*erpSimulado
	limiter *ratelimit.Tiny
}

func (p *statusBudgetProvider) GetOrderSituacao(ctx context.Context, id string) (int, error) {
	if err := p.limiter.WaitRequest(ctx, http.MethodGet); err != nil {
		return 0, &providers.RequestNotSentError{Err: err}
	}
	return p.erpSimulado.GetOrderSituacao(ctx, id)
}

type statusContextRepo struct {
	*repoSimulado
	observedContext context.Context
}

func (r *statusContextRepo) RecordOrderStatus(ctx context.Context, obs ERPOrderStatusObservation) (ERPOrderStatusTransition, bool, error) {
	r.observedContext = ctx
	return r.repoSimulado.RecordOrderStatus(ctx, obs)
}

func TestOrderStatusSweepYieldsQuotaOnlyForPeriodicGET(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, repo, provider, collab := montar(map[string]int{"ext-p1": 5})
		repo.criarCarrinho("cart-1", item("p1", 1))
		if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
			t.Fatal(err)
		}
		orderID := repo.carrinho("cart-1").externalOrderID
		if err := provider.SetOrderSituacao(t.Context(), orderID, providers.SituacaoEnviada); err != nil {
			t.Fatal(err)
		}
		statusRepo := &statusContextRepo{repoSimulado: repo}
		svc.SetOrderStatusRepository(statusRepo)
		limiter := ratelimit.NewManager(zap.NewNop()).GetOrCreateTiny(t.Name())
		collab.erp = &statusBudgetProvider{erpSimulado: provider, limiter: limiter}
		interactive := ratelimit.WithTinyInteractiveRead(t.Context())
		// The operator just used a slot and retains its existing priority lease.
		if err := limiter.WaitRequest(interactive, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			svc.RunERPOrderStatusSweep(t.Context(), time.Hour, 100)
			close(done)
		}()
		time.Sleep(3 * time.Second)
		short, cancel := context.WithTimeout(interactive, time.Second)
		defer cancel()
		if err := limiter.WaitRequest(short, http.MethodGet); err != nil {
			t.Fatalf("periodic status GET consumed the operator's slot: %v", err)
		}
		// Live/payment reads retain normal priority and can take an eligible
		// slot while the periodic reader is yielding to the operator's lease.
		time.Sleep(2500 * time.Millisecond)
		operational, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if _, err := collab.erp.GetOrderSituacao(operational, orderID); err != nil {
			t.Fatalf("operational status read inherited maintenance priority: %v", err)
		}
		<-done
		if repo.carrinho("cart-1").statusERP != string(providers.ERPOrderStatusEnviado) {
			t.Fatal("periodic status sweep failed to converge")
		}
		// Observation/payment/reflection must receive the original context,
		// not the deferrable context used for the GET.
		if err := limiter.WaitRequest(interactive, http.MethodGet); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond)
		observation, stopObservation := context.WithTimeout(statusRepo.observedContext, time.Second)
		defer stopObservation()
		if err := limiter.WaitRequest(observation, http.MethodGet); err != nil {
			t.Fatalf("maintenance priority leaked into observation/payment: %v", err)
		}
	})
}
