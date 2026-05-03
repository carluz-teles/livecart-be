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

func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
