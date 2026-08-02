package notification_inbox

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("notification_inbox"),
	}
}

func (s *Service) List(ctx context.Context, in ListNotificationsInput) ([]NotificationRow, int, int, error) {
	in.Pagination.Normalize()
	rows, total, err := s.repo.List(ctx, in)
	if err != nil {
		return nil, 0, 0, err
	}
	unread, err := s.repo.UnreadCount(ctx, in.UserID)
	if err != nil {
		return nil, 0, 0, err
	}
	return rows, total, unread, nil
}

func (s *Service) UnreadCount(ctx context.Context, userID string) (int, error) {
	return s.repo.UnreadCount(ctx, userID)
}

func (s *Service) MarkRead(ctx context.Context, userID, notifID string) error {
	return s.repo.MarkRead(ctx, userID, notifID)
}

func (s *Service) MarkAllRead(ctx context.Context, userID string) error {
	return s.repo.MarkAllRead(ctx, userID)
}

// =============================================================================
// Writer adapter — implements idea.NotificationWriter
// =============================================================================

// Writer is the dependency exposed to other modules (e.g. internal/idea) so they
// can fan out notifications without importing the inbox package's request/
// response types. Failures are returned to the caller, who decides whether to
// fail the request or just log.
type Writer struct {
	repo *Repository
}

func NewWriter(repo *Repository) *Writer { return &Writer{repo: repo} }

func (w *Writer) NotifyIdeaComment(ctx context.Context, recipientID, actorID, ideaID, commentID, excerpt string) error {
	payload, _ := json.Marshal(map[string]string{"excerpt": excerpt})
	return w.repo.InsertIdeaNotification(ctx, TypeIdeaComment, recipientID, ptrOrNil(actorID), ideaID, ptrOrNil(commentID), payload)
}

func (w *Writer) NotifyIdeaReply(ctx context.Context, recipientID, actorID, ideaID, commentID, excerpt string) error {
	payload, _ := json.Marshal(map[string]string{"excerpt": excerpt})
	return w.repo.InsertIdeaNotification(ctx, TypeIdeaReply, recipientID, ptrOrNil(actorID), ideaID, ptrOrNil(commentID), payload)
}

// NotifyOrderCancellationReverted avisa TODOS os membros ativos da loja que um
// pedido que eles cancelaram acabou sendo pago — o pagamento entrou antes ou
// durante o cancelamento e venceu, então o pedido seguiu o fluxo normal (ERP,
// métricas). Não há nada a fazer no sistema; o aviso existe para o lojista não
// descobrir por acidente que vendeu algo que julgava cancelado. Se ele quiser
// devolver o dinheiro, o estorno é por fora (ainda não temos esse fluxo).
//
// Idempotente pelo índice único parcial (recipient_id, cart_id, type): o
// reactor roda sob entrega at-least-once e um retry não repete o aviso.
func (w *Writer) NotifyOrderCancellationReverted(
	ctx context.Context, storeID, cartID string, shortID int, platformHandle string,
) error {
	payload, _ := json.Marshal(map[string]any{
		"short_id":        shortID,
		"platform_handle": platformHandle,
	})
	return w.repo.InsertStoreOrderNotification(ctx, TypeOrderCancellationReverted, storeID, cartID, payload)
}

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
