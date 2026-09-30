package erp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PaymentReview records a reconciliation observation, not a new receipt/refund.
type PaymentReview struct {
	ExternalOrderID string    `json:"externalOrderId"`
	Reason          string    `json:"reason"`
	PaidCents       *int64    `json:"paidCents,omitempty"`
	OrderTotalCents *int64    `json:"orderTotalCents,omitempty"`
	DetectedAt      time.Time `json:"detectedAt"`
	CheckedAt       time.Time `json:"checkedAt"`
}

type PaymentReviewRecorder interface {
	RecordERPFinancialReview(context.Context, string, string, PaymentReview) error
	ResolveERPFinancialReview(context.Context, string, string, string, time.Time) error
}

func ReadPaymentReview(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, cartID string) (*PaymentReview, error) {
	var review PaymentReview
	err := db.QueryRow(ctx, `SELECT external_order_id,reason,paid_cents,order_total_cents,detected_at,checked_at
        FROM erp_payment_reviews WHERE cart_id=$1 AND resolved_at IS NULL`, cartID).
		Scan(&review.ExternalOrderID, &review.Reason, &review.PaidCents, &review.OrderTotalCents,
			&review.DetectedAt, &review.CheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ERP financial review: %w", err)
	}
	return &review, nil
}
