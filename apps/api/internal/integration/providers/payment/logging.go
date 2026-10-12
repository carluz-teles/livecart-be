package payment

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/mercadopago/sdk-go/pkg/mperror"
	"go.uber.org/zap"
)

func logCardPaymentOutcome(log *zap.Logger, provider string, status PaymentState, fields []zap.Field) {
	switch status {
	case PaymentApproved:
		log.Info(provider+" card payment captured", fields...)
	case PaymentRejected:
		log.Info(provider+" card payment rejected", fields...)
	case PaymentPending, PaymentInProcess:
		log.Info(provider+" card payment pending", fields...)
	default:
		log.Info(provider+" card payment updated", fields...)
	}
}

// The gateway message remains available to the buyer. Logging uses only the
// operation and HTTP status, because provider validation errors can echo PII.
type responseError struct {
	message string
	summary string
	cause   error
}

func (e *responseError) Error() string      { return e.message }
func (e *responseError) LogMessage() string { return e.summary }
func (e *responseError) Unwrap() error      { return e.cause }

func mercadoPagoError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	var response *mperror.ResponseError
	if !errors.As(err, &response) {
		return wrapped
	}
	return &responseError{
		message: wrapped.Error(),
		summary: fmt.Sprintf("mercado pago %s: HTTP %d", operation, response.StatusCode),
		cause:   wrapped,
	}
}

func paymentResponseError(operation string, status int, message string) error {
	summary := fmt.Sprintf("%s: HTTP %d", operation, status)
	return &responseError{message: summary + ": " + message, summary: summary}
}

func pagarmeResponseError(operation string, status int, message string) error {
	return paymentResponseError("pagarme "+operation, status, message)
}

func pagarmeGatewayStatusCode(response map[string]any) int {
	switch code := response["code"].(type) {
	case float64:
		return int(code)
	case string:
		value, err := strconv.Atoi(code)
		if err == nil {
			return value
		}
	}
	return 0
}
