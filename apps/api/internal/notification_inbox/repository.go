package notification_inbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) List(ctx context.Context, in ListNotificationsInput) ([]NotificationRow, int, error) {
	where := "WHERE n.recipient_id = $1"
	if in.UnreadOnly {
		where += " AND n.read_at IS NULL"
	}

	var total int
	if err := r.db.QueryRow(ctx, "SELECT COUNT(*) FROM notifications n "+where, in.UserID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting notifications: %w", err)
	}

	limit := in.Pagination.Limit
	offset := in.Pagination.Offset()

	q := fmt.Sprintf(`
		SELECT n.id, n.type, n.idea_id, n.comment_id,
		       n.actor_id, COALESCE(u.name, u.email) AS actor_name,
		       i.number, i.title,
		       n.payload, n.read_at, n.created_at
		FROM notifications n
		LEFT JOIN users u ON u.id = n.actor_id
		LEFT JOIN ideas i ON i.id = n.idea_id
		%s
		ORDER BY n.created_at DESC
		LIMIT %d OFFSET %d
	`, where, limit, offset)

	rows, err := r.db.Query(ctx, q, in.UserID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing notifications: %w", err)
	}
	defer rows.Close()

	out := make([]NotificationRow, 0, limit)
	for rows.Next() {
		var n NotificationRow
		var actorName *string
		if err := rows.Scan(
			&n.ID, &n.Type, &n.IdeaID, &n.CommentID,
			&n.ActorID, &actorName,
			&n.IdeaNumber, &n.IdeaTitle,
			&n.Payload, &n.ReadAt, &n.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning notification: %w", err)
		}
		n.ActorName = actorName
		out = append(out, n)
	}
	return out, total, rows.Err()
}

func (r *Repository) UnreadCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := r.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE recipient_id = $1 AND read_at IS NULL`,
		userID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting unread: %w", err)
	}
	return n, nil
}

func (r *Repository) MarkRead(ctx context.Context, userID, notifID string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE notifications SET read_at = now()
		 WHERE id = $1 AND recipient_id = $2 AND read_at IS NULL`,
		notifID, userID,
	)
	if err != nil {
		return fmt.Errorf("marking notification read: %w", err)
	}
	_ = tag
	return nil
}

func (r *Repository) MarkAllRead(ctx context.Context, userID string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE notifications SET read_at = now()
		 WHERE recipient_id = $1 AND read_at IS NULL`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("marking all read: %w", err)
	}
	return nil
}

// InsertIdeaNotification is the low-level write used by the writer adapter for
// idea-related notifications. payload is raw JSON; commentID may be nil for
// status-change events.
func (r *Repository) InsertIdeaNotification(
	ctx context.Context,
	notifType, recipientID string,
	actorID *string,
	ideaID string,
	commentID *string,
	payload []byte,
) error {
	// Cast $6 to jsonb so pgx serializes the []byte as JSON text instead of
	// bytea (hex), which Postgres rejects for jsonb columns with SQLSTATE 22P02.
	_, err := r.db.Exec(ctx, `
		INSERT INTO notifications (recipient_id, actor_id, type, idea_id, comment_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
	`, recipientID, actorID, notifType, ideaID, commentID, string(payload))
	if err != nil {
		return fmt.Errorf("inserting notification: %w", err)
	}
	return nil
}
