package idea

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
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

func (s *Service) Create(ctx context.Context, authorID string, req CreateIdeaRequest) (*IdeaListItem, error) {
	if !IsValidCategory(req.Category) {
		return nil, httpx.ErrBadRequest("categoria inválida")
	}

	created, err := s.repo.Create(ctx, authorID, req.Title, req.Description, req.Category)
	if err != nil {
		return nil, err
	}

	out, err := s.repo.GetByID(ctx, created.ID, authorID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) List(ctx context.Context, in ListIdeasInput) (ListIdeasOutput, error) {
	in.Pagination.Normalize()
	items, total, err := s.repo.List(ctx, in)
	if err != nil {
		return ListIdeasOutput{}, err
	}
	return ListIdeasOutput{
		Items:      items,
		Total:      total,
		Pagination: in.Pagination,
	}, nil
}

func (s *Service) GetDetail(ctx context.Context, ideaID, callerID string) (*IdeaDetail, error) {
	it, err := s.repo.GetByID(ctx, ideaID, callerID)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("ideia %s não encontrada", ideaID))
	}

	rows, err := s.repo.ListCommentsForIdea(ctx, ideaID)
	if err != nil {
		return nil, err
	}

	return &IdeaDetail{
		IdeaListItem: *it,
		Comments:     buildCommentTree(rows),
	}, nil
}

// buildCommentTree assembles a parent→replies tree from the flat, time-ordered
// comment list. O(n) — single pass building a map then linking.
func buildCommentTree(rows []CommentRow) []CommentNode {
	byID := make(map[string]*CommentNode, len(rows))
	for _, r := range rows {
		byID[r.ID] = &CommentNode{
			ID:         r.ID,
			Body:       r.Body,
			AuthorID:   r.AuthorID,
			AuthorName: r.AuthorName,
			CreatedAt:  r.CreatedAt,
			Replies:    []CommentNode{},
		}
	}

	var roots []CommentNode
	for _, r := range rows {
		node := byID[r.ID]
		if r.ParentCommentID == nil {
			roots = append(roots, *node)
			continue
		}
		parent, ok := byID[*r.ParentCommentID]
		if !ok {
			// Parent missing (shouldn't happen with the FK) — promote to root.
			roots = append(roots, *node)
			continue
		}
		parent.Replies = append(parent.Replies, *node)
	}

	// The slice copy on append loses the linkage between roots and their
	// nested updates, so re-walk from byID to materialize the final tree.
	resolved := make([]CommentNode, 0, len(roots))
	for _, r := range rows {
		if r.ParentCommentID != nil {
			continue
		}
		resolved = append(resolved, materialize(byID[r.ID], byID, rows))
	}
	return resolved
}

// materialize rebuilds a node with its full replies tree by walking children
// from the flat row list (parent_comment_id == node.ID).
func materialize(node *CommentNode, byID map[string]*CommentNode, rows []CommentRow) CommentNode {
	out := *node
	out.Replies = []CommentNode{}
	for _, r := range rows {
		if r.ParentCommentID == nil || *r.ParentCommentID != node.ID {
			continue
		}
		out.Replies = append(out.Replies, materialize(byID[r.ID], byID, rows))
	}
	return out
}

// ToggleVote enforces the self-vote rule and returns the new vote state.
func (s *Service) ToggleVote(ctx context.Context, ideaID, userID string) (*ToggleVoteResponse, error) {
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

	return &ToggleVoteResponse{VoteCount: count, VotedByMe: voted}, nil
}

// CreateComment posts a comment (or reply) and fans out notifications to the
// idea author and (when a reply) the parent comment author. Self-notifications
// and duplicates are suppressed. Notification failures are logged but do not
// fail the request — the comment is the canonical event.
func (s *Service) CreateComment(ctx context.Context, ideaID, authorID string, req CreateCommentRequest) (*CommentNodeResponse, error) {
	ideaAuthorID, err := s.repo.GetIdeaAuthor(ctx, ideaID)
	if err != nil {
		return nil, err
	}
	if ideaAuthorID == "" {
		return nil, httpx.ErrNotFound(fmt.Sprintf("ideia %s não encontrada", ideaID))
	}

	row, parentAuthorID, err := s.repo.CreateComment(ctx, ideaID, authorID, req.Body, req.ParentCommentID)
	if err != nil {
		return nil, err
	}

	if s.notifier != nil {
		excerpt := excerptOf(req.Body, 120)

		if req.ParentCommentID == nil {
			// Top-level comment: notify the idea author (unless self-comment).
			if ideaAuthorID != authorID {
				if err := s.notifier.NotifyIdeaComment(ctx, ideaAuthorID, authorID, ideaID, row.ID, excerpt); err != nil {
					s.logger.Warn("failed to send idea_comment notification", zap.Error(err))
				}
			}
		} else {
			// Reply: idea author + parent comment author, deduped, no self-notif.
			recipients := dedupRecipients(authorID, ideaAuthorID, parentAuthorID)
			for _, rid := range recipients {
				if err := s.notifier.NotifyIdeaReply(ctx, rid, authorID, ideaID, row.ID, excerpt); err != nil {
					s.logger.Warn("failed to send idea_reply notification",
						zap.String("recipient_id", rid),
						zap.Error(err))
				}
			}
		}
	}

	return &CommentNodeResponse{
		ID:         row.ID,
		Body:       row.Body,
		AuthorID:   row.AuthorID,
		AuthorName: row.AuthorName,
		CreatedAt:  row.CreatedAt,
		Replies:    []CommentNodeResponse{},
	}, nil
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
