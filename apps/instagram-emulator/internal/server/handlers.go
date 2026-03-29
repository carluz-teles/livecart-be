package server

import (
	"github.com/gofiber/fiber/v2"
	"github.com/livecart/instagram-emulator/internal/webhook"
)

// handleWebhookVerification handles GET /webhook for Meta webhook verification
func (s *Server) handleWebhookVerification(c *fiber.Ctx) error {
	mode := c.Query("hub.mode")
	challenge := c.Query("hub.challenge")
	verifyToken := c.Query("hub.verify_token")

	// Validate the verification request
	if mode == "subscribe" && verifyToken == s.config.VerifyToken {
		// Return the challenge to confirm subscription
		return c.SendString(challenge)
	}

	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"error": "Verification failed",
	})
}

// handleGetLiveMedia handles GET /{userId}/live_media
// Returns active live media for the specified user
func (s *Server) handleGetLiveMedia(c *fiber.Ctx) error {
	// Note: In the real Meta API, you'd validate the access_token
	// For the emulator, we just return the current live state

	response := webhook.LiveMediaResponse{
		Data:   []webhook.LiveMedia{},
		Paging: webhook.Paging{},
	}

	// If there's an active live, return it
	if s.session.IsLiveActive() {
		liveMedia := webhook.LiveMedia{
			ID:               s.session.GetMediaID(),
			MediaType:        "BROADCAST",
			MediaProductType: "LIVE",
			Owner: webhook.Owner{
				ID: s.session.GetAccountID(),
			},
			Username: s.session.GetUsername(),
			Comments: webhook.CommentsData{
				Data: []interface{}{},
			},
		}
		response.Data = append(response.Data, liveMedia)
	}

	return c.JSON(response)
}

// handleHealth handles GET /health
func (s *Server) handleHealth(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":      "ok",
		"live_active": s.session.IsLiveActive(),
		"media_id":    s.session.GetMediaID(),
	})
}
