//go:build integration

package checkout

import (
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/integration"
)

func TestQuoteShippingLogsCartIDWithoutCheckoutToken(t *testing.T) {
	f := seedMerchantEdit(t)
	core, logs := observer.New(zap.InfoLevel)
	f.service.logger = zap.New(core)
	f.service.integrationService = integration.NewService(
		integration.NewRepository(testQueries, testPool),
		nil,
		nil,
		nil,
		nil,
		zap.New(core),
	)

	result, err := f.service.QuoteShipping(t.Context(), QuoteShippingInput{Token: f.token, ZipCode: "01234567"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.NoShippingAvailable {
		t.Fatalf("manual freight behavior changed: %+v", result)
	}
	for _, message := range []string{
		"shipping quote: item missing dimensions, delivery skipped",
		"shipping quote: delivery not possible",
	} {
		entries := logs.FilterMessage(message).All()
		if len(entries) != 1 {
			t.Fatalf("expected one %q log, got %d", message, len(entries))
		}
		fields := entries[0].ContextMap()
		if fields["cart_id"] != f.cart || fields["store_id"] != f.store {
			t.Fatalf("missing safe correlation IDs: %v", fields)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), f.token) || strings.Contains(string(encoded), "cart_token") {
			t.Fatalf("checkout token was logged by %q", message)
		}
	}
}
