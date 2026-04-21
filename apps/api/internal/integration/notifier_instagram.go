package integration

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/config"
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

// NotifyEventCheckout sends a checkout link DM to the buyer.
func (n *InstagramNotifier) NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) error {
	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")
	text := fmt.Sprintf(
		"Olá @%s! Sua compra na live está pronta 🎉\n%d itens • R$ %.2f\nFinalize aqui: %s/cart/%s",
		params.PlatformHandle,
		params.TotalItems,
		float64(params.TotalValue)/100,
		frontendURL,
		params.CartToken,
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
