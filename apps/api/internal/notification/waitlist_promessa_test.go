package notification

// O texto de entrada na fila dizia "eu coloco no seu carrinho e te aviso".
// Metade era verdade: o item entra no carrinho sozinho. A outra metade nunca
// aconteceu — a promoção da fila não tem como avisar.
//
// A janela de 24h do Instagram só abre quando o COMPRADOR manda mensagem para
// a conta. Comentar não abre: comentário concede um private reply, permissão
// distinta e de uso único, já gasta nesta própria mensagem. Quando o estoque
// volta não existe comentário novo para responder, então sobrava o DM — e ele
// voltou 403 em 3 de 3 tentativas na live de 16/08.
//
// Prometer um aviso que a plataforma bloqueia faz a compradora esperar por
// algo que não vem, enquanto o item fica reservado para ela e expira sem venda.

import (
	"strings"
	"testing"
)

// Promessas de aviso ativo. Nenhuma pode aparecer no texto da fila.
var promessasDeAviso = []string{
	"te aviso",
	"te avisamos",
	"eu aviso",
	"avisaremos",
	"você será avisad",
	"te chamo",
	"te mando",
}

func TestFilaDeEsperaNaoPrometeAviso(t *testing.T) {
	texto := waitlistJoinedTemplate(t)
	minusculo := strings.ToLower(texto)

	for _, promessa := range promessasDeAviso {
		if strings.Contains(minusculo, promessa) {
			t.Errorf("o texto da fila de espera contém %q — promete um aviso que o "+
				"Instagram bloqueia; a compradora fica esperando e o item expira", promessa)
		}
	}
}

// O contrário também precisa valer: se o texto não disser o que ela deve fazer,
// trocar a promessa por silêncio só piora — ela fica sem saber que o item entra
// no carrinho sozinho e que basta reabrir o link.
func TestFilaDeEsperaMandaOlharOCarrinho(t *testing.T) {
	minusculo := strings.ToLower(waitlistJoinedTemplate(t))

	if !strings.Contains(minusculo, "carrinho") {
		t.Error("o texto da fila não menciona o carrinho — a compradora não sabe onde olhar")
	}
	if !strings.Contains(minusculo, "{link}") {
		t.Error("o texto da fila não traz {link} — sem ele não há para onde ficar de olho")
	}
}

func waitlistJoinedTemplate(t *testing.T) string {
	t.Helper()

	d := DefaultSettings()
	section := templateSection(&d, TypeWaitlistJoined)
	if section == nil || section.Template == "" {
		t.Fatal("template padrão de waitlist_joined não encontrado")
	}
	return section.Template
}
