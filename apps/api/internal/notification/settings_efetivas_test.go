package notification

// O GET de settings devolve a config EFETIVA (20/08/2026): chave nunca
// customizada vem preenchida com o default que o ENVIO já usa. Antes o GET
// omitia a chave e a tela de Comunicações mostrava "Pausada" para e-mails que
// enviavam normalmente — a UI e o envio liam verdades diferentes.

import (
	"encoding/json"
	"testing"
)

func TestConfigEfetivaPreencheChavesNuncaCustomizadas(t *testing.T) {
	// Loja antiga: só os três templates de carrinho existem no JSONB (o shape
	// da migration 000033) — nenhuma chave de e-mail, nenhuma da RN-28.
	raw := `{
		"checkout_immediate": {"enabled": false, "template": "custom"},
		"item_added": {"enabled": true, "template": "custom2"},
		"checkout_reminder": {"enabled": true, "template": "custom3"}
	}`
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	fillMissingWithDefaults(&s)

	// O que a loja customizou fica intocado — inclusive o desligado.
	if s.CheckoutImmediate == nil || s.CheckoutImmediate.Enabled || s.CheckoutImmediate.Template != "custom" {
		t.Errorf("customização da loja foi sobrescrita: %+v", s.CheckoutImmediate)
	}

	// E-mails pós-venda nunca customizados: vêm com o default LIGADO — é o
	// que o envio faz, então é o que a tela tem de mostrar.
	for name, e := range map[string]*EmailTemplateSettings{
		"payment_confirmed": s.PaymentConfirmed,
		"payment_cancelled": s.PaymentCancelled,
		"payment_refunded":  s.PaymentRefunded,
		"shipped":           s.Shipped,
		"delivered":         s.Delivered,
	} {
		if e == nil {
			t.Errorf("%s continua ausente — a tela voltaria a mostrar Pausada", name)
			continue
		}
		if !e.Enabled {
			t.Errorf("%s veio desligado; o envio usa default ligado", name)
		}
	}

	// Gatilhos da RN-28 idem.
	if s.WaitlistJoined == nil || s.OutOfWindowScheduled == nil {
		t.Errorf("gatilhos da RN-28 ausentes após o overlay")
	}
}
