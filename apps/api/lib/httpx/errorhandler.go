package httpx

import (
	"errors"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// ErrorHandler is the single Fiber error handler for the whole API: every
// handler just `return err` and this renders the FE-facing response.
//
// Validation errors (ozzo validation.Errors, returned by a Request's Validate()
// or by BindAndValidate) become a 422 {error, fields} keyed by json tag;
// everything else (ServiceError, fiber.Error, unexpected) defers to
// HandleServiceError.
func ErrorHandler(c *fiber.Ctx, err error) error {
	if fields, ok := validationFields(err); ok {
		if logger != nil {
			// Validation 422s were silent before — the only error category with no
			// server log line. Emit one Warn (client error) tagged category=VALIDATION
			// so the data-quality channel is observable like DOMAIN/REPOSITORY/INFRA.
			// Only field_count is logged, NEVER the field values/messages: those carry
			// user input (email, cpf, address...) and logging them would be a PII leak.
			// Mirrors the 4xx Warn in response.go. The wire response is byte-identical.
			logger.Warn("validation error", append([]zap.Field{
				zap.String("category", string(CategoryValidation)),
				zap.Int("status", fiber.StatusUnprocessableEntity),
				zap.Int("field_count", len(fields)),
				zap.String("request_id", RequestID(c)),
				zap.String("path", c.Path()),
				zap.String("method", c.Method()),
			}, identityFields(c)...)...)
		}
		return c.Status(fiber.StatusUnprocessableEntity).JSON(ValidationEnvelope{
			Error:  "validation failed",
			Fields: fields,
		})
	}
	return HandleServiceError(c, err)
}

// validationFields flattens an ozzo validation.Errors (possibly nested via
// ValidateStruct on embedded/child structs) into a flat field→message map. Keys
// come from the json struct tag (ozzo's default), so they match the request body
// the frontend sent.
func validationFields(err error) (map[string]string, bool) {
	var ve validation.Errors
	if !errors.As(err, &ve) {
		return nil, false
	}
	out := make(map[string]string, len(ve))
	flattenValidation("", ve, out)
	return out, true
}

func flattenValidation(prefix string, ve validation.Errors, out map[string]string) {
	for key, fieldErr := range ve {
		if prefix != "" {
			key = prefix + "." + key
		}
		var nested validation.Errors
		if errors.As(fieldErr, &nested) {
			flattenValidation(key, nested, out)
			continue
		}
		out[key] = fieldErr.Error()
	}
}
