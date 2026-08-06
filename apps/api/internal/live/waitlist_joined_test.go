package live

// Entrar na fila tem de soar como entrar na fila.
//
// O bug de campo: o comprador pediu um produto sem estoque, foi corretamente
// para a lista de espera — e recebeu na DM "Adicionei Perfume Deo Colonia ao
// seu carrinho da Semana black. Agora são 3 itens — R$ 60,00". O texto de
// item_added, para um item que NÃO entrou no carrinho. Nada na mensagem dizia
// "fila", "espera" ou "sem estoque": do lado do comprador, a compra tinha dado
// certo.
//
// Os dois gatilhos de fila que já existiam cobriam só o desfecho —
// waitlist_notified quando o item libera, waitlist_unfulfilled quando não
// libera até o fim. A ENTRADA na fila não tinha mensagem nenhuma, então caía no
// texto genérico de carrinho.

import (
	"testing"

	"livecart/apps/api/internal/notification"
)

func TestNotificationTypeForComment(t *testing.T) {
	cases := []struct {
		name          string
		isNewCart     bool
		waitlistedQty int
		want          notification.NotificationType
	}{
		{
			name: "primeiro pedido, tudo em estoque",
			isNewCart: true, waitlistedQty: 0,
			want: notification.TypeCheckoutImmediate,
		},
		{
			name: "item novo em carrinho existente, tudo em estoque",
			isNewCart: false, waitlistedQty: 0,
			want: notification.TypeItemAdded,
		},
		{
			// O caso de campo.
			name: "item esgotado no primeiro pedido vai para a fila",
			isNewCart: true, waitlistedQty: 1,
			want: notification.TypeWaitlistJoined,
		},
		{
			name: "item esgotado em carrinho existente vai para a fila",
			isNewCart: false, waitlistedQty: 1,
			want: notification.TypeWaitlistJoined,
		},
		{
			// Pediu 3, levou 2, 1 ficou aguardando. A fila ganha o assunto da
			// mensagem: as 2 que entraram aparecem no {total_itens} do próprio
			// template de fila.
			name: "atendimento parcial ainda é mensagem de fila",
			isNewCart: false, waitlistedQty: 1,
			want: notification.TypeWaitlistJoined,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := notificationTypeForComment(tc.isNewCart, tc.waitlistedQty)
			if got != tc.want {
				t.Errorf("notificationTypeForComment(novo=%v, fila=%d) = %s, quero %s",
					tc.isNewCart, tc.waitlistedQty, got, tc.want)
			}
		})
	}
}

// A mensagem de fila não pode reusar o vocabulário de "adicionei ao carrinho",
// que é exatamente o que confundiu o comprador. Este teste lê o texto padrão
// porque o defeito era de COPY, não de roteamento — trocar o tipo e manter o
// texto antigo passaria em todo o resto da suíte.
func TestTextoPadraoDaFilaFalaDeFila(t *testing.T) {
	defaults := notification.DefaultSettings()
	if defaults.WaitlistJoined == nil {
		t.Fatal("waitlist_joined sem texto padrão — a loja que nunca customizou não recebe nada")
	}
	tpl := defaults.WaitlistJoined.Template

	if !containsAny(tpl, "fila", "espera", "aguard") {
		t.Errorf("o texto padrão da fila não menciona fila/espera:\n%s", tpl)
	}
	// "Adicionei ... ao seu carrinho" é a frase que fez o comprador achar que
	// tinha comprado.
	if containsAny(tpl, "Adicionei") {
		t.Errorf("o texto padrão da fila diz que adicionou ao carrinho:\n%s", tpl)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
