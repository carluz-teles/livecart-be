package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// ErrCartNotPayable sinaliza que uma escrita de pagamento foi recusada pelo
// guard da query (cart 'expired'/'cancelled') — 0 rows. O caller deve tratar
// como skip benigno (não finalizar), não como falha do webhook.
//
// B1d: o sentinel foi MOVIDO do integration.Repository para cá porque o
// consumidor canônico é o webhook de pagamento (ProcessPaymentNotification), que
// agora vive neste pacote. O integration.Repository.UpdateCartPaymentStatus
// retorna este mesmo valor (referenciando payment.ErrCartNotPayable) — fonte
// única, sem tradução entre pacotes.
var ErrCartNotPayable = errors.New("cart not payable (expired or cancelled)")

// ProcessPaymentInput is the payment webhook consumer input. It mirrors
// integration.ProcessPaymentInput field-for-field (same names ⇒ same JSON keys)
// so the payment.process command payload stays wire-compatible: the integration
// webhook edge emits it and this package consumes it without a schema change.
type ProcessPaymentInput struct {
	StoreID   string
	Provider  string
	PaymentID string
}

// CartPaymentGateway is the slice of the integration monolith that the payment
// webhook consumer needs. Declared here — in the consumer package — and
// implemented by *integration.Service so the webhook runs against the SAME
// integration.Repository (same pool, same guarded UpdateCartPaymentStatus, same
// outbox) as ExpireCart. Nothing about the payment×expiration serialization
// moves: this port only forwards to the existing repo/service. Keeping the
// interface on this side lets integration depend on payment (delegations) with
// no import cycle — it speaks only in leaf types (providers, events, primitives).
type CartPaymentGateway interface {
	// ResolvePaymentProvider resolves the store's active payment provider for the
	// webhook path (by store + provider name) and returns it together with the
	// integration id used for provider-error reporting.
	ResolvePaymentProvider(ctx context.Context, storeID, provider string) (pp providers.PaymentProvider, integrationID string, err error)

	// HandleProviderError flags the integration unhealthy on provider failures
	// (shared with the fast-path resolver — same method on integration.Service).
	HandleProviderError(ctx context.Context, integrationID, operation string, err error)

	// CartPaymentStatus returns the cart's current payment status (empty string
	// when the cart no longer exists). Used to promote a provider "cancelled" on
	// an already-paid cart to a refund.
	CartPaymentStatus(ctx context.Context, cartID string) (string, error)

	// UpdateCartPaymentStatus applies the guarded cart payment write. It returns
	// ErrCartNotPayable when the cart expired/cancelled between the charge and the
	// webhook — the guard that serializes this consumer against ExpireCart.
	UpdateCartPaymentStatus(ctx context.Context, cartID, paymentStatus, paymentID string, paidAt *time.Time, paymentMethod string) error

	// RestoreCancelledCartAsPaid handles the inverse race (LIV-84): a MANUAL
	// store cancellation that a payment then won. When UpdateCartPaymentStatus
	// rejects a 'paid' write with ErrCartNotPayable, this restores the cancelled
	// cart to paid in a single tx (retomando o estoque devolvido) so the normal
	// fan-out runs — o dinheiro manda. Returns restored=false when the cart is
	// not a store-cancelled cart pending payment (the caller then does the benign
	// ErrCartNotPayable skip).
	RestoreCancelledCartAsPaid(ctx context.Context, cartID, storeID, paymentStatus, paymentID string, paidAt *time.Time, paymentMethod string) (restored bool, err error)

	// CartGMVCents returns the pure item sum (excludes shipping and coupon) for
	// the cart — the single source of truth. A malformed cart id yields (0, nil);
	// a query failure yields (0, err) so the caller can warn and emit gmv=0.
	CartGMVCents(ctx context.Context, cartID string) (int64, error)

	// EmitEvent emits a domain fact on the transactional outbox (events.Emit).
	EmitEvent(ctx context.Context, env events.Envelope) error

	// EmitInternalCommand emits an internal command on the outbox
	// (events.EmitInternal) — used by DispatchPaymentProcess.
	EmitInternalCommand(ctx context.Context, name events.Name, dedupKey string, payload any) error

	// OnCartCancelled runs the inline payment-cancel email hook. cart.cancelled
	// has a second producer (blocked-handle cancel) with a different intent, so
	// its email can't be shared as a reactor — it stays inline here.
	OnCartCancelled(ctx context.Context, cartID string)
}

// cartPaymentFact is the specific-fact strategy: the resolved cart payment
// status → the canonical fact the reactors subscribe to. paid/refunded carry the
// fan-out (cart.paid/cart.refunded reactors); failed/cancelled are terminal facts
// (cancelled's email stays inline — shared-producer). pending emits nothing
// (deprecated telemetry). Refund vs chargeback stay conflated in cart.refunded
// until the provider status exposes the dispute type.
var cartPaymentFact = map[string]events.Name{
	"paid":      events.CartPaid,
	"refunded":  events.CartRefunded,
	"failed":    events.PaymentFailed,
	"cancelled": events.CartCancelled,
}

