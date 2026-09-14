package providers

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TinyCheckoutReconciliationError is a verified business conflict, not an
// infrastructure failure. Fields contain labels only, never customer data.
type TinyCheckoutReconciliationError struct {
	OrderID   string
	Status    int
	InvoiceID int64
	Fields    []string
}

func (e *TinyCheckoutReconciliationError) Error() string {
	status, _ := ERPOrderStatusFromSituacao(e.Status)
	message := fmt.Sprintf("pedido Tiny %s (%s) exige conciliação", e.OrderID, status)
	if len(e.Fields) > 0 {
		message += ": confira " + strings.Join(e.Fields, ", ")
	}
	return message
}

// TinyPaidCheckoutFinalizer is optional: Tiny uses a durable operation when
// the final commercial data requires replacing an existing reservation.
type TinyPaidCheckoutFinalizer interface {
	FinalizePaidCheckout(context.Context, *TinyCheckoutOperation, TinyCheckoutJournal) (*OrderResult, error)
}

// TinyCheckoutOperation is a durable checkpoint, saved BEFORE non-idempotent calls.
// It contains only the business request and progress; never credentials/HTTP logs.
type TinyCheckoutOperation struct {
	ID                 string
	CartID             string
	SourceID           string
	SourceAnchor       string
	TargetID           string
	TargetNumber       string
	TargetStatus       ERPOrderStatus
	Order              ERPOrder
	StartedAt          time.Time
	Prepared           bool
	Replace            bool
	CreateStarted      bool
	ExpectedShippingID int64
	AccountsRequired   bool
	AccountsCleared    bool
	SourceCancelled    bool
	Completed          bool
}

type TinyCheckoutJournal interface {
	Save(context.Context, *TinyCheckoutOperation) error
	Bind(context.Context, *TinyCheckoutOperation) error
}
