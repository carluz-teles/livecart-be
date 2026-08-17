package notification

// Prova de ponta a ponta do merge parcial: grava no JSONB real via
// Service.UpdateSettings e relê a coluna crua. O teste unitário de mergeSettings
// cobre a semântica; este cobre o caminho que quebrava em produção — o
// UpdateStoreNotificationSettings substitui a coluna inteira, então uma Settings
// parcial apagava waitlist_notified e cart_recovery em todo save.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

func seedStoreWithSettings(t *testing.T, raw string) string {
	t.Helper()
	var id string
	slug := fmt.Sprintf("merge-%d", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO stores (name, slug, notification_settings)
		 VALUES ('Loja Merge Test', $1, $2::jsonb) RETURNING id::text`,
		slug, raw,
	).Scan(&id); err != nil {
		t.Fatalf("seedStoreWithSettings: %v", err)
	}
	return id
}

func readRawSettings(t *testing.T, storeID string) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT notification_settings FROM stores WHERE id = $1::uuid`, storeID,
	).Scan(&raw); err != nil {
		t.Fatalf("readRawSettings: %v", err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("notification_settings não é objeto JSON: %v (%s)", err, raw)
	}
	return out
}

func TestUpdateSettingsPreservesUnsentKeysInJSONB(t *testing.T) {
	requireDB(t)

	// Estado inicial: a loja tem as duas chaves que o PUT da aba de
	// Comunicações não carrega.
	const stored = `{
		"checkout_immediate": {"enabled": true, "template": "checkout antigo"},
		"item_added":         {"enabled": true, "template": "item antigo"},
		"checkout_reminder":  {"enabled": true, "template": "lembrete antigo"},
		"waitlist_notified":  {"enabled": true, "template": "fila antiga"},
		"waitlist_joined":    {"enabled": true, "template": "entrou na fila antigo"},
		"payment_cancelled":  {"enabled": true, "subject": "cancelado"},
		"payment_refunded":   {"enabled": true, "subject": "estornado"},
		"cart_recovery":      {"enabled": true, "delay_minutes": 30, "max_attempts": 2,
		                       "quiet_hours_start": 22, "quiet_hours_end": 8}
	}`

	storeID := seedStoreWithSettings(t, stored)
	svc := &Service{queries: testQueries, logger: zap.NewNop()}

	// Exatamente o que a aba de Comunicações manda ao salvar um card: os três
	// templates de carrinho e alguns de e-mail. Nada de waitlist_notified,
	// cart_recovery, payment_cancelled ou payment_refunded.
	incoming := Settings{
		CheckoutImmediate: &TemplateSettings{Enabled: true, Template: "checkout NOVO"},
		ItemAdded:         &TemplateSettings{Enabled: true, Template: "item antigo"},
		CheckoutReminder:  &TemplateSettings{Enabled: true, Template: "lembrete antigo"},
		PaymentConfirmed:  &EmailTemplateSettings{Enabled: true},
	}

	if err := svc.UpdateSettings(context.Background(), storeID, incoming); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got := readRawSettings(t, storeID)

	// A edição pedida foi aplicada.
	var ci TemplateSettings
	if err := json.Unmarshal(got["checkout_immediate"], &ci); err != nil {
		t.Fatalf("checkout_immediate ilegível: %v", err)
	}
	if ci.Template != "checkout NOVO" {
		t.Errorf("checkout_immediate.template = %q, quero %q", ci.Template, "checkout NOVO")
	}

	// E nada mais sumiu — é isto que regredia.
	for _, key := range []string{"waitlist_notified", "waitlist_joined", "cart_recovery", "payment_cancelled", "payment_refunded"} {
		if _, ok := got[key]; !ok {
			t.Errorf("chave %q sumiu do JSONB depois do save", key)
		}
	}

	var cr CartRecoverySettings
	if err := json.Unmarshal(got["cart_recovery"], &cr); err != nil {
		t.Fatalf("cart_recovery ilegível: %v", err)
	}
	if cr.DelayMinutes != 30 || cr.MaxAttempts != 2 || !cr.Enabled {
		t.Errorf("cart_recovery = %+v, quero os valores originais preservados", cr)
	}

	var wl TemplateSettings
	if err := json.Unmarshal(got["waitlist_notified"], &wl); err != nil {
		t.Fatalf("waitlist_notified ilegível: %v", err)
	}
	if wl.Template != "fila antiga" {
		t.Errorf("waitlist_notified.template = %q, quero preservado", wl.Template)
	}
}

// Dois saves seguidos de cards diferentes não podem se atropelar — é o cenário
// real do lojista mexendo em vários cards na mesma sessão.
func TestUpdateSettingsSequentialSavesAccumulate(t *testing.T) {
	requireDB(t)

	storeID := seedStoreWithSettings(t, `{
		"checkout_immediate": {"enabled": true, "template": "A"},
		"item_added":         {"enabled": true, "template": "B"},
		"waitlist_notified":  {"enabled": true, "template": "fila"}
	}`)
	svc := &Service{queries: testQueries, logger: zap.NewNop()}
	ctx := context.Background()

	if err := svc.UpdateSettings(ctx, storeID, Settings{
		CheckoutImmediate: &TemplateSettings{Enabled: true, Template: "A2"},
	}); err != nil {
		t.Fatalf("primeiro save: %v", err)
	}
	if err := svc.UpdateSettings(ctx, storeID, Settings{
		ItemAdded: &TemplateSettings{Enabled: false, Template: "B2"},
	}); err != nil {
		t.Fatalf("segundo save: %v", err)
	}

	final, err := svc.GetSettings(ctx, storeID)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}

	if final.CheckoutImmediate.Template != "A2" {
		t.Errorf("checkout_immediate = %q, quero A2 (o 2º save não pode desfazer o 1º)", final.CheckoutImmediate.Template)
	}
	if final.ItemAdded.Template != "B2" || final.ItemAdded.Enabled {
		t.Errorf("item_added = %+v, quero B2 desabilitado", final.ItemAdded)
	}
	if final.WaitlistJoined == nil || final.WaitlistJoined.Template != "fila" {
		t.Errorf("waitlist_joined = %+v, quero sobreviver aos dois saves", final.WaitlistJoined)
	}
}
