package erp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

type cancellationResultStub struct {
	CartReopener
	cancelled bool
	err       error
	calls     int
}

func (s *cancellationResultStub) CancelCartFromERP(context.Context, string, string) (bool, error) {
	s.calls++
	return s.cancelled, s.err
}

func TestObserveCancellationDistinguishesFinalisationFromRefundDecision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
		err       error
		level     zapcore.Level
		message   string
	}{
		{
			name: "finalisation in progress",
			err:  fmt.Errorf("cancelling cart from ERP: %w", ErrCartBusy), level: zap.InfoLevel,
			message: "ERP cancellation deferred; cart finalisation in progress",
		},
		{
			name: "paid cancellation still needs attention", level: zap.WarnLevel,
			message: "the order was cancelled in the ERP but the cart is paid — the refund is a human decision",
		},
		{
			name: "actual cancellation", cancelled: true, level: zap.InfoLevel,
			message: "cart cancelled following the ERP order",
		},
		{
			name: "database failure stays visible", err: errors.New("database unavailable"), level: zap.ErrorLevel,
			message: "the order was cancelled in the ERP and the cart could not follow",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, _ := montarParcelas(map[string]int{"ext-p1": 20})
			repo.criarCarrinho("cart-1", item("p1", 1))
			ctx := logger.WithStore(t.Context(), "loja-1", "store-test")
			if err := svc.EnsureERPOrderForCart(ctx, "cart-1", "loja-1"); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zap.DebugLevel)
			svc.logger = zap.New(core)
			reopener := &cancellationResultStub{cancelled: tc.cancelled, err: tc.err}
			svc.SetCartReopener(reopener)
			orderID := repo.carrinho("cart-1").externalOrderID
			if err := svc.ObserveOrderStatus(
				ctx, "loja-1", orderID, "1", providers.ERPOrderStatusCancelado, StatusSourceWebhook, nil,
			); err != nil {
				t.Fatal(err)
			}
			entries := logs.FilterMessage(tc.message).All()
			if reopener.calls != 1 || len(entries) != 1 || entries[0].Level != tc.level {
				t.Fatalf("wrong cancellation classification: calls=%d logs=%+v", reopener.calls, logs.All())
			}
			fields := entries[0].ContextMap()
			if fields["cart_id"] != "cart-1" || fields["external_order_id"] != orderID {
				t.Fatalf("missing order correlation: %v", fields)
			}
			if tc.level == zap.InfoLevel && logs.FilterLevelExact(zap.WarnLevel).Len() != 0 {
				t.Fatal("expected cancellation emitted a false warning")
			}
		})
	}
}
