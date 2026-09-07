package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// verifyPaymentAttempt runs with the cart locked. An obsolete quote records the
// money received for review, without marking newly-added items as paid.
func verifyPaymentAttempt(ctx context.Context, tx pgx.Tx, cartID, paymentID, method string, amount int64, paidAt *time.Time, facts []events.Envelope) (review bool, err error) {
	var attemptID, storeID, provider string
	for _, fact := range facts {
		var payload struct {
			StoreID  string                   `json:"store_id"`
			Snapshot *providers.PaymentStatus `json:"payment_snapshot"`
		}
		if err := json.Unmarshal(fact.Payload, &payload); err != nil {
			return false, err
		}
		storeID = payload.StoreID
		provider = string(fact.Source)
		if payload.Snapshot != nil {
			attemptID, _ = payload.Snapshot.Metadata["livecart_attempt_id"].(string)
		}
	}
	if storeID != "" {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT e.store_id::text FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1`, cartID).Scan(&owner); err != nil {
			return false, err
		}
		if owner != storeID {
			return false, fmt.Errorf("payment store does not own cart")
		}
	}
	if provider == "internal" {
		var reviewRequired bool
		if err := tx.QueryRow(ctx, `SELECT payment_review_required FROM carts WHERE id=$1`, cartID).Scan(&reviewRequired); err != nil {
			return false, err
		}
		if reviewRequired {
			return false, httpx.DomainError(409, httpx.CodePaymentReviewRequired, "este pedido já tem dinheiro recebido em conferência; concilie o pagamento antes de confirmar outro recebimento")
		}
	}
	var id, fingerprint, current string
	var expected int64
	err = tx.QueryRow(ctx, `SELECT id::text,amount_cents,cart_fingerprint,cart_payment_fingerprint(cart_id) FROM payment_attempts WHERE cart_id=$1 AND (payment_id=$2 OR id::text=NULLIF($3,'')) AND ($4='' OR provider=$4) ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, cartID, paymentID, attemptID, provider).Scan(&id, &expected, &fingerprint, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		var hasAttempts bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_attempts WHERE cart_id=$1)`, cartID).Scan(&hasAttempts); err != nil {
			return false, err
		}
		if !hasAttempts || provider == "internal" {
			return false, nil
		} // legacy or merchant confirmation
		return false, fmt.Errorf("payment attempt is not bound yet: %s", paymentID)
	}
	if err != nil {
		return false, err
	}
	if fingerprint == current && amount == expected {
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET status='paid' WHERE id=$1`, id)
		return false, err
	}
	reason := "cart changed after payment was created"
	if amount != expected {
		reason = "gateway amount differs from the payment quote"
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_attempts SET status='review_required',review_reason=$2 WHERE id=$1`, id, reason); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO cart_payments(cart_id,amount_cents,gross_covered_cents,method,checkout_id,paid_at) VALUES($1,$2,0,$3,$4,COALESCE($5,now())) ON CONFLICT(cart_id,checkout_id) DO NOTHING`, cartID, amount, method, paymentID, paidAt); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE carts SET payment_review_required=true,payment_status='pending',expires_at=NULL,paid_amount_cents=paid_amount_cents+$2 WHERE id=$1`, cartID, amount)
	return true, err
}
