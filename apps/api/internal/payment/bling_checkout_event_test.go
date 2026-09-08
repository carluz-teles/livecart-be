package payment_test

import (
	"context"
	"encoding/json"
	"testing"

	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
)

func TestSupplementalPaymentEmitsAnotherCartPaidFact(t *testing.T) {
	t.Parallel()
	const cartID = "11111111-1111-1111-1111-111111111111"
	gw := newMockGateway()
	gw.cartStatus = "paid"
	svc := newWebhookService(gw)
	for _, paymentID := range []string{"first-charge", "shipping-charge", "shipping-charge"} {
		gw.provider = stubPaymentProvider{statusResult: &providers.PaymentStatus{
			Status: providers.PaymentApproved, ExternalReference: cartID,
			PaymentID: paymentID, PaymentMethod: "pix", Amount: 900,
		}}
		if err := svc.ProcessPaymentNotification(context.Background(), payment.ProcessPaymentInput{StoreID: "store-1", Provider: "pagarme", PaymentID: paymentID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(gw.emitOrder) != 2 {
		t.Fatalf("expected one fact per payment, with redelivery deduplicated: %v", gw.emitOrder)
	}
	for _, paymentID := range []string{"first-charge", "shipping-charge"} {
		fact, ok := gw.emittedByKey[string(events.CartPaid)+":"+paymentID]
		if !ok {
			t.Fatalf("missing independent cart.paid fact for %s", paymentID)
		}
		var payload struct {
			CartID    string `json:"cart_id"`
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(fact.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.CartID != cartID || payload.PaymentID != paymentID {
			t.Fatalf("invalid supplemental fact: %+v", payload)
		}
	}
}
