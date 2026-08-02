package httpx

import (
	"errors"

	"github.com/gofiber/fiber/v2"
)

type ServiceError struct {
	Code    int
	Message string
	// Reason is an optional stable machine code (e.g. "PAYMENT_NOT_CONFIGURED")
	// the frontend can branch on instead of matching the human message. Empty
	// for most errors; set via WithReason/WithCode or by DomainError. Typed
	// values live in codes.go (Code).
	Reason string
	// Category is a server-side observability tag (DOMAIN | REPOSITORY |
	// INFRASTRUCTURE). It is NEVER serialized onto Envelope — the client only
	// ever sees Code (Reason) + HTTP status. Zero value means "unclassified" and
	// is treated as DOMAIN for 4xx / INFRASTRUCTURE for 5xx at the log site.
	Category Category
	// cause is the wrapped original error, kept for the server log only and
	// exposed via Unwrap. It is NEVER written to the client (5xx responses are
	// forced generic in HandleServiceError), so a pgx/driver string can't leak.
	cause error
	// op names the failing operation (e.g. "GetCart") for repository/infra logs.
	op string
}

func (e *ServiceError) Error() string { return e.Message }

// Unwrap exposes the wrapped cause so errors.Is/errors.As and zap can walk the
// real chain server-side, even though Error() only returns the client-safe
// Message. Returns nil when there is no cause (the common 4xx case).
func (e *ServiceError) Unwrap() error { return e.cause }

// WithReason attaches a stable machine code to a ServiceError so the frontend
// can render a tailored message. No-op if err is not a *ServiceError.
func WithReason(err error, reason string) error {
	var se *ServiceError
	if errors.As(err, &se) {
		se.Reason = reason
	}
	return err
}

// WithCode is the typed sibling of WithReason: it attaches a catalog Code
// (codes.go) to a *ServiceError. No-op if err is not a *ServiceError. Prefer
// DomainError when raising a fresh error; use WithCode to code an existing
// Err* value with a one-line change.
func WithCode(err error, code Code) error {
	return WithReason(err, string(code))
}

// DomainError is the primary way to raise a business-rule error carrying a stable
// FE code. status is the HTTP status (400/403/404/409/410/422); msg is the human
// fallback message; code is the machine code the FE branches on.
func DomainError(status int, code Code, msg string) error {
	return &ServiceError{Code: status, Message: msg, Reason: string(code), Category: CategoryDomain}
}

// RepositoryError wraps a persistence (DB) failure. The client sees a generic
// 500 + INTERNAL with no detail; the wrapped cause, op and category are carried
// for the server log only (via Unwrap / logUnhandledError).
func RepositoryError(cause error, op string) error {
	return &ServiceError{
		Code:     500,
		Message:  "internal server error",
		Reason:   string(CodeInternal),
		Category: CategoryRepository,
		cause:    cause,
		op:       op,
	}
}

// InfrastructureError wraps an external-service/network failure. Same client
// contract as RepositoryError (generic 500 + INTERNAL), tagged INFRASTRUCTURE
// for the server log.
func InfrastructureError(cause error, op string) error {
	return &ServiceError{
		Code:     500,
		Message:  "internal server error",
		Reason:   string(CodeInternal),
		Category: CategoryInfrastructure,
		cause:    cause,
		op:       op,
	}
}

func ErrBadRequest(msg string) error    { return &ServiceError{Code: 400, Message: msg} }
func ErrNotFound(msg string) error      { return &ServiceError{Code: 404, Message: msg} }
func ErrForbidden(msg string) error     { return &ServiceError{Code: 403, Message: msg} }
func ErrConflict(msg string) error      { return &ServiceError{Code: 409, Message: msg} }
func ErrGone(msg string) error          { return &ServiceError{Code: 410, Message: msg} }
func ErrUnprocessable(msg string) error { return &ServiceError{Code: 422, Message: msg} }
func ErrInternal(msg string) error      { return &ServiceError{Code: 500, Message: msg} }

func IsNotFound(err error) bool {
	var se *ServiceError
	return errors.As(err, &se) && se.Code == 404
}

// StatusFromError resolve o status HTTP que um erro produz — o mesmo
// mapeamento que HandleServiceError aplica ao escrever a resposta. Usado pelo
// RequestLogger, que roda antes do ErrorHandler e por isso não pode ler o
// status da response em requests com erro.
func StatusFromError(err error) int {
	var se *ServiceError
	if errors.As(err, &se) {
		return se.Code
	}
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return fiber.StatusInternalServerError
}
