package user

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"livecart/apps/api/internal/events"
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
	// Verify webhook signature if secret is configured.
	// ATENÇÃO: com CLERK_WEBHOOK_SECRET vazio a verificação é pulada inteira e
	// esta rota — que cria/altera/apaga usuários e não passa pelo AuthMiddleware
	// — fica aberta. Manter o segredo setado em staging e produção.
	webhookSecret := os.Getenv("CLERK_WEBHOOK_SECRET")
	if webhookSecret != "" {
		ok := verifyWebhookSignature(
			c.Body(),
			c.Get("svix-id"),
			c.Get("svix-timestamp"),
			c.Get("svix-signature"),
			webhookSecret,
		)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(httpx.Envelope{Error: "invalid signature"})
		}
	}

	var payload ClerkWebhookPayload
	if err := c.BodyParser(&payload); err != nil {
		return httpx.BadRequest(c, "invalid request body")
	}

	switch payload.Type {
	case "user.created":
		return h.handleUserCreated(c, payload.Data)
	case "user.updated":
		return h.handleUserUpdated(c, payload.Data)
	case "user.deleted":
		return h.handleUserDeleted(c, payload.Data)
	default:
		// Ignore other events
		return httpx.OK(c, fiber.Map{"message": "event ignored"})
	}
}

func (h *WebhookHandler) handleUserCreated(c *fiber.Ctx, data json.RawMessage) error {
	var userData ClerkUserData
	if err := json.Unmarshal(data, &userData); err != nil {
		return httpx.BadRequest(c, "invalid user data")
	}

	email := getPrimaryEmail(userData)
	name := ""
	if userData.FirstName != "" || userData.LastName != "" {
		name = strings.TrimSpace(userData.FirstName + " " + userData.LastName)
	}

	// Upsert user - will create if doesn't exist
	_, err := h.service.repo.UpsertUser(c.UserContext(), userData.ID, email, name, userData.ImageURL)
	if err != nil {
		return httpx.HandleServiceError(c, err)
	}

	// Group K: user.signed_up fact (best-effort, observability only).
	_ = events.EmitInternal(c.UserContext(), h.service.repo.q, events.UserSignedUp,
		"user.signed_up:"+userData.ID, struct {
			ClerkID string `json:"clerk_id"`
			Email   string `json:"email"`
		}{ClerkID: userData.ID, Email: email})

	return httpx.OK(c, fiber.Map{"message": "user created"})
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

	// Update user in users table
	err := h.service.UpdateUser(c.UserContext(), userData.ID, email, name, userData.ImageURL)
	if err != nil {
		// If user not found, create it (webhook ordering issue)
		if httpx.IsNotFound(err) {
			_, createErr := h.service.repo.UpsertUser(c.UserContext(), userData.ID, email, name, userData.ImageURL)
			if createErr != nil {
				return httpx.HandleServiceError(c, createErr)
			}
			// Group K: this fallback path is effectively a creation.
			_ = events.EmitInternal(c.UserContext(), h.service.repo.q, events.UserSignedUp,
				"user.signed_up:"+userData.ID, struct {
					ClerkID string `json:"clerk_id"`
					Email   string `json:"email"`
				}{ClerkID: userData.ID, Email: email})
			return httpx.OK(c, fiber.Map{"message": "user created (from update)"})
		}
		return httpx.HandleServiceError(c, err)
	}

	// Group K: user.updated fact (best-effort, observability only). The dedup
	// key folds in email+name+avatar so identical redeliveries collapse while a
	// real profile change produces a distinct event.
	_ = events.EmitInternal(c.UserContext(), h.service.repo.q, events.UserUpdated,
		"user.updated:"+userData.ID+":"+email+":"+name+":"+userData.ImageURL, struct {
			ClerkID string `json:"clerk_id"`
			Email   string `json:"email"`
		}{ClerkID: userData.ID, Email: email})

	return httpx.OK(c, fiber.Map{"message": "user updated"})
}

func (h *WebhookHandler) handleUserDeleted(c *fiber.Ctx, data json.RawMessage) error {
	var userData struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &userData); err != nil {
		return httpx.BadRequest(c, "invalid user data")
	}

	err := h.service.DeleteUser(c.UserContext(), userData.ID)
	if err != nil {
		// If user not found, that's OK
		if httpx.IsNotFound(err) {
			return httpx.OK(c, fiber.Map{"message": "user not found"})
		}
		return httpx.HandleServiceError(c, err)
	}

	// Group K: user.deleted fact (best-effort, observability only).
	_ = events.EmitInternal(c.UserContext(), h.service.repo.q, events.UserDeleted,
		"user.deleted:"+userData.ID, struct {
			ClerkID string `json:"clerk_id"`
		}{ClerkID: userData.ID})

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

// Janela de tolerância do timestamp, igual à do Svix. Barra o replay de uma
// entrega capturada sem rejeitar entregas legitimamente atrasadas.
const webhookTolerance = 5 * time.Minute

// svixSignature computes the base64 HMAC-SHA256 that Svix (and therefore Clerk)
// expects for a delivery.
//
// Três detalhes que uma implementação ingênua erra — e cada um sozinho já
// rejeita 100% dos webhooks: o conteúdo assinado é "id.timestamp.body" e não o
// body puro; a chave é o que vem DEPOIS do prefixo whsec_, decodificado de
// base64 para bytes; e a saída é base64, não hex.
func svixSignature(secret, msgID, timestamp string, payload []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)

	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyWebhookSignature validates a Svix-signed delivery: headers present,
// timestamp inside the tolerance window and a matching v1 signature.
func verifyWebhookSignature(payload []byte, msgID, timestamp, signatureHeader, secret string) bool {
	if msgID == "" || timestamp == "" || signatureHeader == "" {
		return false
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if drift := time.Since(time.Unix(seconds, 0)); drift > webhookTolerance || drift < -webhookTolerance {
		return false
	}

	expected, err := svixSignature(secret, msgID, timestamp, payload)
	if err != nil {
		return false
	}

	// O header carrega uma lista separada por espaço ("v1,<sig> v1,<sig>"):
	// durante uma rotação de segredo o Svix assina com os dois. Basta uma bater.
	for _, entry := range strings.Split(signatureHeader, " ") {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}

	return false
}
