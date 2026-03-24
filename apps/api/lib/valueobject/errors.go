package valueobject

import "errors"

// General value object errors
var (
	ErrInvalidEmail = errors.New("invalid email format")
	ErrEmptyEmail   = errors.New("email cannot be empty")

	ErrInvalidUUID = errors.New("invalid uuid format")
	ErrEmptyUUID   = errors.New("uuid cannot be empty")

	ErrInvalidRole = errors.New("invalid role")
	ErrEmptyRole   = errors.New("role cannot be empty")

	ErrNegativeMoney = errors.New("money cannot be negative")
)
