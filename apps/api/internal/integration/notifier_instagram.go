package integration

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/live"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/logger"
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

// NotifyEventCheckout envia a mensagem de "a campanha encerrou, o prazo para
// pagar começou" (RN-28, gatilho 3).
//
// O texto era hardcoded aqui — "Sua compra na live está pronta 🎉" — fora do
// subsistema de Comunicações: o lojista não podia editar, nada consultava as
// settings e nenhuma linha ia para notification_logs. Justamente a mensagem que
// o produto chama de "maior impacto em conversão".
//
// Agora passa pelo notification.Service, que resolve template, registra e —
// quando a janela de 7 dias do private reply já venceu — devolve não-entrega
// COM MOTIVO em vez de tentar um DM que o Instagram recusa (erro 2534022) e
// registrar isso como falha genérica.
func (n *InstagramNotifier) NotifyEventCheckout(ctx context.Context, params NotifyEventCheckoutParams) (NotifyEventCheckoutResult, error) {
	if n.svc.notificationService == nil {
		return NotifyEventCheckoutResult{}, fmt.Errorf("notification service not configured")
	}

	shouldNotify, err := n.svc.notificationService.ShouldNotify(ctx, params.StoreID, notification.TypeEventDeadlineStarted, false)
	if err != nil {
		return NotifyEventCheckoutResult{}, fmt.Errorf("checking notification settings: %w", err)
	}
	if !shouldNotify {
		// Template desligado pelo lojista. Não é não-entrega: ele escolheu não
		// mandar. Reportar como entregue evita que a decisão dele apareça na
		// lista de "não avisados", que é para quem o Instagram barrou.
		return NotifyEventCheckoutResult{Delivered: true}, nil
	}

	frontendURL := config.FrontendURL.StringOr("http://localhost:3000")
	vars := notification.TemplateVariables{
		Handle:     "@" + params.PlatformHandle,
		TotalItens: params.TotalItems,
		Total:      notification.FormatCurrency(params.TotalValue),
		TotalCents: params.TotalValue,
		Link:       fmt.Sprintf("%s/cart/%s", frontendURL, params.CartToken),
		LiveTitulo: params.EventTitle,
	}
	if storeInfo, err := n.svc.repo.GetStoreInfo(ctx, params.StoreID); err == nil {
		vars.Loja = storeInfo.Name
	}
	if params.DeadlineAt != nil {
		vars.PrazoFinal = live.FormatBRT(*params.DeadlineAt)
	}

	commentAt := time.Time{}
	if params.CommentCreatedAt != nil {
		commentAt = *params.CommentCreatedAt
	}

	result, err := n.svc.notificationService.Send(ctx, notification.SendInput{
		StoreID:           params.StoreID,
		EventID:           params.EventID,
		CartID:            params.CartID,
		CartToken:         params.CartToken,
		PlatformUserID:    params.PlatformUserID,
		PlatformHandle:    params.PlatformHandle,
		PlatformCommentID: params.CommentID,
		NotificationType:  notification.TypeEventDeadlineStarted,
		Variables:         vars,
		CommentCreatedAt:  commentAt,
	})
	if err != nil {
		return NotifyEventCheckoutResult{}, err
	}

	switch result.Status {
	case notification.StatusSent:
		return NotifyEventCheckoutResult{Delivered: true}, nil
	case notification.StatusSkipped:
		return NotifyEventCheckoutResult{Delivered: true}, nil
	default:
		logger.From(ctx, n.logger).Info("event checkout message not delivered",
			zap.String("store_id", params.StoreID),
			zap.String("event_id", params.EventID),
			zap.String("cart_id", params.CartID),
			zap.String("platform_user_id", params.PlatformUserID),
			zap.String("reason", string(result.Reason)),
		)
		return NotifyEventCheckoutResult{
			Delivered:  false,
			Reason:     string(result.Reason),
			ReasonText: notification.UndeliverableReasonText(result.Reason),
		}, nil
	}
}
