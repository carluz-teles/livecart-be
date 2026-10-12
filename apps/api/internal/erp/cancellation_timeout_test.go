package erp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

type cancellationReadTimeoutERP struct {
	*erpSimulado
	blockRead bool
}

func (p *cancellationReadTimeoutERP) SetOrderSituacao(ctx context.Context, id string, situation int) error {
	if err := p.erpSimulado.SetOrderSituacao(ctx, id, situation); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func (p *cancellationReadTimeoutERP) GetOrderSituacao(ctx context.Context, id string) (int, error) {
	if p.blockRead {
		<-ctx.Done()
		return providers.SituacaoDesconhecida, ctx.Err()
	}
	return p.erpSimulado.GetOrderSituacao(ctx, id)
}

func TestCancellationCompensationSurvivesVerificationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, repo, provider, collab := montar(map[string]int{"ext-p1": 2})
		repo.criarCarrinho("cart-1", item("p1", 1))
		if err := svc.EnsureERPOrderForCart(t.Context(), "cart-1", "loja-1"); err != nil {
			t.Fatal(err)
		}
		repo.expirarCarrinho("cart-1")
		collab.erp = &cancellationReadTimeoutERP{erpSimulado: provider, blockRead: true}
		start := time.Now()
		err := svc.OnCartExpired(t.Context(), "cart-1", "loja-1")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 20*time.Second {
			t.Fatalf("verification did not exhaust its deadline: elapsed=%s err=%v", time.Since(start), err)
		}
		if state := repo.carrinho("cart-1").state; state != OrderStateOpen {
			t.Fatalf("exhausted read budget stranded cancellation claim: %s", state)
		}
		// The webhook arrives after compensation; the next delivery must GET
		// the already cancelled sale and converge without a second PUT.
		repo.mu.Lock()
		repo.carrinhos["cart-1"].statusERP = "cancelado"
		repo.carrinhos["cart-1"].expiryProtected = true
		repo.mu.Unlock()
		collab.erp = provider
		if err := svc.OnCartExpired(t.Context(), "cart-1", "loja-1"); err != nil {
			t.Fatal(err)
		}
		if repo.carrinho("cart-1").state != OrderStateCancelled || provider.situacoes != 1 {
			t.Fatalf("retry failed to converge without writing: state=%s PUTs=%d", repo.carrinho("cart-1").state, provider.situacoes)
		}
	})
}
