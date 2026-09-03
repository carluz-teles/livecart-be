package live

import "testing"

// A DM SÓ SAI QUANDO É VERDADE.
//
// Regra do lojista, 02/09/2026: "não podemos avisar o cliente que deu certo sem
// saber se vai dar certo. Virou loteria."
//
// Produção, 01/09/2026, @dany.lifestyle: ela comentou 2091, recebeu
//
//	"Oi @dany.lifestyle! ➕
//	 Novo item adicionado: Pote com Tampa Pinha – 11cm
//	 Seu carrinho agora tem 40 itens"
//
// e a escrita no Tiny morreu num 429 no mesmo minuto. Ela ficou com a prova de
// uma compra que a loja nunca enxergou.
//
// A ordem das operações já estava certa — a reserva roda ANTES da mensagem. O
// que faltava era a falha PESAR: ela só virava um Warn e a DM saía igual.
func TestUmItemPendenteCalaADMDoComentarioInteiro(t *testing.T) {
	casos := []struct {
		nome      string
		itens     []resultadoDoItem
		pendentes int
	}{
		{"tudo confirmado — a DM sai",
			[]resultadoDoItem{{erpPendente: false}, {erpPendente: false}}, 0},
		{"o único item falhou — cala",
			[]resultadoDoItem{{erpPendente: true}}, 1},
		{"um de três falhou — cala o comentário INTEIRO",
			[]resultadoDoItem{{erpPendente: false}, {erpPendente: true}, {erpPendente: false}}, 1},
		{"todos falharam", []resultadoDoItem{{erpPendente: true}, {erpPendente: true}}, 2},
		{"comentário sem item", nil, 0},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			if got := itensPendentesNoERP(c.itens); got != c.pendentes {
				t.Errorf("pendentes = %d, queria %d", got, c.pendentes)
			}
		})
	}
}

// Um comentário vira UMA DM: "2071 x6 e 2091" é uma frase só, com o total do
// carrinho. Mandar a metade que deu certo faria a compradora conferir um total
// que não bate — pior do que adiar a mensagem inteira.
func TestMetadeConfirmadaNaoAutorizaMeiaMensagem(t *testing.T) {
	metade := []resultadoDoItem{{erpPendente: false}, {erpPendente: true}}
	if itensPendentesNoERP(metade) == 0 {
		t.Error("um comentário com item pendente foi tratado como totalmente " +
			"confirmado — a DM sairia com um total que a loja não tem")
	}
}
