package live

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/google/uuid"

	"livecart/apps/api/internal/live/domain"
	"livecart/apps/api/lib/httpx"
)

type RecoverCommentRequest struct {
	Confirmed bool `json:"confirmed"`
}

func (r RecoverCommentRequest) Validate() error {
	return validation.ValidateStruct(&r, validation.Field(&r.Confirmed, validation.Required))
}

type RecoverCommentInput struct {
	StoreID   string
	EventID   string
	CommentID string
	ActorID   string
}

func (r RecoverCommentRequest) ToInput(storeID, eventID, commentID, actorID string) (RecoverCommentInput, error) {
	for _, id := range []string{storeID, eventID, commentID, actorID} {
		if _, err := uuid.Parse(id); err != nil {
			return RecoverCommentInput{}, httpx.DomainError(422, httpx.CodeCommentRecoveryInvalid, "identificador inválido")
		}
	}
	return RecoverCommentInput{StoreID: storeID, EventID: eventID, CommentID: commentID, ActorID: actorID}, nil
}

type RecoverCommentResponse struct {
	Status string `json:"status"`
}

func NewRecoverCommentResponse(recovery *domain.CommentRecovery) RecoverCommentResponse {
	return RecoverCommentResponse{Status: recovery.Status}
}
