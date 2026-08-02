package httpx

import (
	"errors"

	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

var logger *zap.Logger

// SetLogger sets the package-level logger for error logging.
func SetLogger(l *zap.Logger) {
	logger = l
}

// Envelope is the standard API response wrapper. RequestID acompanha respostas
// de erro para correlacionar o que o cliente reporta com a linha de log.
type Envelope struct {
	Data      any    `json:"data,omitempty"`
	Error     string `json:"error,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	// Reason is an optional stable machine code accompanying an error so the
	// frontend can branch without matching the human message.
	Reason string `json:"reason,omitempty"`
}

// ValidationEnvelope is the response for validation errors.
type ValidationEnvelope struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields"`
}

func OK(c *fiber.Ctx, data any) error {
	return c.Status(fiber.StatusOK).JSON(Envelope{Data: data})
}

func Created(c *fiber.Ctx, data any) error {
	return c.Status(fiber.StatusCreated).JSON(Envelope{Data: data})
}

func NoContent(c *fiber.Ctx) error {
	return c.SendStatus(fiber.StatusNoContent)
}

// DeletedResponse is the response for successful DELETE operations.
type DeletedResponse struct {
	ID string `json:"id"`
}

func Deleted(c *fiber.Ctx, id string) error {
	return c.Status(fiber.StatusOK).JSON(Envelope{Data: DeletedResponse{ID: id}})
}

func BadRequest(c *fiber.Ctx, msg string) error {
	return c.Status(fiber.StatusBadRequest).JSON(Envelope{Error: msg})
}

func Unauthorized(c *fiber.Ctx, msg string) error {
	return c.Status(fiber.StatusUnauthorized).JSON(Envelope{Error: msg})
}

func Forbidden(c *fiber.Ctx, msg string) error {
	return c.Status(fiber.StatusForbidden).JSON(Envelope{Error: msg})
}

func InternalError(c *fiber.Ctx, msg string) error {
	return c.Status(fiber.StatusInternalServerError).JSON(Envelope{Error: msg})
}

func ValidationError(c *fiber.Ctx, err error) error {
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return BadRequest(c, "invalid request")
	}

	fields := make(map[string]string, len(ve))
	for _, fe := range ve {
		fields[fe.Field()] = fe.Tag()
	}

	return c.Status(fiber.StatusUnprocessableEntity).JSON(ValidationEnvelope{
		Error:  "validation failed",
		Fields: fields,
	})
}

func HandleServiceError(c *fiber.Ctx, err error) error {
	reqID := RequestID(c)

	var se *ServiceError
	if errors.As(err, &se) {
		if se.Code >= 500 {
			logUnhandledError(c, err, reqID)
			// 5xx is hardened: the client NEVER sees se.Message, se.Reason or the
			// wrapped cause — those can carry a pgx string, a provider URL or a
			// driver error. Force a generic body regardless of what the throw site
			// put on the ServiceError. The rich detail lives only in the log above.
			return c.Status(se.Code).JSON(Envelope{
				Error:     "internal server error",
				RequestID: reqID,
				Reason:    string(CodeInternal),
			})
		}
		if logger != nil {
			// Client errors (4xx) are expected — warn keeps the message visible
			// without paging anyone. Tag category/code so every error line is
			// queryable by category (unclassified 4xx defaults to DOMAIN).
			category := string(se.Category)
			if category == "" {
				category = string(CategoryDomain)
			}
			logger.Warn("service error", append([]zap.Field{
				zap.Int("status", se.Code),
				zap.String("message", se.Message),
				zap.String("category", category),
				zap.String("code", se.Reason),
				zap.String("request_id", reqID),
				zap.String("path", c.Path()),
				zap.String("method", c.Method()),
			}, identityFields(c)...)...)
		}
		return c.Status(se.Code).JSON(Envelope{Error: se.Message, RequestID: reqID, Reason: se.Reason})
	}

	// Erros do próprio Fiber (rota inexistente → 404, método não permitido →
	// 405, body acima do limite → 413...) mantêm o status real. Antes caíam no
	// ramo genérico e viravam 500 "internal error" — poluía o log de erro e
	// mascarava o que de fato aconteceu.
	var fe *fiber.Error
	if errors.As(err, &fe) {
		if fe.Code >= 500 {
			logUnhandledError(c, err, reqID)
		} else if logger != nil {
			logger.Warn("http error", append([]zap.Field{
				zap.Int("status", fe.Code),
				zap.String("message", fe.Message),
				zap.String("request_id", reqID),
				zap.String("path", c.Path()),
				zap.String("method", c.Method()),
			}, identityFields(c)...)...)
		}
		return c.Status(fe.Code).JSON(Envelope{Error: fe.Message, RequestID: reqID})
	}

	logUnhandledError(c, err, reqID)
	return c.Status(fiber.StatusInternalServerError).JSON(Envelope{Error: "internal server error", RequestID: reqID})
}

// logUnhandledError é a linha que diz O QUE quebrou e para QUEM: erro completo
// (cadeia de wraps) + request_id + rota + identidade. O access log do
// RequestLogger só diz que o request falhou; o diagnóstico começa por aqui.
func logUnhandledError(c *fiber.Ctx, err error, reqID string) {
	if logger == nil {
		return
	}
	// Category/code default to INFRASTRUCTURE/INTERNAL: a bare (non-ServiceError)
	// error reaching here is by definition an uncategorized infra/bug failure.
	category, code := string(CategoryInfrastructure), string(CodeInternal)
	var se *ServiceError
	hasSE := errors.As(err, &se)
	if hasSE {
		if se.Category != "" {
			category = string(se.Category)
		}
		if se.Reason != "" {
			code = se.Reason
		}
	}
	fields := []zap.Field{
		zap.Error(err),
		zap.String("category", category),
		zap.String("code", code),
		zap.String("request_id", reqID),
		zap.String("method", c.Method()),
		zap.String("path", c.Path()),
		zap.String("ip", c.IP()),
	}
	// The wrapped cause/op never reach the client (5xx is forced generic) but are
	// the actual diagnosis — surface them here, in the log only.
	if hasSE {
		if se.cause != nil {
			fields = append(fields, zap.NamedError("cause", se.cause))
		}
		if se.op != "" {
			fields = append(fields, zap.String("op", se.op))
		}
	}
	if route := c.Route(); route != nil && route.Path != "" {
		fields = append(fields, zap.String("route", route.Path))
	}
	fields = append(fields, identityFields(c)...)
	logger.Error("internal error", fields...)
}

// identityFields devolve quem sofreu o erro: user + store (id e slug), quando
// o request os carrega nos Locals.
func identityFields(c *fiber.Ctx) []zap.Field {
	fields := make([]zap.Field, 0, 3)
	if userID := GetUserID(c); userID != "" {
		fields = append(fields, zap.String("user_id", userID))
	}
	if storeID := GetStoreID(c); storeID != "" {
		fields = append(fields, zap.String("store_id", storeID))
	}
	if storeSlug := GetStoreSlug(c); storeSlug != "" {
		fields = append(fields, zap.String("store_slug", storeSlug))
	}
	if traceID, ok := c.Locals("trace_id").(string); ok && traceID != "" {
		fields = append(fields, zap.String("trace_id", traceID))
	}
	return fields
}
