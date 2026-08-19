package coupon

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldErr reports whether err is a validation.Errors carrying the given
// json field key. Returns false if err is nil or not a validation.Errors.
func hasFieldErr(err error, field string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, present := verrs[field]
	return present
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }
func typePtr(v Type) *Type    { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestCreateRequestValidate(t *testing.T) {
	// valid baseline: Code within Length(2,40), a valid Type, non-negative
	// numeric fields, MaxUses omitted (nil pointer is skipped by ozzo).
	valid := func() CreateRequest {
		return CreateRequest{
			Code:             "SAVE10",
			Type:             TypePercent,
			ValueCents:       0,
			PercentBPS:       1000,
			MaxUses:          intPtr(5),
			MinPurchaseCents: 0,
			Active:           true,
		}
	}

	cases := []struct {
		name     string
		mutate   func(r *CreateRequest)
		wantErr  bool
		errField string
	}{
		// exactly one valid case
		{"valid", func(r *CreateRequest) {}, false, ""},

		// Code — Required
		{"code required", func(r *CreateRequest) { r.Code = "" }, true, "code"},
		// Code — Length min (2)
		{"code too short", func(r *CreateRequest) { r.Code = "X" }, true, "code"},
		// Code — Length max (40)
		{"code too long", func(r *CreateRequest) {
			r.Code = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX" // 41 chars
		}, true, "code"},

		// Type — Required
		{"type required", func(r *CreateRequest) { r.Type = "" }, true, "type"},
		// Type — In
		{"type not in set", func(r *CreateRequest) { r.Type = Type("bogus") }, true, "type"},

		// ValueCents — Min(0)
		{"valueCents negative", func(r *CreateRequest) { r.ValueCents = -1 }, true, "valueCents"},

		// PercentBPS — Min(0)
		{"percentBps negative", func(r *CreateRequest) { r.PercentBPS = -1 }, true, "percentBps"},
		// PercentBPS — Max(10000)
		{"percentBps over max", func(r *CreateRequest) { r.PercentBPS = 10001 }, true, "percentBps"},

		// MaxUses — Min(1). NB: ozzo treats a pointer-to-0 as an empty value
		// and skips the rule (no Required), so only a negative value trips it.
		{"maxUses below min", func(r *CreateRequest) { r.MaxUses = intPtr(-1) }, true, "maxUses"},

		// MinPurchaseCents — Min(0)
		{"minPurchaseCents negative", func(r *CreateRequest) { r.MinPurchaseCents = -1 }, true, "minPurchaseCents"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			err := r.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.errField != "" && !hasFieldErr(err, tc.errField) {
				t.Fatalf("expected error on field %q, got %v", tc.errField, err)
			}
		})
	}
}

func TestUpdateRequestValidate(t *testing.T) {
	// valid baseline: every field present and within bounds. All-nil is also
	// valid (partial update) — covered as its own case.
	valid := func() UpdateRequest {
		return UpdateRequest{
			Type:             typePtr(TypeFixed),
			ValueCents:       int64Ptr(500),
			PercentBPS:       intPtr(0),
			MaxUses:          intPtr(1),
			MinPurchaseCents: int64Ptr(0),
			Active:           boolPtr(false),
		}
	}

	cases := []struct {
		name     string
		req      UpdateRequest
		wantErr  bool
		errField string
	}{
		// exactly one valid case (all fields present, within bounds)
		{"valid all present", valid(), false, ""},
		// also valid: all-nil partial update (rules skipped)
		{"valid all nil", UpdateRequest{}, false, ""},

		// Type — In
		{"type not in set", UpdateRequest{Type: typePtr(Type("bogus"))}, true, "type"},

		// ValueCents — Min(0)
		{"valueCents negative", UpdateRequest{ValueCents: int64Ptr(-1)}, true, "valueCents"},

		// PercentBPS — Min(0)
		{"percentBps negative", UpdateRequest{PercentBPS: intPtr(-1)}, true, "percentBps"},
		// PercentBPS — Max(10000)
		{"percentBps over max", UpdateRequest{PercentBPS: intPtr(10001)}, true, "percentBps"},

		// MaxUses — Min(1). NB: pointer-to-0 is treated as empty by ozzo and
		// skipped; only a negative value trips Min.
		{"maxUses below min", UpdateRequest{MaxUses: intPtr(-1)}, true, "maxUses"},

		// MinPurchaseCents — Min(0)
		{"minPurchaseCents negative", UpdateRequest{MinPurchaseCents: int64Ptr(-1)}, true, "minPurchaseCents"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.errField != "" && !hasFieldErr(err, tc.errField) {
				t.Fatalf("expected error on field %q, got %v", tc.errField, err)
			}
		})
	}
}

func TestApplyCouponRequestValidate(t *testing.T) {
	cases := []struct {
		name     string
		req      ApplyCouponRequest
		wantErr  bool
		errField string
	}{
		// exactly one valid case
		{"valid", ApplyCouponRequest{Code: "SAVE10"}, false, ""},
		// Code — Required
		{"code required", ApplyCouponRequest{Code: ""}, true, "code"},
		// Code — Length min (2)
		{"code too short", ApplyCouponRequest{Code: "X"}, true, "code"},
		// Code — Length max (40)
		{"code too long", ApplyCouponRequest{
			Code: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAX", // 41 chars
		}, true, "code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.errField != "" && !hasFieldErr(err, tc.errField) {
				t.Fatalf("expected error on field %q, got %v", tc.errField, err)
			}
		})
	}
}
