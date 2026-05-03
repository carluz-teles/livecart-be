package notification_inbox

import (
	"encoding/json"
	"time"

	"livecart/apps/api/lib/query"
)

const (
	TypeIdeaComment      = "idea_comment"
	TypeIdeaReply        = "idea_reply"
	TypeIdeaStatusChange = "idea_status_change"
)

// =============================================================================
// HTTP request/response
// =============================================================================

type NotificationResponse struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	IdeaID     *string         `json:"ideaId,omitempty"`
	CommentID  *string         `json:"commentId,omitempty"`
	ActorID    *string         `json:"actorId,omitempty"`
	ActorName  *string         `json:"actorName,omitempty"`
	IdeaNumber *int64          `json:"ideaNumber,omitempty"`
	IdeaTitle  *string         `json:"ideaTitle,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	ReadAt     *time.Time      `json:"readAt,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

type ListNotificationsResponse struct {
	Data        []NotificationResponse   `json:"data"`
	UnreadCount int                      `json:"unreadCount"`
	Pagination  query.PaginationResponse `json:"pagination"`
}

type UnreadCountResponse struct {
	Count int `json:"count"`
}

// =============================================================================
// Service / repository row
// =============================================================================

type NotificationRow struct {
	ID         string
	Type       string
	IdeaID     *string
	CommentID  *string
	ActorID    *string
	ActorName  *string
	IdeaNumber *int64
	IdeaTitle  *string
	Payload    []byte
	ReadAt     *time.Time
	CreatedAt  time.Time
}

type ListNotificationsInput struct {
	UserID     string
	UnreadOnly bool
	Pagination query.Pagination
}
