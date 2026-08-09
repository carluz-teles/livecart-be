package integration

// A janela de silêncio do webhook de comentário, calibrada pelo que foi medido.
//
// Em 09/08, com o Cloudflare comprovadamente fora do caminho, duas transmissões
// seguidas se comportaram igual:
//
//	live …535256   webhook de 16:33:35 a 16:34:16 (41s)   live seguiu até 16:38:54
//	live …981275   webhook de 16:53:55 a 16:54:13 (18s)   live seguiu até 16:56:44
//
// Mesma mídia antes e depois do corte. Comentários continuaram nascendo — o
// polling os capturou com 3 a 20 segundos de idade — e o campo `messaging`
// seguiu chegando na mesma conexão. Nós respondemos 200 aos 16 POST da janela,
// sem uma falha de assinatura.
//
// Contra um corte que acontece no primeiro minuto de transmissão, os cinco
// minutos originais de espera eram quase a live inteira.

import (
	"testing"
	"time"
)

func TestJanelaDeSilencioPegaOCorteMedido(t *testing.T) {
	// Os dois cortes reais: 41s e 18s de entrega, depois nada. A janela tem de
	// disparar dentro da transmissão, não depois dela.
	cortesMedidos := []time.Duration{41 * time.Second, 18 * time.Second}
	duracaoDaLiveMaisCurta := 197 * time.Second // a de 16:53:27 → 16:56:44

	for _, corte := range cortesMedidos {
		// Detecção acontece em corte + janela. Precisa caber na transmissão.
		deteccao := corte + webhookSilenceAlert
		if deteccao >= duracaoDaLiveMaisCurta {
			t.Errorf("corte aos %v + janela de %v detecta em %v, depois do fim da live mais curta (%v) — "+
				"a reinscrição chegaria tarde demais para servir de alguma coisa",
				corte, webhookSilenceAlert, deteccao, duracaoDaLiveMaisCurta)
		}
	}
}

// A janela não pode ser tão curta a ponto de confundir "ninguém comentou" com
// "a entrega caiu". Numa live vendendo, comentário sai a cada poucos segundos;
// numa pausa natural, meio minuto de silêncio é comum.
func TestJanelaDeSilencioNaoConfundePausaComQueda(t *testing.T) {
	if webhookSilenceAlert < 60*time.Second {
		t.Errorf("janela de %v trata pausa natural da live como queda de entrega", webhookSilenceAlert)
	}
	// E o sweep roda a cada 20s: a janela precisa valer vários sweeps, senão o
	// alerta dispara na granularidade errada.
	if webhookSilenceAlert < 3*20*time.Second {
		t.Errorf("janela de %v é curta perto do sweep de 20s", webhookSilenceAlert)
	}
}

// O recheck periódico da inscrição é a rede lenta; a janela de silêncio é a
// rápida. Se a lenta fosse mais rápida que a rápida, a rápida não serviria.
func TestSilencioAgeAntesDoRecheckPeriodico(t *testing.T) {
	if webhookSilenceAlert >= subscriptionRecheckEvery {
		t.Errorf("silêncio (%v) não pode ser mais lento que o recheck periódico (%v) — "+
			"a reação ao corte medido tem de vir antes da varredura de rotina",
			webhookSilenceAlert, subscriptionRecheckEvery)
	}
}
