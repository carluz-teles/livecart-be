package httpx

import "github.com/gofiber/fiber/v2"

// Validatable is any request DTO that validates itself. Request types implement
// it with an ozzo `func (r XRequest) Validate() error`.
type Validatable interface {
	Validate() error
}

// BindAndValidate parses the request body into dst and runs its Validate() in a
// single step. It returns:
//   - ErrBadRequest (400) when the body cannot be parsed,
//   - the ozzo validation.Errors (rendered as 422 {error, fields} by ErrorHandler)
//     when validation fails,
//   - nil when the request is well-formed and valid.
//
// Usage in a handler:
//
//	var req CreateXRequest
//	if err := httpx.BindAndValidate(c, &req); err != nil {
//	    return err
//	}
func BindAndValidate(c *fiber.Ctx, dst Validatable) error {
	if err := c.BodyParser(dst); err != nil {
		return ErrBadRequest("invalid request body")
	}
	return dst.Validate()
}
