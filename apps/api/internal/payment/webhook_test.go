package payment_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
)

// assertT is a minimal assertion helper (this package's tests avoid testify).
type assertT struct{ t *testing.T }

func newAssert(t *testing.T) assertT { return assertT{t} }

func (a assertT) noErr(err error) {
	a.t.Helper()
	if err != nil {
		a.t.Fatalf("unexpected error: %v", err)
	}
}

func (a assertT) eq(want, got any) {
	a.t.Helper()
	if !reflect.DeepEqual(want, got) {
		a.t.Fatalf("want %v, got %v", want, got)
	}
}

// mockGateway is a scripted payment.CartPaymentGateway. EmitEvent models the
// transactional outbox: it dedups by DedupKey (a Postgres unique constraint in
// production), so asserting the collapse here proves ProcessPaymentNotification
// hands a STABLE key across the provider's at-least-once redeliveries.
type mockGateway struct {
	provider      providers.PaymentProvider
	integrationID string
	resolveErr    error

	cartStatus    string
	cartStatusErr error

	updateErr   error
	updateCalls []string // captured cart payment statuses passed to the write

	restored     bool // RestoreCancelledCartAsPaid return
	restoreErr   error
	restoreCalls int

	gmv    int64
	gmvErr error

	emittedByKey map[string]events.Envelope
	emitOrder    []string
	internalCmds []string
	cancelled    []string
	handled      []string
}

func newMockGateway() *mockGateway {
	return &mockGateway{emittedByKey: map[string]events.Envelope{}}
}

func (m *mockGateway) ResolvePaymentProvider(_ context.Context, _, _ string) (providers.PaymentProvider, string, error) {
	return m.provider, m.integrationID, m.resolveErr
}

func (m *mockGateway) HandleProviderError(_ context.Context, _, operation string, _ error) {
	m.handled = append(m.handled, operation)
}

func (m *mockGateway) CartPaymentStatus(_ context.Context, _ string) (string, error) {
	return m.cartStatus, m.cartStatusErr
}

func (m *mockGateway) UpdateCartPaymentStatus(_ context.Context, _, paymentStatus, _ string, _ *time.Time, _ string) error {
	m.updateCalls = append(m.updateCalls, paymentStatus)
	return m.updateErr
}

func (m *mockGateway) RestoreCancelledCartAsPaid(_ context.Context, _, _, _, _ string, _ *time.Time, _ string) (bool, error) {
	m.restoreCalls++
	return m.restored, m.restoreErr
}

func (m *mockGateway) CartGMVCents(_ context.Context, _ string) (int64, error) {
	return m.gmv, m.gmvErr
}

func (m *mockGateway) EmitEvent(_ context.Context, env events.Envelope) error {
	if _, seen := m.emittedByKey[env.DedupKey]; !seen {
		m.emitOrder = append(m.emitOrder, env.DedupKey)
	}
	m.emittedByKey[env.DedupKey] = env // last-write-wins, same as an upsert-on-conflict
	return nil
}

func (m *mockGateway) EmitInternalCommand(_ context.Context, name events.Name, _ string, _ any) error {
	m.internalCmds = append(m.internalCmds, string(name))
	return nil
}

func (m *mockGateway) OnCartCancelled(_ context.Context, cartID string) {
	m.cancelled = append(m.cancelled, cartID)
}

// newWebhookService builds a payment.Service wired only with the gateway — the
// webhook consumer never touches the resolver/idempotency fields.
func newWebhookService(gw payment.CartPaymentGateway) *payment.Service {
	svc := payment.NewService(nil, nil, zap.NewNop())
	svc.SetCartPaymentGateway(gw)
	return svc
}

