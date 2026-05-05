package coupon

import "context"

// RedemptionSyncer is the lifecycle adapter the integration package consumes
// to flip redemptions on payment webhooks. Lives in the coupon package so
// the integration package keeps no compile-time dep on coupon internals; it
// only sees the interface declared on its side and a concrete value injected
// at app boot.
type RedemptionSyncer struct {
	svc *Service
}

func NewRedemptionSyncer(svc *Service) *RedemptionSyncer {
	return &RedemptionSyncer{svc: svc}
}

func (s *RedemptionSyncer) OnCartPaid(ctx context.Context, cartID string) error {
	return s.svc.ConfirmRedemption(ctx, cartID)
}

func (s *RedemptionSyncer) OnCartRefunded(ctx context.Context, cartID string) error {
	return s.svc.RefundRedemption(ctx, cartID)
}

// OnShippingChanged satisfies checkout.CouponLifecycle. The same adapter
// covers both interfaces because RedemptionSyncer carries everything the
// coupon service needs.
func (s *RedemptionSyncer) OnShippingChanged(ctx context.Context, cartID string) error {
	return s.svc.ReevaluateOnShippingChange(ctx, cartID)
}

// OnCartMutated satisfies checkout.CouponLifecycle. Routes to the coupon
// service so the discount/redemption stays consistent with the new cart
// subtotal after item add / update / remove.
func (s *RedemptionSyncer) OnCartMutated(ctx context.Context, cartID string) error {
	return s.svc.ReevaluateOnCartMutation(ctx, cartID)
}
