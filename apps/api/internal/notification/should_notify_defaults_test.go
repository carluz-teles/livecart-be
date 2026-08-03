package notification

// Seção NULA no JSONB significa "a loja nunca customizou", nunca "a loja
// desligou".
//
// O caso real: a loja de staging tinha checkout_immediate, item_added e
// checkout_reminder com valor JSON `null` e as chaves de e-mail intactas —
// resíduo de um save anterior ao mergeSettings. O comprador comentou "Eu quero
// 1003" numa live, o comentário virou carrinho, o estoque foi reservado no ERP
// e a DM com o link nunca saiu. Sem erro, sem log, sem nada na tela: parecia
// que o webhook não tinha chegado.
//
// A causa era ShouldNotify lendo a seção CRUA e tratando nil como desligado,
// enquanto a renderização já caía no padrão pelo getTemplateSettings. A
// mensagem estava pronta; o portão é que a barrava por falta de uma
// configuração que ninguém precisava ter feito.
//
// Este teste fixa o contrato no NÍVEL DA DECISÃO, que é onde ele quebrou:
// dadas seções nulas, ShouldNotify decide igual a uma loja que nunca mexeu em
// nada.

import "testing"

// cartFlowGate descreve o que cada gatilho de carrinho exige do cart_settings.
type cartFlowGate struct {
	notifType NotificationType
	isNewCart bool
	settings  CartMessageSettings
}

func gatesUnderTest() []cartFlowGate {
	on := CartMessageSettings{RealTimeCart: true, SendExpirationReminder: true}
	return []cartFlowGate{
		{TypeCheckoutImmediate, true, on},
		{TypeItemAdded, false, on},
		{TypeCheckoutReminder, false, on},
	}
}

// TestSecaoNulaCaiNoPadraoEmVezDeSilenciar é o guard do bug de staging.
func TestSecaoNulaCaiNoPadraoEmVezDeSilenciar(t *testing.T) {
	for _, g := range gatesUnderTest() {
		t.Run(string(g.notifType), func(t *testing.T) {
			// O JSONB de staging: as chaves de carrinho existem e valem null.
			stored := Settings{
				CheckoutImmediate: nil,
				ItemAdded:         nil,
				CheckoutReminder:  nil,
			}

			svc := &Service{}
			section := svc.getTemplateSettings(&stored, g.notifType)
			if section == nil {
				t.Fatalf("%s: seção nula não caiu no padrão — o gatilho fica mudo para a loja inteira", g.notifType)
			}
			if !section.Enabled {
				t.Errorf("%s: o padrão veio desligado; uma loja que nunca configurou nada não receberia a DM", g.notifType)
			}
		})
	}
}

// TestPadraoDecideIgualAUmaLojaQueNuncaCustomizou compara as duas leituras que
// divergiram: seção ausente contra o default explícito. Se um dia alguém voltar
// a ler a seção crua no portão, os dois lados deixam de bater aqui.
func TestPadraoDecideIgualAUmaLojaQueNuncaCustomizou(t *testing.T) {
	defaults := DefaultSettings()
	vazio := Settings{}
	svc := &Service{}

	for _, g := range gatesUnderTest() {
		t.Run(string(g.notifType), func(t *testing.T) {
			comDefault := svc.getTemplateSettings(&defaults, g.notifType)
			semNada := svc.getTemplateSettings(&vazio, g.notifType)

			if comDefault == nil || semNada == nil {
				t.Fatalf("%s: uma das leituras devolveu nil (default=%v, vazio=%v)", g.notifType, comDefault, semNada)
			}
			if comDefault.Enabled != semNada.Enabled {
				t.Errorf("%s: loja sem configuração decide Enabled=%v, mas o padrão diz %v",
					g.notifType, semNada.Enabled, comDefault.Enabled)
			}
			if comDefault.Template != semNada.Template {
				t.Errorf("%s: loja sem configuração cairia num texto diferente do padrão", g.notifType)
			}
		})
	}
}
