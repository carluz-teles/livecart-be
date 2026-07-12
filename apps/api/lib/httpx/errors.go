package httpx

import (
	"errors"

	"github.com/gofiber/fiber/v2"
)

type ServiceError struct {
	Code    int
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

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
