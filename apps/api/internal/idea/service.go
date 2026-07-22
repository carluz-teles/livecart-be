package idea

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/idea/domain"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// NotificationWriter is the dependency the idea service uses to fire in-app
// notifications. Implemented by internal/notification_inbox; injected to keep
// the modules decoupled (no import cycle).
type NotificationWriter interface {
	NotifyIdeaComment(ctx context.Context, recipientID, actorID, ideaID, commentID, excerpt string) error
	NotifyIdeaReply(ctx context.Context, recipientID, actorID, ideaID, commentID, excerpt string) error
}

type Service struct {
	repo     *Repository
	notifier NotificationWriter
	logger   *zap.Logger
}

func NewService(repo *Repository, notifier NotificationWriter, logger *zap.Logger) *Service {
	return &Service{
		repo:     repo,
		notifier: notifier,
		logger:   logger.Named("idea"),
	}
}

// Create validates the category, inserts the idea, and returns the persisted
// entity (loaded with author name and caller-relative projections).
func (s *Service) Create(ctx context.Context, in CreateIdeaInput) (*domain.Idea, error) {
	if !IsValidCategory(in.Category) {
		return nil, httpx.ErrBadRequest("categoria inválida")
	}

	id, err := s.repo.Create(ctx, in.AuthorID, in.Title, in.Description, in.Category)
	if err != nil {
		return nil, err
	}

	created, err := s.repo.GetByID(ctx, id, in.AuthorID)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// List returns a page of idea entities plus the total for pagination.
func (s *Service) List(ctx context.Context, in ListIdeasInput) ([]*domain.Idea, int, error) {
	in.Pagination.Normalize()
	return s.repo.List(ctx, in)
}

// GetDetail returns an idea with its threaded comments.
func (s *Service) GetDetail(ctx context.Context, ideaID, callerID string) (*domain.IdeaDetail, error) {
	idea, err := s.repo.GetByID(ctx, ideaID, callerID)
	if err != nil {
		return nil, err
	}
	if idea == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("ideia %s não encontrada", ideaID))
	}

	comments, err := s.repo.ListCommentsForIdea(ctx, ideaID)
	if err != nil {
		return nil, err
	}

	return domain.NewIdeaDetail(idea, domain.BuildTree(comments)), nil
}

// ToggleVote enforces the self-vote rule and returns the new vote state.
func (s *Service) ToggleVote(ctx context.Context, ideaID, userID string) (*domain.VoteResult, error) {
	authorID, err := s.repo.GetIdeaAuthor(ctx, ideaID)
	if err != nil {
		return nil, err
	}
	if authorID == "" {
		return nil, httpx.ErrNotFound(fmt.Sprintf("ideia %s não encontrada", ideaID))
	}
	if authorID == userID {
		return nil, httpx.ErrForbidden("não é possível votar na própria ideia")
	}

	voted, err := s.repo.ToggleVote(ctx, ideaID, userID)
	if err != nil {
		return nil, err
	}

	count, err := s.repo.GetVoteCount(ctx, ideaID)
	if err != nil {
		return nil, err
	}

	return domain.NewVoteResult(count, voted), nil
}

// CreateComment posts a comment (or reply) and fans out notifications to the
// idea author and (when a reply) the parent comment author. Self-notifications
// and duplicates are suppressed. Notification failures are logged but do not
// fail the request — the comment is the canonical event.
func (s *Service) CreateComment(ctx context.Context, in CreateCommentInput) (*domain.Comment, error) {
	ideaAuthorID, err := s.repo.GetIdeaAuthor(ctx, in.IdeaID)
	if err != nil {
		return nil, err
	}
	if ideaAuthorID == "" {
		return nil, httpx.ErrNotFound(fmt.Sprintf("ideia %s não encontrada", in.IdeaID))
	}

	comment, parentAuthorID, err := s.repo.CreateComment(ctx, in.IdeaID, in.AuthorID, in.Body, in.ParentCommentID)
	if err != nil {
		return nil, err
	}

	if s.notifier != nil {
		excerpt := excerptOf(in.Body, 120)

		if in.ParentCommentID == nil {
			// Top-level comment: notify the idea author (unless self-comment).
			if ideaAuthorID != in.AuthorID {
				if err := s.notifier.NotifyIdeaComment(ctx, ideaAuthorID, in.AuthorID, in.IdeaID, comment.ID(), excerpt); err != nil {
					logger.From(ctx, s.logger).Warn("failed to send idea_comment notification", zap.Error(err))
				}
			}
		} else {
			// Reply: idea author + parent comment author, deduped, no self-notif.
			recipients := dedupRecipients(in.AuthorID, ideaAuthorID, parentAuthorID)
			for _, rid := range recipients {
				if err := s.notifier.NotifyIdeaReply(ctx, rid, in.AuthorID, in.IdeaID, comment.ID(), excerpt); err != nil {
					logger.From(ctx, s.logger).Warn("failed to send idea_reply notification",
						zap.String("recipient_id", rid),
						zap.Error(err))
				}
			}
		}
	}

	return comment, nil
}

// dedupRecipients returns the set of userIDs to notify, excluding the actor and
// any duplicates. Order is preserved as given.
func dedupRecipients(actorID string, candidates ...string) []string {
	seen := map[string]struct{}{actorID: {}}
	out := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func excerptOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
