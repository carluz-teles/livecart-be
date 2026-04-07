package integration

import (
	"encoding/json"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// HandleInstagramVerification handles GET /api/webhooks/instagram for Meta webhook verification
// @Summary Handle Instagram webhook verification
// @Description Verifies the webhook endpoint with Meta's challenge-response mechanism
// @Tags webhooks
// @Param hub.mode query string true "Must be 'subscribe'"
// @Param hub.challenge query string true "Challenge to return"
// @Param hub.verify_token query string true "Verification token"
// @Success 200 {string} string "Returns the challenge"
// @Failure 403 {object} map[string]string
// @Router /api/webhooks/instagram [get]
func (h *WebhookHandler) HandleInstagramVerification(c *fiber.Ctx) error {
	mode := c.Query("hub.mode")
	challenge := c.Query("hub.challenge")
	verifyToken := c.Query("hub.verify_token")

	expectedToken := config.InstagramVerifyToken.StringOr("livecart_verify_token")

	h.logger.Info("instagram webhook verification request",
		zap.String("mode", mode),
		zap.Bool("has_challenge", challenge != ""),
		zap.String("received_token", verifyToken),
		zap.String("expected_token", expectedToken),
	)

	if mode == "subscribe" && verifyToken == expectedToken {
		h.logger.Info("instagram webhook verification successful")
		return c.SendString(challenge)
	}

	h.logger.Warn("instagram webhook verification failed",
		zap.String("mode", mode),
		zap.Bool("token_match", verifyToken == expectedToken),
	)

	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"error": "verification failed",
	})
}

// HandleInstagramWebhook handles POST /api/webhooks/instagram for incoming events
// @Summary Handle Instagram webhook events
// @Description Receives and processes Instagram live_comments and messages events
// @Tags webhooks
// @Accept json
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/webhooks/instagram [post]
func (h *WebhookHandler) HandleInstagramWebhook(c *fiber.Ctx) error {
	body := c.Body()
	if len(body) == 0 {
		h.logger.Error("empty instagram webhook body")
		return httpx.BadRequest(c, "empty body")
	}

	// Parse the webhook payload
	var payload InstagramWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Error("failed to parse instagram webhook payload", zap.Error(err))
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	h.logger.Info("instagram webhook received",
		zap.String("object", payload.Object),
		zap.Int("entries", len(payload.Entry)),
	)

	// Process each entry
	for _, entry := range payload.Entry {
		// Process changes (live_comments)
		for _, change := range entry.Changes {
			if err := h.processInstagramChange(c, entry, change, body); err != nil {
				h.logger.Error("failed to process instagram change",
					zap.String("field", change.Field),
					zap.Error(err),
				)
				// Continue processing other changes
			}
		}

		// Process messaging (DMs)
		for _, msg := range entry.Messaging {
			if err := h.processInstagramMessage(c, entry, msg, body); err != nil {
				h.logger.Error("failed to process instagram message",
					zap.String("sender_id", msg.Sender.ID),
					zap.Error(err),
				)
				// Continue processing other messages
			}
		}
	}

	return httpx.OK(c, fiber.Map{"status": "received"})
}

// processInstagramChange processes a single change event (like live_comments)
func (h *WebhookHandler) processInstagramChange(c *fiber.Ctx, entry InstagramEntry, change InstagramChange, rawBody []byte) error {
	h.logger.Info("processing instagram change",
		zap.String("account_id", entry.ID),
		zap.String("field", change.Field),
	)

	switch change.Field {
	case "live_comments":
		return h.processLiveComment(c, entry, change, rawBody)
	default:
		h.logger.Info("ignoring unhandled instagram change field",
			zap.String("field", change.Field),
		)
		return nil
	}
}

// processLiveComment processes a live_comments event
func (h *WebhookHandler) processLiveComment(c *fiber.Ctx, entry InstagramEntry, change InstagramChange, rawBody []byte) error {
	// Parse the value as LiveCommentValue
	valueBytes, err := json.Marshal(change.Value)
	if err != nil {
		return err
	}

	var comment InstagramLiveCommentValue
	if err := json.Unmarshal(valueBytes, &comment); err != nil {
		return err
	}

	h.logger.Info("processing live comment",
		zap.String("account_id", entry.ID),
		zap.String("comment_id", comment.CommentID),
		zap.String("user_id", comment.From.ID),
		zap.String("username", comment.From.Username),
		zap.String("text", comment.Text),
		zap.String("media_id", comment.Media.ID),
	)

	// Process the comment through the service
	if err := h.service.ProcessInstagramComment(c.Context(), ProcessInstagramCommentInput{
		AccountID: entry.ID,
		MediaID:   comment.Media.ID,
		CommentID: comment.CommentID,
		UserID:    comment.From.ID,
		Username:  comment.From.Username,
		Text:      comment.Text,
		Timestamp: entry.Time,
	}); err != nil {
		h.logger.Error("failed to process instagram comment",
			zap.String("comment_id", comment.CommentID),
			zap.Error(err),
		)
		// Don't return error - we still want to acknowledge the webhook
	}

	return nil
}

// processInstagramMessage processes a messaging event (DM)
func (h *WebhookHandler) processInstagramMessage(c *fiber.Ctx, entry InstagramEntry, msg InstagramMessage, rawBody []byte) error {
	h.logger.Info("processing instagram message",
		zap.String("account_id", entry.ID),
		zap.String("sender_id", msg.Sender.ID),
		zap.String("message_id", msg.Message.MID),
		zap.String("text", msg.Message.Text),
	)

	// Process the message through the service
	if err := h.service.ProcessInstagramMessage(c.Context(), ProcessInstagramMessageInput{
		AccountID: entry.ID,
		SenderID:  msg.Sender.ID,
		MessageID: msg.Message.MID,
		Text:      msg.Message.Text,
		Timestamp: msg.Timestamp,
	}); err != nil {
		h.logger.Error("failed to process instagram message",
			zap.String("message_id", msg.Message.MID),
			zap.Error(err),
		)
		// Don't return error - we still want to acknowledge the webhook
	}

	return nil
}
