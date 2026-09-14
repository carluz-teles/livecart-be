package providers

import (
	"context"
	"time"
)

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
