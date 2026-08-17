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

	got := NewLiveCommerceCartPayload("staging")

	if got.EventType != eventTypeLiveCommerceCart {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceCart)
	}
	if got.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", got.Environment, "staging")
	}
}

func TestNewLiveCommercePaymentPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommercePaymentPayload("staging")

	if got.EventType != eventTypeLiveCommercePayment {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommercePayment)
	}
	if got.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", got.Environment, "staging")
	}
}

func TestNewLiveCommerceEventPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceEventPayload("staging")

	if got.EventType != eventTypeLiveCommerceEvent {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceEvent)
	}
	if got.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", got.Environment, "staging")
	}
}

func TestNewLiveCommerceCartItemPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceCartItemPayload("staging")

	if got.EventType != eventTypeLiveCommerceCartItem {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceCartItem)
	}
	if got.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", got.Environment, "staging")
	}
}

func TestNewLiveCommerceCommentPayload(t *testing.T) {
	t.Parallel()

	got := NewLiveCommerceCommentPayload("staging")

	if got.EventType != eventTypeLiveCommerceComment {
		t.Errorf("EventType = %q, want %q", got.EventType, eventTypeLiveCommerceComment)
	}
	if got.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", got.Environment, "staging")
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

	payload := NewLiveCommercePaymentPayload("staging")
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

	payload := NewLiveCommercePaymentPayload("staging")

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	if strings.Contains(string(raw), "billable") {
		t.Errorf("marshaled payload = %s, want no \"billable\" attribute when unresolved", raw)
	}
}
