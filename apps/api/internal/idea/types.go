package idea

import (
	"time"

	"livecart/apps/api/lib/query"
)

// =============================================================================
// HTTP request/response types
// =============================================================================

type CreateIdeaRequest struct {
	Title       string `json:"title" validate:"required,min=8,max=140"`
	Description string `json:"description" validate:"required,min=20,max=5000"`
	Category    string `json:"category" validate:"required"`
}

type CreateCommentRequest struct {
	Body            string  `json:"body" validate:"required,min=1,max=5000"`
	ParentCommentID *string `json:"parentCommentId,omitempty" validate:"omitempty,uuid"`
}

type IdeaListItemResponse struct {
	ID            string    `json:"id"`
	Number        int64     `json:"number"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Category      string    `json:"category"`
	CategoryLabel string    `json:"categoryLabel"`
	Status        string    `json:"status"`
	AuthorID      string    `json:"authorId"`
	AuthorName    string    `json:"authorName"`
	VoteCount     int       `json:"voteCount"`
	CommentCount  int       `json:"commentCount"`
	VotedByMe     bool      `json:"votedByMe"`
	IsAuthor      bool      `json:"isAuthor"`
	CreatedAt     time.Time `json:"createdAt"`
}

type ListIdeasResponse struct {
	Data       []IdeaListItemResponse   `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type CommentNodeResponse struct {
	ID         string                `json:"id"`
	Body       string                `json:"body"`
	AuthorID   string                `json:"authorId"`
	AuthorName string                `json:"authorName"`
	CreatedAt  time.Time             `json:"createdAt"`
	Replies    []CommentNodeResponse `json:"replies"`
}

type IdeaDetailResponse struct {
	IdeaListItemResponse
	Comments []CommentNodeResponse `json:"comments"`
}

type ToggleVoteResponse struct {
	VoteCount int  `json:"voteCount"`
	VotedByMe bool `json:"votedByMe"`
}

type CategoriesResponse struct {
	Data []Category `json:"data"`
}

// =============================================================================
// Service layer
// =============================================================================

type ListIdeasInput struct {
	UserID     string // internal UUID of caller (for "mine" tab and votedByMe enrichment)
	Tab        string
	Category   string
	Search     string
	Sort       string
	Pagination query.Pagination
}

type ListIdeasOutput struct {
	Items      []IdeaListItem
	Total      int
	Pagination query.Pagination
}

type IdeaListItem struct {
	ID           string
	Number       int64
	Title        string
	Description  string
	Category     string
	Status       string
	AuthorID     string
	AuthorName   string
	VoteCount    int
	CommentCount int
	VotedByMe    bool
	CreatedAt    time.Time
}

type IdeaDetail struct {
	IdeaListItem
	Comments []CommentNode
}

type CommentNode struct {
	ID         string
	Body       string
	AuthorID   string
	AuthorName string
	CreatedAt  time.Time
	Replies    []CommentNode
}

type CommentRow struct {
	ID              string
	IdeaID          string
	ParentCommentID *string
	AuthorID        string
	AuthorName      string
	Body            string
	CreatedAt       time.Time
}