// SetCartPaymentGateway wires the integration-backed gateway. Mirrors the
// integration.Service.SetPaymentService setter used for the reverse wiring: the
// gateway is set at boot immediately after NewService (main.newApp). Kept a
// setter (not a constructor param) so the existing fast-path tests that build a
// Service without a gateway are unaffected.
func (s *Service) SetCartPaymentGateway(gw CartPaymentGateway) {
	s.gateway = gw
}

// DispatchPaymentProcess is the thin webhook edge (L1 of the event choreography):
// it emits a payment.process COMMAND to the transactional outbox instead of
// running the reconciliation in a detached goroutine. The command consumer
// (main.newApp) runs ProcessPaymentNotification with asynq retry + dead-letter,
// and the outbox makes it crash-durable. dedup_key is empty on purpose — see the
// events.PaymentProcess doc.
func (s *Service) DispatchPaymentProcess(ctx context.Context, input ProcessPaymentInput) error {
	if s.gateway == nil {
		return fmt.Errorf("payment: cart payment gateway not wired")
	}
	return s.gateway.EmitInternalCommand(ctx, events.PaymentProcess, "", input)
}

// ProcessPaymentNotification processes a payment webhook notification (L2 of the
// choreography). It re-queries the gateway for the authoritative status, applies
// the guarded cart payment write (UpdateCartPaymentStatus — serialized against
// ExpireCart), then emits the canonical cart payment fact (cart.paid/cart.refunded/
// …) deduplicated by payment_id so the provider's at-least-once burst collapses.
// The fan-out (coupon, order/GMV/email/waitlist, billing) and ERP finalisation
// run as reactors of those facts, registered in main.newApp.
func (s *Service) ProcessPaymentNotification(ctx context.Context, input ProcessPaymentInput) error {
	if s.gateway == nil {
		return fmt.Errorf("payment: cart payment gateway not wired")
	}

	paymentProvider, integrationID, err := s.gateway.ResolvePaymentProvider(ctx, input.StoreID, input.Provider)
	if err != nil {
		return err
	}

	status, err := paymentProvider.GetPaymentStatus(ctx, input.PaymentID)
	if err != nil {
		s.gateway.HandleProviderError(ctx, integrationID, "process_payment_notification", err)
		return fmt.Errorf("getting payment status: %w", err)
	}

	logger.From(ctx, s.logger).Info("payment notification processed",
		zap.String("payment_id", input.PaymentID),
		zap.String("status", string(status.Status)),
		zap.String("external_reference", status.ExternalReference),
	)

	// ExternalReference contains the cart ID (set when creating checkout)
	if status.ExternalReference == "" {
		logger.From(ctx, s.logger).Warn("payment notification has no external reference, cannot update cart",
			zap.String("payment_id", input.PaymentID),
		)
		return nil
	}

	// Map payment status to cart payment status
	var cartPaymentStatus string
	switch status.Status {
	case providers.PaymentApproved:
		cartPaymentStatus = "paid"
	case providers.PaymentRejected:
		cartPaymentStatus = "failed"
	case providers.PaymentCancelled:
		cartPaymentStatus = "cancelled"
		// Cart JÁ PAGO cuja cobrança foi cancelada = dinheiro devolvido =
		// estorno. O Pagar.me responde "canceled" ao refund de charge não
		// liquidada (praticamente todo estorno same-day) — sem esta
		// transição os hooks de estorno (devolução da taxa de sucesso,
		// e-mail, ERP) nunca rodam nesses casos. "refunded" é terminal: o
		// estorno dispara uma rajada de webhooks (charge.refunded +
		// order.canceled) e o segundo não pode rebaixar o estado.
		if st, cErr := s.gateway.CartPaymentStatus(ctx, status.ExternalReference); cErr == nil &&
			(st == "paid" || st == "refunded") {
			cartPaymentStatus = "refunded"
		}
	case providers.PaymentRefunded:
		cartPaymentStatus = "refunded"
	case providers.PaymentPending, providers.PaymentInProcess:
		cartPaymentStatus = "pending"
	default:
		cartPaymentStatus = "pending"
	}

	// Update cart payment status and payment method
	if err := s.gateway.UpdateCartPaymentStatus(ctx, status.ExternalReference, cartPaymentStatus, status.PaymentID, status.PaidAt, status.PaymentMethod); err != nil {
		if errors.Is(err, ErrCartNotPayable) {
			// Corrida cancelamento × pagamento, lado inverso (LIV-84): o lojista
			// cancelou o carrinho e o pagamento foi aprovado assim mesmo (PIX pago
			// com o QR já aberto, cartão em análise, webhook atrasado). Regra de
			// negócio: **o pagamento vence** — o pedido volta e consta como PAGO.
			// Só vale para o cancelamento MANUAL do lojista: expirado ou bloqueado
			// por handle não são restaurados (o guard da query decide).
			restored := false
			if cartPaymentStatus == "paid" {
				var restoreErr error
				restored, restoreErr = s.gateway.RestoreCancelledCartAsPaid(ctx,
					status.ExternalReference, input.StoreID, cartPaymentStatus, status.PaymentID, status.PaidAt, status.PaymentMethod)
				if restoreErr != nil {
					logger.From(ctx, s.logger).Error("failed to restore store-cancelled cart as paid",
						zap.String("cart_id", status.ExternalReference), zap.Error(restoreErr))
					return fmt.Errorf("restoring cancelled cart as paid: %w", restoreErr)
				}
			}
			if !restored {
				// O cart expirou/cancelou entre a cobrança e este webhook e não é
				// um cancelamento de lojista restaurável. Não finalizamos (não marca
				// pago, não toca ERP). Se dinheiro entrou mesmo, fica para a
				// reconciliação (E6). ACK benigno.
				logger.From(ctx, s.logger).Info("payment webhook for cart no longer payable (expired/cancelled), skipping finalization",
					zap.String("cart_id", status.ExternalReference),
					zap.String("payment_status", cartPaymentStatus),
				)
				return nil
			}
			// Restaurado: o fluxo segue idêntico ao de um pagamento normal — emite
			// cart.paid e o fan-out (Order, ERP, e-mail, cupom, billing) roda como se
			// o cancelamento nunca tivesse acontecido. As colunas de ERP foram
			// zeradas no restore, então a finalização cria um pedido de venda limpo.
			logger.From(ctx, s.logger).Warn("store-cancelled cart was PAID — cancellation reverted, order is paid",
				zap.String("cart_id", status.ExternalReference),
				zap.String("payment_id", status.PaymentID),
			)
		} else {
			logger.From(ctx, s.logger).Error("failed to update cart payment status",
				zap.String("cart_id", status.ExternalReference),
				zap.String("payment_status", cartPaymentStatus),
				zap.Error(err),
			)
			return fmt.Errorf("updating cart payment status: %w", err)
		}
	}

	logger.From(ctx, s.logger).Info("cart payment status updated",
		zap.String("cart_id", status.ExternalReference),
		zap.String("payment_status", cartPaymentStatus),
		zap.String("payment_method", status.PaymentMethod),
	)

	// Emit the canonical CART payment fact (specific-fact strategy — L3). It only
	// fires when the guarded UpdateCartPaymentStatus actually held. The fan-out
	// (coupon, order/GMV/email/waitlist, billing) now lives in REACTORS that
	// consume cart.paid / cart.refunded (registered in main.newApp) — decoupled
	// and retriable — instead of the inline cascade this method used to run.
	// dedup by payment_id so the provider's at-least-once burst collapses.
	if name, ok := cartPaymentFact[cartPaymentStatus]; ok {
		// The paid fact carries the FRESH gateway snapshot (installments, fees,
		// money-release date); OnCartPaid freezes it into the order.paid payload so
		// record the payment in Tiny without re-fetching — the same data the
		// resumable state machine persists to carts.erp_payment_snapshot for retry.
		var snap *providers.PaymentStatus
		if cartPaymentStatus == "paid" {
			snap = status
		}
		// gmv_cents = soma pura de itens (exclui frete e cupom) — fonte única de
		// verdade via CartGMVCents. Falha → emite com 0 (receptor usa fallback).
		var gmvCents int64
		if cartPaymentStatus == "paid" {
			if v, err := s.gateway.CartGMVCents(ctx, status.ExternalReference); err == nil {
				gmvCents = v
			} else {
				logger.From(ctx, s.logger).Warn("cart.paid: CartGMVCents failed, emitting gmv_cents=0",
					zap.String("cart_id", status.ExternalReference), zap.Error(err))
			}
		}
		payload, _ := json.Marshal(struct {
			CartID          string                   `json:"cart_id"`
			StoreID         string                   `json:"store_id"`
			PaymentID       string                   `json:"payment_id"`
			Method          string                   `json:"payment_method"`
			GMVCents        int64                    `json:"gmv_cents,omitempty"`
			PaymentSnapshot *providers.PaymentStatus `json:"payment_snapshot,omitempty"`
		}{status.ExternalReference, input.StoreID, status.PaymentID, status.PaymentMethod, gmvCents, snap})
		_ = s.gateway.EmitEvent(ctx, events.Envelope{
			Name:     name,
			Source:   events.Source(input.Provider),
			DedupKey: string(name) + ":" + status.PaymentID,
			Payload:  payload,
		})
	}

	// cart.cancelled has a SECOND producer (blocked-handle cancel) with a
	// different intent, so its reactor can't be shared safely — keep the
	// payment-cancel email inline here. The nil-hook guard lives in the adapter.
	if cartPaymentStatus == "cancelled" {
		s.gateway.OnCartCancelled(ctx, status.ExternalReference)
	}

	// ERP now runs entirely as event reactors: finalisation on order.paid
	// (ReactOrderPaidERP — the gateway snapshot rides the order.paid payload frozen
	// by OnCartPaid), refund on order.refunded (ReactOrderRefundedERP), reversal on
	// cart.expired (ReactCartExpiredERP — pre-payment reservation, no Order). They
	// get asynq retry + DLQ instead of the swallowed inline best-effort this method
	// used to run.
	return nil
}
