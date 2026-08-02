package integration

import (
	"encoding/json"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
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

	logger.From(c.Context(), h.logger).Info("instagram webhook verification request",
		zap.String("mode", mode),
		zap.Bool("has_challenge", challenge != ""),
		zap.String("received_token", verifyToken),
		zap.String("expected_token", expectedToken),
	)

	if mode == "subscribe" && verifyToken == expectedToken {
		logger.From(c.Context(), h.logger).Info("instagram webhook verification successful")
		return c.SendString(challenge)
	}

	logger.From(c.Context(), h.logger).Warn("instagram webhook verification failed",
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
		logger.From(c.Context(), h.logger).Error("empty instagram webhook body")
		return httpx.BadRequest(c, "empty body")
	}

	// Signature check. Deploy 1 runs in observation mode: the outcome is
	// computed and logged, but the payload is processed regardless — a wrong
	// INSTAGRAM_APP_SECRET in production would otherwise stop every comment
	// from becoming a cart, and we would find out from the sales chart instead
	// of an alert. Flip INSTAGRAM_WEBHOOK_ENFORCE_SIGNATURE once the logs show
	// only "valid" for real Meta traffic.
	outcome := verifyInstagramSignature(body, c.Get("X-Hub-Signature-256"), config.InstagramAppSecret.String())
	enforcing := config.InstagramWebhookEnforceSignature.BoolOr(false)

	if !outcome.ok() {
		log := logger.From(c.Context(), h.logger).With(
			zap.String("signature_outcome", string(outcome)),
			zap.Bool("enforcing", enforcing),
			zap.Int("body_bytes", len(body)),
		)
		if enforcing {
			log.Warn("instagram webhook rejected: signature check failed")
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid signature"})
		}
		log.Warn("instagram webhook accepted with failing signature (observation mode)")
	}

	// Parse the webhook payload
	var payload InstagramWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.From(c.Context(), h.logger).Error("failed to parse instagram webhook payload", zap.Error(err))
		return httpx.BadRequest(c, "invalid webhook payload")
	}

	logger.From(c.Context(), h.logger).Info("instagram webhook received",
		zap.String("object", payload.Object),
		zap.Int("entries", len(payload.Entry)),
	)

	// Process each entry
	for _, entry := range payload.Entry {
		// Process changes (live_comments)
		for _, change := range entry.Changes {
			if err := h.processInstagramChange(c, entry, change, body); err != nil {
				logger.From(c.Context(), h.logger).Error("failed to process instagram change",
					zap.String("field", change.Field),
					zap.Error(err),
				)
				// Continue processing other changes
			}
		}

		// Process messaging (DMs)
		for _, msg := range entry.Messaging {
			if err := h.processInstagramMessage(c, entry, msg, body); err != nil {
				logger.From(c.Context(), h.logger).Error("failed to process instagram message",
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
	logger.From(c.Context(), h.logger).Info("processing instagram change",
		zap.String("account_id", entry.ID),
		zap.String("field", change.Field),
	)

	switch change.Field {
	case "live_comments":
		return h.processLiveComment(c, entry, change, rawBody, events.SourceInstagramLive)
	case "comments":
		// Feed-post comments share the same payload shape as live comments and
		// the same processing path (the post media id maps to the event). Mark
		// the post event as webhook-active so the polling capture can stop.
		return h.processPostComment(c, entry, change, rawBody)
	default:
		logger.From(c.Context(), h.logger).Info("ignoring unhandled instagram change field",
			zap.String("field", change.Field),
		)
		return nil
	}
}

// processPostComment handles a feed-post comment event. It reuses the live
// comment path and flags the mapped post event so polling stops.
func (h *WebhookHandler) processPostComment(c *fiber.Ctx, entry InstagramEntry, change InstagramChange, rawBody []byte) error {
	valueBytes, err := json.Marshal(change.Value)
	if err != nil {
		return err
	}
	var comment InstagramLiveCommentValue
	if err := json.Unmarshal(valueBytes, &comment); err != nil {
		return err
	}
	if comment.Media.ID != "" {
		if mErr := h.service.MarkMediaWebhookActive(c.Context(), comment.Media.ID); mErr != nil {
			logger.From(c.Context(), h.logger).Warn("failed to mark post event webhook active",
				zap.String("media_id", comment.Media.ID),
				zap.Error(mErr),
			)
		}
	}
	return h.processLiveComment(c, entry, change, rawBody, events.SourceInstagramPost)
}

// processLiveComment processes a live_comments (or feed-post comments) event.
// source is the canonical origin stamped on the dispatched comment.received
// event (instagram_live for live comments, instagram_post for feed posts).
func (h *WebhookHandler) processLiveComment(c *fiber.Ctx, entry InstagramEntry, change InstagramChange, rawBody []byte, source events.Source) error {
	// Parse the value as LiveCommentValue
	valueBytes, err := json.Marshal(change.Value)
	if err != nil {
		return err
	}

	var comment InstagramLiveCommentValue
	if err := json.Unmarshal(valueBytes, &comment); err != nil {
		return err
	}

	logger.From(c.Context(), h.logger).Info("processing live comment",
		zap.String("account_id", entry.ID),
		zap.String("comment_id", comment.CommentID),
		zap.String("user_id", comment.From.ID),
		zap.String("username", comment.From.Username),
		zap.String("text", comment.Text),
		zap.String("media_id", comment.Media.ID),
	)

	// Prefer the Instagram-scoped ID (IGSID) when present — it is the only
	// identifier accepted by the Messaging API as recipient.id. The plain
	// from.id is app-scoped and cannot be used to send DMs.
	userID := comment.From.SelfIGScopedID
	if userID == "" {
		userID = comment.From.ID
	}

	// Inverted flow: dispatch a canonical comment.received event (transactional
	// outbox) and return fast. The domain work (cart/stock/waitlist) runs in the
	// event consumer via ProcessInstagramComment — idempotent by comment_id.
	if err := h.service.DispatchCommentReceived(c.Context(), ProcessInstagramCommentInput{
		AccountID: entry.ID,
		MediaID:   comment.Media.ID,
		CommentID: comment.CommentID,
		UserID:    userID,
		Username:  comment.From.Username,
		Text:      comment.Text,
		// E41: era entry.Time cru — o carimbo da ENTREGA do webhook, e em
		// milissegundos nos produtos de Instagram. time.Unix(ms, 0) aterrissa no
		// ano ~57000, então a janela de 7 dias do private reply (RN-37) nunca
		// disparava por este caminho: ela só valia no polling, que já carregava
		// segundos. CommentedAt prefere o carimbo do próprio comentário e
		// normaliza ms→s.
		Timestamp:  comment.CommentedAt(entry.Time),
		RawPayload: rawBody,
	}, source); err != nil {
		logger.From(c.Context(), h.logger).Error("failed to dispatch instagram comment",
			zap.String("comment_id", comment.CommentID),
			zap.Error(err),
		)
		// Don't return error - we still want to acknowledge the webhook.
	}

	return nil
}

// processInstagramMessage processes a messaging event (DM)
func (h *WebhookHandler) processInstagramMessage(c *fiber.Ctx, entry InstagramEntry, msg InstagramMessage, rawBody []byte) error {
	logger.From(c.Context(), h.logger).Info("processing instagram message",
		zap.String("account_id", entry.ID),
		zap.String("sender_id", msg.Sender.ID),
		zap.String("message_id", msg.Message.MID),
		zap.String("text", msg.Message.Text),
	)

	// A reply to a story (reply_to.story.id) maps the DM to a story-commerce event.
	var replyToStoryID string
	if msg.Message.ReplyTo != nil && msg.Message.ReplyTo.Story != nil {
		replyToStoryID = msg.Message.ReplyTo.Story.ID
	}

	// Process the message through the service
	if err := h.service.ProcessInstagramMessage(c.Context(), ProcessInstagramMessageInput{
		AccountID: entry.ID,
		SenderID:  msg.Sender.ID,
		MessageID: msg.Message.MID,
		Text:      msg.Message.Text,
		// A Messaging API carimba em milissegundos. A resposta a um story reply
		// sai por DM (que não tem a janela de 7 dias do private reply), mas o
		// valor viaja para ProcessInstagramCommentInput.Timestamp — deixá-lo em
		// ms seria plantar o mesmo bug para o próximo consumidor.
		Timestamp:      epochSeconds(msg.Timestamp),
		ReplyToStoryID: replyToStoryID,
		IsEcho:         msg.Message.IsEcho,
		RawPayload:     rawBody,
	}); err != nil {
		logger.From(c.Context(), h.logger).Error("failed to process instagram message",
			zap.String("message_id", msg.Message.MID),
			zap.Error(err),
		)
		// Don't return error - we still want to acknowledge the webhook
	}

	return nil
}
