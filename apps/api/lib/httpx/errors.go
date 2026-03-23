package httpx

import "errors"

type ServiceError struct {
	Code    int
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

func ErrNotFound(msg string) error      { return &ServiceError{Code: 404, Message: msg} }
func ErrConflict(msg string) error      { return &ServiceError{Code: 409, Message: msg} }
func ErrUnprocessable(msg string) error { return &ServiceError{Code: 422, Message: msg} }
func ErrForbidden(msg string) error     { return &ServiceError{Code: 403, Message: msg} }

func IsNotFound(err error) bool {
	var se *ServiceError
	return errors.As(err, &se) && se.Code == 404
}
