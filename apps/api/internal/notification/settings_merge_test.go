package notification

import "testing"

// TestMergeSettingsPreservesAbsentSections is the regression guard for the
// silent JSONB wipe: UpdateStoreNotificationSettings replaces the whole column,
// and UpdateSettingsRequest has no field for waitlist_notified or cart_recovery,
// so a save from the communications tab used to reset both.
func TestMergeSettingsPreservesAbsentSections(t *testing.T) {
	tpl := func(text string) *TemplateSettings {
		return &TemplateSettings{Enabled: true, Template: text}
	}

	// What the store has today.
	current := Settings{
		CheckoutImmediate: tpl("checkout antigo"),
		ItemAdded:         tpl("item antigo"),
		CheckoutReminder:  tpl("lembrete antigo"),
		WaitlistNotified:  tpl("fila antiga"),
		PaymentConfirmed:  &EmailTemplateSettings{Enabled: true, Subject: "pago"},
		PaymentCancelled:  &EmailTemplateSettings{Enabled: true, Subject: "cancelado"},
		PaymentRefunded:   &EmailTemplateSettings{Enabled: true, Subject: "estornado"},
		CartRecovery:      &CartRecoverySettings{Enabled: true, DelayMinutes: 30, MaxAttempts: 2},
	}

	cases := []struct {
		name     string
		incoming Settings
		assert   func(t *testing.T, got Settings)
	}{
		{
			// The exact shape the communications tab sends: three cart templates
			// and a couple of email ones, nothing else.
			name: "save from communications tab keeps waitlist and cart recovery",
			incoming: Settings{
				CheckoutImmediate: tpl("checkout novo"),
				ItemAdded:         tpl("item antigo"),
				CheckoutReminder:  tpl("lembrete antigo"),
				PaymentConfirmed:  &EmailTemplateSettings{Enabled: true, Subject: "pago"},
			},
			assert: func(t *testing.T, got Settings) {
				if got.CheckoutImmediate.Template != "checkout novo" {
					t.Errorf("checkout_immediate = %q, quero %q", got.CheckoutImmediate.Template, "checkout novo")
				}
				if got.WaitlistNotified == nil {
					t.Fatal("waitlist_notified foi apagado")
				}
				if got.WaitlistNotified.Template != "fila antiga" {
					t.Errorf("waitlist_notified = %q, quero preservado", got.WaitlistNotified.Template)
				}
				if got.CartRecovery == nil {
					t.Fatal("cart_recovery foi apagado")
				}
				if got.CartRecovery.DelayMinutes != 30 || got.CartRecovery.MaxAttempts != 2 {
					t.Errorf("cart_recovery = %+v, quero preservado", got.CartRecovery)
				}
				// The tab never sends these two — they must survive as well.
				if got.PaymentCancelled == nil || got.PaymentRefunded == nil {
					t.Error("payment_cancelled/payment_refunded foram apagados")
				}
			},
		},
		{
			name:     "empty payload changes nothing",
			incoming: Settings{},
			assert: func(t *testing.T, got Settings) {
				if got.CheckoutImmediate.Template != "checkout antigo" {
					t.Errorf("checkout_immediate = %q, quero intacto", got.CheckoutImmediate.Template)
				}
				if got.WaitlistNotified == nil || got.CartRecovery == nil {
					t.Error("payload vazio nao pode apagar secao nenhuma")
				}
			},
		},
		{
			name: "explicitly disabling a section is honoured",
			incoming: Settings{
				CheckoutReminder: &TemplateSettings{Enabled: false, Template: "lembrete antigo"},
			},
			assert: func(t *testing.T, got Settings) {
				if got.CheckoutReminder.Enabled {
					t.Error("checkout_reminder devia ter sido desligado")
				}
				if got.CheckoutImmediate.Template != "checkout antigo" {
					t.Error("desligar uma secao nao pode mexer nas outras")
				}
			},
		},
		{
			name: "waitlist and cart recovery are still writable when sent",
			incoming: Settings{
				WaitlistNotified: tpl("fila nova"),
				CartRecovery:     &CartRecoverySettings{Enabled: false, DelayMinutes: 90, MaxAttempts: 1},
			},
			assert: func(t *testing.T, got Settings) {
				if got.WaitlistNotified.Template != "fila nova" {
					t.Errorf("waitlist_notified = %q, quero %q", got.WaitlistNotified.Template, "fila nova")
				}
				if got.CartRecovery.DelayMinutes != 90 || got.CartRecovery.Enabled {
					t.Errorf("cart_recovery = %+v, quero sobrescrito", got.CartRecovery)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, mergeSettings(current, tc.incoming))
		})
	}
}

// TestMergeSettingsDoesNotMutateCurrent guards the caller's copy: UpdateSettings
// passes a dereferenced *Settings from GetSettings, and callers reuse it.
func TestMergeSettingsDoesNotMutateCurrent(t *testing.T) {
	current := Settings{
		CheckoutImmediate: &TemplateSettings{Enabled: true, Template: "original"},
		WaitlistNotified:  &TemplateSettings{Enabled: true, Template: "fila"},
	}

	_ = mergeSettings(current, Settings{
		CheckoutImmediate: &TemplateSettings{Enabled: false, Template: "novo"},
	})

	if current.CheckoutImmediate.Template != "original" {
		t.Errorf("current mutado: %q", current.CheckoutImmediate.Template)
	}
	if current.CheckoutImmediate.Enabled != true {
		t.Error("current.Enabled mutado")
	}
}
