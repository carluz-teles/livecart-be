package idea

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/internal/idea/domain"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// =============================================================================
// Ideas
// =============================================================================

// Create inserts a new idea and returns its id. The full entity (with
// author name and caller-relative projections) is loaded by GetByID afterwards.
func (r *Repository) Create(ctx context.Context, authorID, title, description, category string) (string, error) {
	const q = `
		INSERT INTO ideas (title, description, category, author_user_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`
	var id string
	if err := r.db.QueryRow(ctx, q, title, description, category, authorID).Scan(&id); err != nil {
		return "", fmt.Errorf("creating idea: %w", err)
	}
	return id, nil
}

// GetByID loads an idea as a domain entity, resolving the author name and the
// caller-relative votedByMe flag. Returns nil when not found.
func (r *Repository) GetByID(ctx context.Context, ideaID, callerID string) (*domain.Idea, error) {
	const q = `
		SELECT i.id, i.number, i.title, i.description, i.category, i.status,
		       i.author_user_id, COALESCE(u.name, u.email) AS author_name,
		       i.vote_count, i.comment_count,
		       EXISTS(SELECT 1 FROM idea_votes v WHERE v.idea_id = i.id AND v.user_id = $2) AS voted_by_me,
		       i.created_at
		FROM ideas i
		JOIN users u ON u.id = i.author_user_id
		WHERE i.id = $1
	`
	var (
		id, title, description, category, status, authorID, authorName string
		number                                                         int64
		voteCount, commentCount                                        int
		votedByMe                                                      bool
		createdAt                                                      time.Time
	)
	err := r.db.QueryRow(ctx, q, ideaID, callerID).Scan(
		&id, &number, &title, &description, &category, &status,
		&authorID, &authorName, &voteCount, &commentCount, &votedByMe, &createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting idea: %w", err)
	}
	return domain.Reconstruct(
		id, number, title, description, category, status,
		authorID, authorName, voteCount, commentCount, votedByMe, createdAt,
	), nil
}

// List builds a dynamic query covering the feed filters: tab (status/scope),
// category, full-text search (or numeric number lookup), sort, pagination.
//
// The COUNT and LIST queries share the same WHERE filters but COUNT does not
// need the caller's userID (only LIST does, for the votedByMe EXISTS subquery).
// We collect filters as `(template, value)` pairs with a `?` marker, then
// render the placeholders against a starting index — that lets us reuse the
// same filter set with different leading args per query.
func (r *Repository) List(ctx context.Context, in ListIdeasInput) ([]*domain.Idea, int, error) {
	type filter struct {
		template string
		value    any
	}
	var filters []filter

	switch in.Tab {
	case TabMine:
		filters = append(filters, filter{"i.author_user_id = ?", in.UserID})
	case TabUnderStudy:
		filters = append(filters, filter{"i.status = ?", StatusUnderStudy})
	case TabCompleted:
		filters = append(filters, filter{"i.status = ?", StatusCompleted})
	}

	if in.Category != "" {
		filters = append(filters, filter{"i.category = ?", in.Category})
	}

	if in.Search != "" {
		raw := strings.TrimSpace(in.Search)
		raw = strings.TrimPrefix(raw, "#")
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filters = append(filters, filter{"i.number = ?", n})
		} else {
			filters = append(filters, filter{
				"to_tsvector('portuguese', i.title || ' ' || i.description) @@ plainto_tsquery('portuguese', ?)",
				in.Search,
			})
		}
	}

	renderWhere := func(startIdx int) (string, []any) {
		if len(filters) == 0 {
			return "", nil
		}
		parts := make([]string, len(filters))
		args := make([]any, 0, len(filters))
		for i, f := range filters {
			parts[i] = strings.Replace(f.template, "?", fmt.Sprintf("$%d", startIdx+i), 1)
			args = append(args, f.value)
		}
		return " WHERE " + strings.Join(parts, " AND "), args
	}

	countWhere, countArgs := renderWhere(1)
	var total int
	if err := r.db.QueryRow(ctx, "SELECT COUNT(*) FROM ideas i"+countWhere, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting ideas: %w", err)
	}

	orderBy := "i.vote_count DESC, i.created_at DESC"
	if in.Sort == SortNew || in.Tab == TabNew {
		orderBy = "i.created_at DESC"
	}

	limit := in.Pagination.Limit
	offset := in.Pagination.Offset()

	// LIST query: $1 is the caller's userID (used by the votedByMe subquery);
	// filters start at $2.
	listWhere, listFilterArgs := renderWhere(2)
	listArgs := append([]any{in.UserID}, listFilterArgs...)

	listQ := fmt.Sprintf(`
		SELECT i.id, i.number, i.title, i.description, i.category, i.status,
		       i.author_user_id, COALESCE(u.name, u.email) AS author_name,
		       i.vote_count, i.comment_count,
		       EXISTS(SELECT 1 FROM idea_votes v WHERE v.idea_id = i.id AND v.user_id = $1) AS voted_by_me,
		       i.created_at
		FROM ideas i
		JOIN users u ON u.id = i.author_user_id
		%s
		ORDER BY %s
		LIMIT %d OFFSET %d
	`, listWhere, orderBy, limit, offset)

	rows, err := r.db.Query(ctx, listQ, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing ideas: %w", err)
	}
	defer rows.Close()

	out := make([]*domain.Idea, 0, limit)
	for rows.Next() {
		var (
			id, title, description, category, status, authorID, authorName string
			number                                                         int64
			voteCount, commentCount                                        int
			votedByMe                                                      bool
			createdAt                                                      time.Time
		)
		if err := rows.Scan(
			&id, &number, &title, &description, &category, &status,
			&authorID, &authorName, &voteCount, &commentCount, &votedByMe, &createdAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning idea row: %w", err)
		}
		out = append(out, domain.Reconstruct(
			id, number, title, description, category, status,
			authorID, authorName, voteCount, commentCount, votedByMe, createdAt,
		))
	}
	return out, total, rows.Err()
}

