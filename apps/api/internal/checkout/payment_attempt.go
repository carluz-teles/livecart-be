package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/internal/cartedit"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
)

// Freeze the exact quote before contacting the gateway. Binding its external ID
// is separate because a webhook can arrive before the create response.
func (r *Repository) createPaymentAttempt(ctx context.Context, pool *pgxpool.Pool, cartID, integrationID, provider, method string, amount int64, items []providers.CheckoutItem) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var review, payable bool
	if err = tx.QueryRow(ctx, `SELECT payment_review_required,status IN ('active','checkout') AND COALESCE(payment_status,'pending') NOT IN ('paid','refunded') FROM carts WHERE id=$1 FOR UPDATE`, cartID).Scan(&review, &payable); err != nil {
		return "", err
	}
	if err := cartedit.AssertReady(ctx, tx, cartID); err != nil {
		return "", err
	}
	if review {
		return "", httpx.DomainError(409, httpx.CodePaymentReviewRequired, "há um pagamento recebido em conferência; aguarde a loja antes de pagar novamente")
	}
	if !payable {
		return "", httpx.DomainError(409, httpx.CodeCartNotPayable, "o carrinho não está disponível para pagamento")
	}
	var subtotal, shipping, coupon int64
	var pct int
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT SUM((quantity-waitlisted_quantity)*unit_price) FROM cart_items WHERE cart_id=c.id),0),COALESCE(c.shipping_cost_cents,0),COALESCE(c.coupon_discount_cents,0),e.pix_discount_percent FROM carts c JOIN live_events e ON e.id=c.event_id WHERE c.id=$1`, cartID).Scan(&subtotal, &shipping, &coupon, &pct); err != nil {
		return "", err
	}
	expected := max(int64(0), subtotal+shipping-coupon)
	if method == "pix" {
		expected -= max(int64(0), expected-shipping) * int64(pct) / 100
	}
	if expected != amount {
		return "", httpx.DomainError(409, httpx.CodeCartItemChanged, "o carrinho mudou; atualize os valores antes de pagar")
	}
	var valid bool
	raw, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	// Compare the quantities and prices actually sent, including same-total swaps.
	// Simple protocol infers []byte as bytea; send JSON text for the jsonb cast.
	if err = tx.QueryRow(ctx, `WITH sent AS (SELECT x->>'id' AS id,(x->>'quantity')::int AS qty,(x->>'unit_price')::bigint AS price FROM jsonb_array_elements($2::jsonb) x), current AS (SELECT product_id::text AS id,quantity-waitlisted_quantity AS qty,unit_price AS price FROM cart_items WHERE cart_id=$1 AND quantity>waitlisted_quantity) SELECT NOT EXISTS((SELECT * FROM sent EXCEPT SELECT * FROM current) UNION ALL (SELECT * FROM current EXCEPT SELECT * FROM sent))`, cartID, string(raw)).Scan(&valid); err != nil {
		return "", err
	}
	if !valid {
		return "", httpx.DomainError(409, httpx.CodeCartItemChanged, "os itens mudaram; atualize o carrinho antes de pagar")
	}
	var id string
	if err = tx.QueryRow(ctx, `INSERT INTO payment_attempts(cart_id,integration_id,provider,amount_cents,cart_fingerprint) VALUES($1,$2,$3,$4,cart_payment_fingerprint($1)) RETURNING id::text`, cartID, integrationID, provider, amount).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (r *Repository) bindPaymentAttempt(ctx context.Context, pool *pgxpool.Pool, attemptID, paymentID, cancelID string) error {
	if paymentID == "" {
		return fmt.Errorf("gateway returned no payment id")
	}
	_, err := pool.Exec(ctx, `UPDATE payment_attempts SET payment_id=$2,cancel_id=NULLIF($3,''),status=CASE WHEN status='created' THEN 'pending' ELSE status END WHERE id=$1 AND (payment_id IS NULL OR payment_id=$2)`, attemptID, paymentID, cancelID)
	return err
}

func (r *Repository) pixAttemptIntegration(ctx context.Context, pool *pgxpool.Pool, cartID, cancelID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT integration_id::text FROM payment_attempts WHERE cart_id=$1 AND cancel_id=$2 ORDER BY created_at DESC LIMIT 1`, cartID, cancelID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
