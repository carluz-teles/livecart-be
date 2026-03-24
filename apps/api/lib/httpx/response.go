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

// Envelope is the standard API response wrapper.
type Envelope struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
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
	var se *ServiceError
	if errors.As(err, &se) {
		// Log client errors (4xx) at warn level, they're expected
		if logger != nil && se.Code >= 400 && se.Code < 500 {
			logger.Warn("service error",
				zap.Int("status", se.Code),
				zap.String("message", se.Message),
				zap.String("path", c.Path()),
				zap.String("method", c.Method()),
			)
		}
		return c.Status(se.Code).JSON(Envelope{Error: se.Message})
	}

	// Log unexpected errors (5xx) at error level
	if logger != nil {
		logger.Error("internal error",
			zap.Error(err),
			zap.String("path", c.Path()),
			zap.String("method", c.Method()),
		)
	}
	return c.Status(fiber.StatusInternalServerError).JSON(Envelope{Error: "internal server error"})
}