// =============================================================================
// Votes
// =============================================================================

// ToggleVote inserts the vote if missing, deletes it if present. Returns the
// new state (voted) so the caller can build the response without a re-read.
// vote_count is updated by the trg_idea_votes_count trigger.
func (r *Repository) ToggleVote(ctx context.Context, ideaID, userID string) (voted bool, err error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM idea_votes WHERE idea_id = $1 AND user_id = $2)`,
		ideaID, userID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking vote: %w", err)
	}

	if exists {
		if _, err := tx.Exec(ctx,
			`DELETE FROM idea_votes WHERE idea_id = $1 AND user_id = $2`, ideaID, userID,
		); err != nil {
			return false, fmt.Errorf("deleting vote: %w", err)
		}
		voted = false
	} else {
		if _, err := tx.Exec(ctx,
			`INSERT INTO idea_votes (idea_id, user_id) VALUES ($1, $2)`, ideaID, userID,
		); err != nil {
			return false, fmt.Errorf("inserting vote: %w", err)
		}
		voted = true
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit vote tx: %w", err)
	}
	return voted, nil
}

func (r *Repository) GetVoteCount(ctx context.Context, ideaID string) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT vote_count FROM ideas WHERE id = $1`, ideaID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("getting vote count: %w", err)
	}
	return n, nil
}

// =============================================================================
// Comments
// =============================================================================

// CreateComment inserts the comment and returns it as a domain entity together
// with the parent comment's author_user_id (or "" when top-level). The service
// uses both to decide which notifications to fire. Counter is updated by the
// trigger.
func (r *Repository) CreateComment(ctx context.Context, ideaID, authorID, body string, parentCommentID *string) (*domain.Comment, string, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var parentAuthorID string
	if parentCommentID != nil {
		err := tx.QueryRow(ctx,
			`SELECT author_user_id FROM idea_comments WHERE id = $1 AND idea_id = $2`,
			*parentCommentID, ideaID,
		).Scan(&parentAuthorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("parent comment not in this idea")
		}
		if err != nil {
			return nil, "", fmt.Errorf("loading parent comment: %w", err)
		}
	}

	var (
		id        string
		createdAt time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO idea_comments (idea_id, parent_comment_id, author_user_id, body)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, ideaID, parentCommentID, authorID, body).Scan(&id, &createdAt)
	if err != nil {
		return nil, "", fmt.Errorf("inserting comment: %w", err)
	}

	var authorName string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(name, email) FROM users WHERE id = $1`, authorID,
	).Scan(&authorName); err != nil {
		return nil, "", fmt.Errorf("loading author name: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("commit comment tx: %w", err)
	}

	comment := domain.ReconstructComment(id, ideaID, parentCommentID, authorID, authorName, body, createdAt)
	return comment, parentAuthorID, nil
}

// ListCommentsForIdea returns all comments flat as domain entities, sorted by
// created_at ASC. The service builds the reply tree from this.
func (r *Repository) ListCommentsForIdea(ctx context.Context, ideaID string) ([]*domain.Comment, error) {
	const q = `
		SELECT c.id, c.idea_id, c.parent_comment_id, c.author_user_id,
		       COALESCE(u.name, u.email) AS author_name, c.body, c.created_at
		FROM idea_comments c
		JOIN users u ON u.id = c.author_user_id
		WHERE c.idea_id = $1
		ORDER BY c.created_at ASC
	`
	rows, err := r.db.Query(ctx, q, ideaID)
	if err != nil {
		return nil, fmt.Errorf("listing comments: %w", err)
	}
	defer rows.Close()

	var out []*domain.Comment
	for rows.Next() {
		var (
			id, cIdeaID, authorID, authorName, body string
			parent                                  *string
			createdAt                               time.Time
		)
		if err := rows.Scan(
			&id, &cIdeaID, &parent, &authorID, &authorName, &body, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scanning comment: %w", err)
		}
		out = append(out, domain.ReconstructComment(id, cIdeaID, parent, authorID, authorName, body, createdAt))
	}
	return out, rows.Err()
}

// GetIdeaAuthor returns the author_user_id for the given idea. Used by the
// service to decide who to notify on comments and to block self-voting.
func (r *Repository) GetIdeaAuthor(ctx context.Context, ideaID string) (string, error) {
	var authorID string
	err := r.db.QueryRow(ctx, `SELECT author_user_id FROM ideas WHERE id = $1`, ideaID).Scan(&authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting idea author: %w", err)
	}
	return authorID, nil
}
