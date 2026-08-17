package exporter

import (
	"encoding/json"
	"strings"
	"testing"
)

// boolPtr is a small test helper for populating LiveCommercePaymentPayload's
// pointer field Billable (shared with listeners_test.go).
func boolPtr(b bool) *bool {
	return &b
}

func TestNewLiveCommerceCartPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceCartPayload()

	if got.EventType != eventTypeLiveCommerceCart {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceCart)
	}
}

func TestNewLiveCommercePaymentPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommercePaymentPayload()

	if got.EventType != eventTypeLiveCommercePayment {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommercePayment)
	}
}

func TestNewLiveCommerceEventPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceEventPayload()

	if got.EventType != eventTypeLiveCommerceEvent {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceEvent)
	}
}

func TestNewLiveCommerceCartItemPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceCartItemPayload()

	if got.EventType != eventTypeLiveCommerceCartItem {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceCartItem)
	}
}

// TestLiveCommercePaymentPayload_BillableFalseIsSerialized proves billable=false
// (a real, legitimate business state — e.g. a trial-store sale that isn't
// billable) survives JSON serialization as a present "billable":false
// attribute, rather than being dropped by omitempty like a plain bool would.
// This guards the New Relic export from being indistinguishable between
// "billable resolved to false" and "billable enrichment never ran".
func TestLiveCommercePaymentPayload_BillableFalseIsSerialized(t *testing.T) {
	t.Parallel()

	payload := NewLiveCommercePaymentPayload()
	payload.Billable = boolPtr(false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	if !strings.Contains(string(raw), `"billable":false`) {
		t.Errorf("marshaled payload = %s, want it to contain \"billable\":false", raw)
	}
}

// TestLiveCommercePaymentPayload_BillableNilIsOmitted proves that when
// Billable is genuinely unresolved (nil — enrichment didn't run/failed), the
// attribute is omitted from the exported event rather than defaulting to a
// misleading false.
func TestLiveCommercePaymentPayload_BillableNilIsOmitted(t *testing.T) {
	t.Parallel()

	payload := NewLiveCommercePaymentPayload()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	if strings.Contains(string(raw), "billable") {
		t.Errorf("marshaled payload = %s, want no \"billable\" attribute when unresolved", raw)
	}
}
