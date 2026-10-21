package providers

import "testing"

func TestMissingERPOrderClosesPurchaseWithoutBecomingWebhookStatus(t *testing.T) {
	t.Parallel()

	status := ERPOrderStatusNaoEncontrado
	if !status.Terminal() || !status.FechadoParaNovosItens() || status.VoltouAViver() {
		t.Fatal("missing order must stop the sweep and item admission without reopening the cart")
	}
	if _, ok := ParseERPOrderStatus(string(status)); ok {
		t.Fatal("absence must not be accepted as an ERP webhook situation")
	}
	if _, ok := SituacaoFromERPOrderStatus(status); ok {
		t.Fatal("absence has no ERP situation code")
	}
}
