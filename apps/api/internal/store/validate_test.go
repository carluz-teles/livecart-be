package store

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// hasFieldErr reports whether err is a validation.Errors carrying an entry for
// the given json field key. Returns false for non-validation errors.
func hasFieldErr(err error, field string) bool {
	verrs, ok := err.(validation.Errors)
	if !ok {
		return false
	}
	_, exists := verrs[field]
	return exists
}

func intPtr(i int) *int { return &i }

func TestCreateStoreRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     CreateStoreRequest
		wantErr bool
		field   string // json key expected in the error (optional)
	}{
		{"valid", CreateStoreRequest{Name: "My Store", Slug: "my-store"}, false, ""},

		// Name rules.
		{"name required", CreateStoreRequest{Name: "", Slug: "my-store"}, true, "name"},
		{"name too short", CreateStoreRequest{Name: "A", Slug: "my-store"}, true, "name"},
		{"name too long", CreateStoreRequest{Name: makeRunes('a', 101), Slug: "my-store"}, true, "name"},

		// Slug rules.
		{"slug required", CreateStoreRequest{Name: "My Store", Slug: ""}, true, "slug"},
		{"slug too short", CreateStoreRequest{Name: "My Store", Slug: "a"}, true, "slug"},
		{"slug too long", CreateStoreRequest{Name: "My Store", Slug: makeRunes('a', 51)}, true, "slug"},
		{"slug invalid format uppercase", CreateStoreRequest{Name: "My Store", Slug: "My-Store"}, true, "slug"},
		{"slug invalid format leading hyphen", CreateStoreRequest{Name: "My Store", Slug: "-store"}, true, "slug"},
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
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

// makeRunes returns n copies of r as a string, used to build over-length inputs.
func makeRunes(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func TestUpdateStoreRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     UpdateStoreRequest
		wantErr bool
		field   string
	}{
		{"valid", UpdateStoreRequest{Name: "My Store"}, false, ""},

		// Name is the only validated field.
		{"name required", UpdateStoreRequest{Name: ""}, true, "name"},
		{"name too short", UpdateStoreRequest{Name: "A"}, true, "name"},
		{"name too long", UpdateStoreRequest{Name: makeRunes('a', 101)}, true, "name"},
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
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

func TestUpdateShippingDefaultsRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     UpdateShippingDefaultsRequest
		wantErr bool
		field   string
	}{
		{"valid with dims", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "box",
			HeightCm: intPtr(10), WidthCm: intPtr(20), LengthCm: intPtr(30),
		}, false, ""},
		{"valid without dims (nil pointers skip Min)", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 0, PackageFormat: "roll",
		}, false, ""},

		// PackageWeightGrams Min(0).
		{"weight negative", UpdateShippingDefaultsRequest{
			PackageWeightGrams: -1, PackageFormat: "box",
		}, true, "packageWeightGrams"},

		// PackageFormat Required + In.
		{"format required", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "",
		}, true, "packageFormat"},
		{"format not in enum", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "envelope",
		}, true, "packageFormat"},

		// Dimension Min(1) — pointer to a non-empty value below the minimum.
		// ozzo's Min skips the zero value (treated as empty), so use negatives.
		{"height below min", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "box", HeightCm: intPtr(-1),
		}, true, "heightCm"},
		{"width below min", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "box", WidthCm: intPtr(-1),
		}, true, "widthCm"},
		{"length below min", UpdateShippingDefaultsRequest{
			PackageWeightGrams: 100, PackageFormat: "box", LengthCm: intPtr(-1),
		}, true, "lengthCm"},
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
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}

func TestUpdateCartSettingsRequestValidate(t *testing.T) {
	// valid baseline: all bounded fields inside range, reminder toggle ON so the
	// conditional ExpirationReminderMinutes rule is exercised.
	valid := UpdateCartSettingsRequest{
		ExpirationMinutes:         30,
		MaxQuantityPerItem:        5,
		MessageCooldownSeconds:    60,
		SendExpirationReminder:    true,
		ExpirationReminderMinutes: 10,
	}

	cases := []struct {
		name    string
		mutate  func(r *UpdateCartSettingsRequest)
		wantErr bool
		field   string
	}{
		{"valid", func(r *UpdateCartSettingsRequest) {}, false, ""},

		// ExpirationMinutes Required + Min(15) + Max(1440). Required makes the
		// zero value (omitted field) fail instead of silently slipping the floor.
		// O piso 15 espelha o CHECK da migration 000104 — antes dela o mínimo
		// era 5, e 5..14 passavam aqui para estourar no banco.
		{"expiration zero rejected", func(r *UpdateCartSettingsRequest) { r.ExpirationMinutes = 0 }, true, "expirationMinutes"},
		{"expiration below min", func(r *UpdateCartSettingsRequest) { r.ExpirationMinutes = 14 }, true, "expirationMinutes"},
		{"expiration at old floor now rejected", func(r *UpdateCartSettingsRequest) { r.ExpirationMinutes = 5 }, true, "expirationMinutes"},
		{"expiration at new floor accepted", func(r *UpdateCartSettingsRequest) { r.ExpirationMinutes = 15 }, false, ""},
		{"expiration above max", func(r *UpdateCartSettingsRequest) { r.ExpirationMinutes = 1441 }, true, "expirationMinutes"},

		// MaxQuantityPerItem Required + Min(1): zero is now rejected, not skipped.
		{"maxQuantity zero rejected", func(r *UpdateCartSettingsRequest) { r.MaxQuantityPerItem = 0 }, true, "maxQuantityPerItem"},
		{"maxQuantity below min", func(r *UpdateCartSettingsRequest) { r.MaxQuantityPerItem = -1 }, true, "maxQuantityPerItem"},

		// MessageCooldownSeconds Min(0) Max(300).
		{"cooldown below min", func(r *UpdateCartSettingsRequest) { r.MessageCooldownSeconds = -1 }, true, "messageCooldownSeconds"},
		{"cooldown above max", func(r *UpdateCartSettingsRequest) { r.MessageCooldownSeconds = 301 }, true, "messageCooldownSeconds"},

		// ExpirationReminderMinutes is conditionally required: enforced only when
		// SendExpirationReminder is on.
		{"reminder zero rejected when toggle on", func(r *UpdateCartSettingsRequest) { r.ExpirationReminderMinutes = 0 }, true, "expirationReminderMinutes"},
		{"reminder below min", func(r *UpdateCartSettingsRequest) { r.ExpirationReminderMinutes = -1 }, true, "expirationReminderMinutes"},
		{"reminder above max", func(r *UpdateCartSettingsRequest) { r.ExpirationReminderMinutes = 61 }, true, "expirationReminderMinutes"},
		// Toggle off: reminder minutes irrelevant, zero must pass.
		{"reminder zero skipped when toggle off", func(r *UpdateCartSettingsRequest) {
			r.SendExpirationReminder = false
			r.ExpirationReminderMinutes = 0
		}, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			err := req.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
			if tc.wantErr && tc.field != "" && !hasFieldErr(err, tc.field) {
				t.Fatalf("expected error on field %q, got %v", tc.field, err)
			}
		})
	}
}
