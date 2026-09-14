package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"livecart/apps/api/internal/integration/providers"
	providererp "livecart/apps/api/internal/integration/providers/erp"
)

// PrepareTinyPaidOrder is called with the cart finalisation lock and mutating
// claim held. Its durable operation survives an API or database interruption.
func (s *Service) PrepareTinyPaidOrder(ctx context.Context, provider providers.ERPProvider, cartID, storeID, sourceID string) (string, error) {
	finalizer, ok := provider.(providers.TinyPaidCheckoutFinalizer)
	if !ok {
		return "", fmt.Errorf("tiny paid checkout finalizer is not configured")
	}
	integration, err := s.repo.GetActiveERP(ctx, storeID)
	if err != nil {
		return "", err
	}
	if integration.Provider != "tiny" {
		return "", fmt.Errorf("checkout integration is not Tiny")
	}
	cart, err := s.repo.GetCartForPaidOrder(ctx, cartID)
	if err != nil {
		return "", err
	}
	if cart.StoreID != storeID {
		return "", fmt.Errorf("checkout cart belongs to another store")
	}
	checkout, err := s.loadTinyPaidCheckout(ctx, cartID, storeID)
	if err != nil {
		return "", err
	}
	grid, err := s.repo.ListCartGridItems(ctx, cartID)
	if err != nil {
		return "", err
	}
	var items []providers.ERPOrderItem
	var total int64
	for _, i := range grid {
		if i.Quantity <= 0 {
			continue
		}
		if i.ProductExternalID == "" {
			return "", fmt.Errorf("tiny: item pago sem vínculo com ERP")
		}
		items = append(items, providers.ERPOrderItem{ProductID: i.ProductExternalID, Name: i.ProductName, Quantity: i.Quantity, UnitPrice: i.UnitPrice, Note: strings.TrimSpace(providers.LiveCartItemMarker + " " + i.ProductKeyword)})
		total += int64(i.Quantity) * i.UnitPrice
	}
	total += checkout.FreightCents - checkout.DiscountCents
	var paid int64
	for _, p := range checkout.Payments {
		paid += p.AmountCents
	}
	if len(items) == 0 || total <= 0 || total != paid {
		return "", fmt.Errorf("tiny: itens, frete e desconto não correspondem ao valor pago")
	}
	journal := &tinyCheckoutJournal{repo: s.repo, cartID: cartID, storeID: storeID, integrationID: integration.ID, sourceID: sourceID}
	op, err := journal.load(ctx)
	if err != nil {
		return "", err
	}
	sourceAnchor := "lc-cart-" + cartID
	if op != nil && op.Completed && op.TargetID == sourceID {
		sourceAnchor = "lc-cart-" + op.Order.ExternalID
	}
	if op != nil && (!tinySameSnapshot(op.Order.Checkout, &checkout) || !tinySameSnapshot(op.Order.Items, items)) {
		if !op.Completed {
			// Preserve the recorded request while recovering an interrupted POST.
			// A later payment becomes a new revision only after that operation is
			// bound; otherwise changing its marker/payload could duplicate orders.
			previous, err := finalizer.FinalizePaidCheckout(ctx, op, journal)
			if err != nil {
				return "", err
			}
			return s.PrepareTinyPaidOrder(ctx, provider, cartID, storeID, previous.OrderID)
		}
		op = nil
	}
	if op == nil {
		operationID := uuid.NewString()
		op = &providers.TinyCheckoutOperation{ID: operationID, CartID: cartID, SourceID: sourceID, SourceAnchor: sourceAnchor, StartedAt: time.Now().UTC(),
			Order: providers.ERPOrder{ExternalID: "paid-" + operationID, Items: items, TotalAmount: total,
				Observation: fmt.Sprintf("LiveCart - Evento %s - @%s | Carrinho %s | Substitui reserva Tiny %s", cart.EventID, cart.PlatformHandle, cartID, sourceID),
				Checkout:    &checkout, Shipping: checkout.Shipping, ShippingAddress: checkout.Address}}
		if err := journal.Save(ctx, op); err != nil {
			return "", err
		}
	}
	result, err := finalizer.FinalizePaidCheckout(ctx, op, journal)
	if err != nil {
		return "", err
	}
	return result.OrderID, nil
}

