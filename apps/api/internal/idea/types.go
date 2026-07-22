package idea

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"

	"livecart/apps/api/internal/idea/domain"
	"livecart/apps/api/lib/query"
)

// =============================================================================
// Handler layer - Request types (syntactic gate via ozzo) + ToInput mappers
// =============================================================================

type CreateIdeaRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
}

// Validate is the syntactic gate (ozzo). Category membership is a semantic rule
// checked in the service against the category catalog.
func (r CreateIdeaRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Title, validation.Required, validation.Length(8, 140)),
		validation.Field(&r.Description, validation.Required, validation.Length(20, 5000)),
		validation.Field(&r.Category, validation.Required),
	)
}

// ToInput builds the usecase input. There are no value objects to construct for
// an idea (author is the authenticated caller, category is a plain slug), so the
// mapping cannot fail — the error return keeps the handler shape uniform.
func (r CreateIdeaRequest) ToInput(authorID string) (CreateIdeaInput, error) {
	return CreateIdeaInput{
		AuthorID:    authorID,
		Title:       r.Title,
		Description: r.Description,
		Category:    r.Category,
	}, nil
}

type CreateCommentRequest struct {
	Body            string  `json:"body"`
	ParentCommentID *string `json:"parentCommentId,omitempty"`
}

// Validate is the syntactic gate (ozzo). parentCommentId, when present, must be
// a UUID.
func (r CreateCommentRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Body, validation.Required, validation.Length(1, 5000)),
		validation.Field(&r.ParentCommentID, is.UUIDv4),
	)
}

// ToInput builds the usecase input for posting a comment/reply.
func (r CreateCommentRequest) ToInput(ideaID, authorID string) (CreateCommentInput, error) {
	return CreateCommentInput{
		IdeaID:          ideaID,
		AuthorID:        authorID,
		Body:            r.Body,
		ParentCommentID: r.ParentCommentID,
	}, nil
}

// =============================================================================
// Handler layer - Response types + outbound mappers (presentation knows domain)
// =============================================================================

type IdeaResponse struct {
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

// NewIdeaResponse maps an Idea entity to its API response. callerID resolves the
// caller-relative isAuthor flag; the category label is derived from the catalog.
func NewIdeaResponse(i *domain.Idea, callerID string) IdeaResponse {
	return IdeaResponse{
		ID:            i.ID(),
		Number:        i.Number(),
		Title:         i.Title(),
		Description:   i.Description(),
		Category:      i.Category(),
		CategoryLabel: categoryLabel(i.Category()),
		Status:        i.Status(),
		AuthorID:      i.AuthorID(),
		AuthorName:    i.AuthorName(),
		VoteCount:     i.VoteCount(),
		CommentCount:  i.CommentCount(),
		VotedByMe:     i.VotedByMe(),
		IsAuthor:      i.IsAuthoredBy(callerID),
		CreatedAt:     i.CreatedAt(),
	}
}

type ListIdeasResponse struct {
	Data       []IdeaResponse           `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

// NewListIdeasResponse maps a page of Idea entities to the feed response.
func NewListIdeasResponse(ideas []*domain.Idea, callerID string, pagination query.Pagination, total int) ListIdeasResponse {
	data := make([]IdeaResponse, len(ideas))
	for i, it := range ideas {
		data[i] = NewIdeaResponse(it, callerID)
	}
	return ListIdeasResponse{
		Data:       data,
		Pagination: query.NewPaginationResponse(pagination, total),
	}
}

type CommentResponse struct {
	ID         string            `json:"id"`
	Body       string            `json:"body"`
	AuthorID   string            `json:"authorId"`
	AuthorName string            `json:"authorName"`
	CreatedAt  time.Time         `json:"createdAt"`
	Replies    []CommentResponse `json:"replies"`
}

// NewCommentResponse maps a Comment entity (with its reply subtree) to the API
// response, recursing over replies.
func NewCommentResponse(c *domain.Comment) CommentResponse {
	replies := make([]CommentResponse, len(c.Replies()))
	for i, r := range c.Replies() {
		replies[i] = NewCommentResponse(r)
	}
	return CommentResponse{
		ID:         c.ID(),
		Body:       c.Body(),
		AuthorID:   c.AuthorID(),
		AuthorName: c.AuthorName(),
		CreatedAt:  c.CreatedAt(),
		Replies:    replies,
	}
}

// NewCommentResponses maps a slice of comment roots to responses.
func NewCommentResponses(comments []*domain.Comment) []CommentResponse {
	out := make([]CommentResponse, len(comments))
	for i, c := range comments {
		out[i] = NewCommentResponse(c)
	}
	return out
}

type IdeaDetailResponse struct {
	IdeaResponse
	Comments []CommentResponse `json:"comments"`
}

// NewIdeaDetailResponse maps an IdeaDetail read model to the detail response.
func NewIdeaDetailResponse(d *domain.IdeaDetail, callerID string) IdeaDetailResponse {
	return IdeaDetailResponse{
		IdeaResponse: NewIdeaResponse(d.Idea(), callerID),
		Comments:     NewCommentResponses(d.Comments()),
	}
}

type ToggleVoteResponse struct {
	VoteCount int  `json:"voteCount"`
	VotedByMe bool `json:"votedByMe"`
}

// NewToggleVoteResponse maps a VoteResult to the vote toggle response.
func NewToggleVoteResponse(v *domain.VoteResult) ToggleVoteResponse {
	return ToggleVoteResponse{
		VoteCount: v.VoteCount(),
		VotedByMe: v.VotedByMe(),
	}
}

// categoryLabel resolves a category slug to its PT-BR display label, falling
// back to the slug when unknown.
func categoryLabel(slug string) string {
	for _, cat := range Categories {
		if cat.Slug == slug {
			return cat.Label
		}
	}
	return slug
}

// =============================================================================
// Service layer - Input types (usecases return *domain entities)
// =============================================================================

type CreateIdeaInput struct {
	AuthorID    string
	Title       string
	Description string
	Category    string
}

type ListIdeasInput struct {
	UserID     string // internal UUID of caller (for "mine" tab and votedByMe enrichment)
	Tab        string
	Category   string
	Search     string
	Sort       string
	Pagination query.Pagination
}

type CreateCommentInput struct {
	IdeaID          string
	AuthorID        string
	Body            string
	ParentCommentID *string
}
