package user

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/lib/httpx"
)

// ClerkWebhookPayload represents the Clerk webhook event structure
type ClerkWebhookPayload struct {
	Data   json.RawMessage `json:"data"`
	Object string          `json:"object"`
	Type   string          `json:"type"`
}

// ClerkUserData represents user data from Clerk webhooks
type ClerkUserData struct {
	ID             string `json:"id"`
	EmailAddresses []struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
	} `json:"email_addresses"`
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	ImageURL       string `json:"image_url"`
	PrimaryEmailID string `json:"primary_email_address_id"`
}

// WebhookHandler handles Clerk webhook events
type WebhookHandler struct {
	service *Service
}

func NewWebhookHandler(service *Service) *WebhookHandler {
	return &WebhookHandler{service: service}
}

func (h *WebhookHandler) RegisterRoutes(app *fiber.App) {
	// Webhook routes are not authenticated via JWT, they use webhook signature
	app.Post("/api/webhooks/clerk", h.HandleClerkWebhook)
}

// HandleClerkWebhook godoc
// @Summary      Handle Clerk webhook events
// @Description  Receives and processes Clerk webhook events for user sync
// @Tags         webhooks
// @Accept       json
// @Produce      json
// @Success      200 {object} httpx.Envelope
// @Failure      400 {object} httpx.Envelope
// @Failure      401 {object} httpx.Envelope
// @Router       /api/webhooks/clerk [post]
func (h *WebhookHandler) HandleClerkWebhook(c *fiber.Ctx) error {
	// Verify webhook signature if secret is configured
	webhookSecret := os.Getenv("CLERK_WEBHOOK_SECRET")
	if webhookSecret != "" {
		signature := c.Get("svix-signature")
		if !verifyWebhookSignature(c.Body(), signature, webhookSecret) {
			return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "invalid signature"})
		}
	}

	var payload ClerkWebhookPayload
	if err := c.BodyParser(&payload); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	switch payload.Type {
	case "user.updated":
		return h.handleUserUpdated(c, payload.Data)
	case "user.deleted":
		return h.handleUserDeleted(c, payload.Data)
	default:
		// Ignore other events
		return httpx.OK(c, fiber.Map{"message": "event ignored"})
	}
}

func (h *WebhookHandler) handleUserUpdated(c *fiber.Ctx, data json.RawMessage) error {
	var userData ClerkUserData
	if err := json.Unmarshal(data, &userData); err != nil {
		return httpx.BadRequest(c, "invalid user data")
	}

	email := getPrimaryEmail(userData)
	name := ""
	if userData.FirstName != "" || userData.LastName != "" {
		name = strings.TrimSpace(userData.FirstName + " " + userData.LastName)
	}

	err := h.service.UpdateUserAllStores(c.Context(), userData.ID, email, name, userData.ImageURL)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, fiber.Map{"message": "user updated"})
}

func (h *WebhookHandler) handleUserDeleted(c *fiber.Ctx, data json.RawMessage) error {
	var userData struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &userData); err != nil {
		return httpx.BadRequest(c, "invalid user data")
	}

	err := h.service.DeleteUser(c.Context(), userData.ID)
	if err != nil {
		// If user not found, that's OK
		if httpx.IsNotFound(err) {
			return httpx.OK(c, fiber.Map{"message": "user not found"})
		}
		return httpx.HandleServiceError(c, err)
	}

	return httpx.OK(c, fiber.Map{"message": "user deleted"})
}

func getPrimaryEmail(userData ClerkUserData) string {
	for _, email := range userData.EmailAddresses {
		if email.ID == userData.PrimaryEmailID {
			return email.EmailAddress
		}
	}
	if len(userData.EmailAddresses) > 0 {
		return userData.EmailAddresses[0].EmailAddress
	}
	return ""
}

func verifyWebhookSignature(payload []byte, signature, secret string) bool {
	// Svix signature format: v1,<signature>
	parts := strings.Split(signature, ",")
	if len(parts) < 2 {
		return false
	}

	// Get the signature part
	sig := ""
	for _, part := range parts {
		if strings.HasPrefix(part, "v1=") {
			sig = strings.TrimPrefix(part, "v1=")
			break
		}
	}
	if sig == "" {
		return false
	}

	// Compute expected signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(sig), []byte(expected))
}