func (s *Service) loadTinyPaidCheckout(ctx context.Context, cartID, storeID string) (providers.ERPOrderCheckout, error) {
	out, err := s.LoadERPOrderCheckout(ctx, cartID, storeID)
	if err != nil {
		return out, err
	}
	if out.Address != nil {
		out.Address.RecipientName, out.Address.Document, out.Address.Phone = out.Customer.Name, out.Customer.CpfCnpj, out.Customer.Phone
	}
	rows, err := s.repo.pool.Query(ctx, `
 SELECT cp.amount_cents,cp.method,cp.checkout_id,cp.paid_at,
        COALESCE(op.gateway_snapshot,'{}'::jsonb),COALESCE(op.card_snapshot,'{}'::jsonb),
        COALESCE(c.card_installments,0),
        (SELECT count(*) FROM cart_payments sibling WHERE sibling.cart_id=c.id AND sibling.method='credit_card')
 FROM cart_payments cp JOIN carts c ON c.id=cp.cart_id
 LEFT JOIN orders o ON o.cart_id=c.id LEFT JOIN order_payments op ON op.order_id=o.id
 WHERE COALESCE(c.joined_to_cart_id,c.id)=$1 ORDER BY cp.paid_at,cp.created_at`, cartID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Payments = nil
	for rows.Next() {
		var pay providers.ERPOrderPayment
		var gateway, card []byte
		var cardCount, legacyInstallments int
		if err := rows.Scan(&pay.Amount, &pay.Method, &pay.PaymentID, &pay.PaidAt, &gateway, &card, &legacyInstallments, &cardCount); err != nil {
			return out, err
		}
		if pay.Method == "credit_card" {
			var status providers.PaymentStatus
			if err := json.Unmarshal(gateway, &status); err != nil {
				return out, fmt.Errorf("reading Tiny gateway schedule: %w", err)
			}
			if status.PaymentID == pay.PaymentID || cardCount == 1 {
				pay.Installments, pay.MoneyReleaseDate = status.Installments, status.MoneyReleaseDate
				if pay.Installments == 0 {
					var snap struct {
						Installments int `json:"installments"`
					}
					if err := json.Unmarshal(card, &snap); err != nil {
						return out, err
					}
					pay.Installments = snap.Installments
				}
				if pay.Installments == 0 && cardCount == 1 {
					pay.Installments = legacyInstallments
				}
			}
			if pay.Installments < 1 {
				return out, fmt.Errorf("tiny: parcelamento do cartão %s não identificado", pay.PaymentID)
			}
		}
		installments, err := providererp.TinyPaidInstallments(&pay)
		if err != nil {
			return out, err
		}
		out.Payments = append(out.Payments, installments...)
	}
	return out, rows.Err()
}

func (s *Service) LoadTinyPaidInstallments(ctx context.Context, cartID, storeID string) ([]providers.ERPInstallment, error) {
	checkout, err := s.loadTinyPaidCheckout(ctx, cartID, storeID)
	return checkout.Payments, err
}

type tinyCheckoutJournal struct {
	repo                                     *Repository
	cartID, storeID, integrationID, sourceID string
}

func (j *tinyCheckoutJournal) load(ctx context.Context) (*providers.TinyCheckoutOperation, error) {
	var raw []byte
	err := j.repo.pool.QueryRow(ctx, `SELECT progress FROM tiny_checkout_operations
 WHERE cart_id=$1 AND integration_id=$2
 AND (NOT completed OR source_order_id=$3 OR target_order_id=$3)
 ORDER BY completed,created_at DESC LIMIT 1`, j.cartID, j.integrationID, j.sourceID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var op providers.TinyCheckoutOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, err
	}
	return &op, nil
}

func (j *tinyCheckoutJournal) Save(ctx context.Context, op *providers.TinyCheckoutOperation) error {
	if op.CartID != j.cartID {
		return fmt.Errorf("tiny checkout journal cart mismatch")
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	result, err := j.repo.pool.Exec(ctx, `
 INSERT INTO tiny_checkout_operations(id,cart_id,integration_id,source_order_id,target_order_id,progress,completed)
 SELECT $1,$2,$3,$4,NULLIF($5,''),$6,$7 FROM carts c JOIN live_events e ON e.id=c.event_id
 JOIN integrations i ON i.id=$3 AND i.store_id=e.store_id AND i.provider='tiny'
 WHERE c.id=$2 AND e.store_id=$8
 ON CONFLICT(id) DO UPDATE SET target_order_id=EXCLUDED.target_order_id,
 progress=EXCLUDED.progress,completed=EXCLUDED.completed,updated_at=now()
 WHERE tiny_checkout_operations.cart_id=EXCLUDED.cart_id
 AND tiny_checkout_operations.integration_id=EXCLUDED.integration_id`, op.ID, j.cartID, j.integrationID, op.SourceID, op.TargetID, raw, op.Completed, j.storeID)
	if err != nil {
		return fmt.Errorf("saving Tiny checkout checkpoint: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("tiny checkout checkpoint owner mismatch")
	}
	return nil
}

func (j *tinyCheckoutJournal) Bind(ctx context.Context, op *providers.TinyCheckoutOperation) error {
	tx, err := j.repo.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := tx.Exec(ctx, `UPDATE carts c SET external_order_id=$1,erp_order_number=NULLIF($2,''),
 erp_order_status='aprovado',erp_order_status_at=now()
 FROM live_events e WHERE c.id=$3 AND e.id=c.event_id AND e.store_id=$4
 AND c.erp_order_state='mutating' AND (c.external_order_id=$5 OR c.external_order_id=$1)`, op.TargetID, op.TargetNumber, j.cartID, j.storeID, op.SourceID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("tiny checkout lost cart claim before binding")
	}
	if _, err := tx.Exec(ctx, `UPDATE order_payments p SET external_order_id=$1 FROM orders o WHERE o.id=p.order_id AND o.cart_id=$2 AND o.store_id=$3`, op.TargetID, j.cartID, j.storeID); err != nil {
		return err
	}
	op.Completed = true
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	result, err = tx.Exec(ctx, `UPDATE tiny_checkout_operations SET progress=$1,completed=true,target_order_id=$2,updated_at=now() WHERE id=$3 AND cart_id=$4 AND integration_id=$5`, raw, op.TargetID, op.ID, j.cartID, j.integrationID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("tiny checkout checkpoint missing before binding")
	}
	return tx.Commit(ctx)
}

func tinySameSnapshot(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}