func TestService_ProcessPaymentNotification(t *testing.T) {
	t.Parallel()

	const (
		cartID    = "11111111-1111-1111-1111-111111111111"
		paymentID = "pay_123"
	)

	t.Run("paid emits cart.paid once with the payment-id dedup key", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status:            providers.PaymentApproved,
			ExternalReference: cartID,
			PaymentID:         paymentID,
			PaymentMethod:     "pix",
		}}
		gw.gmv = 4200
		svc := newWebhookService(gw)

		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{
			StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID,
		})
		is.noErr(err)
		is.eq(1, len(gw.emitOrder))
		env, ok := gw.emittedByKey[string(events.CartPaid)+":"+paymentID]
		if !ok {
			t.Fatalf("expected cart.paid emitted with key %q; got keys %v", string(events.CartPaid)+":"+paymentID, gw.emitOrder)
		}
		is.eq(string(events.CartPaid), string(env.Name))
		is.eq([]string{"paid"}, gw.updateCalls)
		is.eq(0, len(gw.cancelled))
	})

	t.Run("redelivery of the same payment id collapses to one fact", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status:            providers.PaymentApproved,
			ExternalReference: cartID,
			PaymentID:         paymentID,
			PaymentMethod:     "pix",
		}}
		svc := newWebhookService(gw)
		in := payment.ProcessPaymentInput{StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID}

		// Gateway burst: the provider delivers the same webhook twice.
		is.noErr(svc.ProcessPaymentNotification(context.Background(), in))
		is.noErr(svc.ProcessPaymentNotification(context.Background(), in))

		// Both deliveries carry the identical (stable) dedup key, so the outbox
		// collapses them into a single stored fact.
		is.eq(1, len(gw.emitOrder))
		is.eq(string(events.CartPaid)+":"+paymentID, gw.emitOrder[0])
	})

	t.Run("cart no longer payable skips the fact", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status:            providers.PaymentApproved,
			ExternalReference: cartID,
			PaymentID:         paymentID,
		}}
		gw.updateErr = payment.ErrCartNotPayable // expired/cancelled between charge and webhook
		svc := newWebhookService(gw)

		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{
			StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID,
		})
		is.noErr(err) // benign ACK
		is.eq(0, len(gw.emitOrder))
		is.eq([]string{"paid"}, gw.updateCalls) // the guarded write was attempted
		is.eq(1, gw.restoreCalls)               // tentou restaurar (paid), mas não era store-cancel → skip
	})

	t.Run("payment wins a store cancellation → cart restored and paid fact emitted (LIV-84)", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status:            providers.PaymentApproved,
			ExternalReference: cartID,
			PaymentID:         paymentID,
		}}
		gw.updateErr = payment.ErrCartNotPayable // lojista cancelou entre a cobrança e o webhook
		gw.restored = true                       // ...mas era cancelamento manual do lojista → o pagamento vence
		svc := newWebhookService(gw)

		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{
			StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID,
		})
		is.noErr(err)
		is.eq(1, gw.restoreCalls)
		// Restaurado: o fluxo segue como pagamento normal — cart.paid emitido.
		if _, ok := gw.emittedByKey[string(events.CartPaid)+":"+paymentID]; !ok {
			t.Fatalf("expected cart.paid emitted after restore; got %v", gw.emitOrder)
		}
	})

	t.Run("provider cancel on an already-paid cart is promoted to a refund", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status:            providers.PaymentCancelled,
			ExternalReference: cartID,
			PaymentID:         paymentID,
		}}
		gw.cartStatus = "paid" // the cart was already paid → cancel means money returned
		svc := newWebhookService(gw)

		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{
			StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID,
		})
		is.noErr(err)
		is.eq([]string{"refunded"}, gw.updateCalls)
		_, ok := gw.emittedByKey[string(events.CartRefunded)+":"+paymentID]
		if !ok {
			t.Fatalf("expected cart.refunded emitted; got %v", gw.emitOrder)
		}
		is.eq(0, len(gw.cancelled)) // promoted to refunded, so the cancel hook does NOT run
	})

	t.Run("provider status error reports and propagates", func(t *testing.T) {
		t.Parallel()
		is := newAssert(t)

		gw := newMockGateway()
		gw.provider = stubPaymentProvider{statusErr: errors.New("gateway 500")}
		svc := newWebhookService(gw)

		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{
			StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID,
		})
		if err == nil {
			t.Fatal("expected error from provider status failure")
		}
		is.eq([]string{"process_payment_notification"}, gw.handled)
		is.eq(0, len(gw.emitOrder))
	})

	t.Run("gateway not wired returns a clear error", func(t *testing.T) {
		t.Parallel()
		svc := payment.NewService(nil, nil, zap.NewNop()) // no SetCartPaymentGateway
		err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{})
		if err == nil {
			t.Fatal("expected error when gateway is not wired")
		}
	})
}

func TestService_DispatchPaymentProcess(t *testing.T) {
	t.Parallel()

	is := newAssert(t)
	gw := newMockGateway()
	svc := newWebhookService(gw)

	err := svc.DispatchPaymentProcess(context.Background(), payment.ProcessPaymentInput{
		StoreID: "store-1", Provider: "pagarme", PaymentID: "pay_9",
	})
	is.noErr(err)
	is.eq([]string{string(events.PaymentProcess)}, gw.internalCmds)
}
