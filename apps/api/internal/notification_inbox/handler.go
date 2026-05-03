package notification_inbox

import (
	"encoding/json"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/query"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(router fiber.Router) {
	g := router.Group("/notifications")
	g.Get("/", h.List)
	g.Get("/unread-count", h.UnreadCount)
	g.Post("/:id/read", h.MarkRead)
	g.Post("/read-all", h.MarkAllRead)
}

func (h *Handler) List(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)

	rows, total, unread, err := h.service.List(c.Context(), ListNotificationsInput{
		UserID:     userID,
		UnreadOnly: c.QueryBool("unreadOnly", false),
		Pagination: query.Pagination{
			Page:  c.QueryInt("page", query.DefaultPage),
			Limit: c.QueryInt("limit", query.DefaultLimit),
		},
	})
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	resp := make([]NotificationResponse, len(rows))
	for i, n := range rows {
		resp[i] = toResponse(n)
	}

	pagination := query.NewPaginationResponse(query.Pagination{
		Page:  c.QueryInt("page", query.DefaultPage),
		Limit: c.QueryInt("limit", query.DefaultLimit),
	}, total)

	return httpx.OK(c, ListNotificationsResponse{
		Data:        resp,
		UnreadCount: unread,
		Pagination:  pagination,
	})
}

func (h *Handler) UnreadCount(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	n, err := h.service.UnreadCount(c.Context(), userID)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.OK(c, UnreadCountResponse{Count: n})
}

func (h *Handler) MarkRead(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	id := c.Params("id")
	if err := h.service.MarkRead(c.Context(), userID, id); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.NoContent(c)
}

func (h *Handler) MarkAllRead(c *fiber.Ctx) error {
	userID := httpx.GetInternalUserID(c)
	if err := h.service.MarkAllRead(c.Context(), userID); err != nil {
		return httpx.HandleServiceError(c, err)
	}
	return httpx.NoContent(c)
}

func toResponse(n NotificationRow) NotificationResponse {
	payload := json.RawMessage(n.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return NotificationResponse{
		ID:         n.ID,
		Type:       n.Type,
		IdeaID:     n.IdeaID,
		CommentID:  n.CommentID,
		ActorID:    n.ActorID,
		ActorName:  n.ActorName,
		IdeaNumber: n.IdeaNumber,
		IdeaTitle:  n.IdeaTitle,
		Payload:    payload,
		ReadAt:     n.ReadAt,
		CreatedAt:  n.CreatedAt,
	}
}
