package integration

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// InstagramNotifier implements Notifier by sending DMs through the Instagram
// integration of the target store. Falls back to no-op for notification types
// that are not implemented yet.
type InstagramNotifier struct {
	svc    *Service
	logger *zap.Logger
}

// NewInstagramNotifier creates a Notifier backed by the Instagram provider.
func NewInstagramNotifier(svc *Service, logger *zap.Logger) *InstagramNotifier {
	return &InstagramNotifier{
		svc:    svc,
		logger: logger.Named("instagram-notifier"),
	}
}

// NotifyWaitlistAvailable is not implemented yet.
func (n *InstagramNotifier) NotifyWaitlistAvailable(_ context.Context, _ NotifyWaitlistParams) error {
	return nil
}

// NotifyCartExpiring is not implemented yet.
func (n *InstagramNotifier) NotifyCartExpiring(_ context.Context, _ NotifyCartExpiringParams) error {
	return nil
}

// NotifyEventCheckout sends a (mocked) checkout link DM to the buyer.
func (n *InstagramNotifier) NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) error {
	text := fmt.Sprintf(
		"Olá @%s! Sua compra na live está pronta 🎉\n%d itens • R$ %.2f\nFinalize aqui: https://checkout.livecart.app/c/%s (link mock)",
		params.PlatformHandle,
		params.TotalItems,
		float64(params.TotalValue)/100,
		params.CartID,
	)

	if err := n.svc.SendInstagramDM(ctx, params.StoreID, params.PlatformUserID, text); err != nil {
		n.logger.Warn("notify event checkout failed",
			zap.String("store_id", params.StoreID),
			zap.String("event_id", params.EventID),
			zap.String("cart_id", params.CartID),
			zap.String("platform_user_id", params.PlatformUserID),
			zap.Error(err),
		)
		return err
	}
	return nil
}
