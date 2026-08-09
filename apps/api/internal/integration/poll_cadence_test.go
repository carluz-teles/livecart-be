package integration

// A cadência do polling de comentário, e por que ela deixou de ser detalhe.
//
// O polling nasceu como rede de segurança para o webhook. Virou o caminho
// principal: a Meta para de entregar `live_comments` no meio da transmissão e
// volta sozinha depois de dezenas de minutos. Em 08/08 foram 40,5 minutos de
// silêncio (18:30:59 → 19:11:30) com `messaging` fluindo o tempo todo na mesma
// conexão, e na janela das 21h os 4 webhooks pararam às 21:04:33 enquanto
// comentários novos continuavam nascendo.
//
// Que continuavam nascendo não é suposição: a idade na captura mostrou os
// comentários do silêncio com 3 a 20 segundos de vida. Onze dos dezessete
// vieram por polling.
//
// Com o webhook mudo, a idade na captura É a espera do comprador entre escrever
// "quero" e o carrinho existir.

import (
	"testing"
	"time"
)

func TestCadenciaDoPollingAceleraComLiveNoAr(t *testing.T) {
	if got := pollInterval(true); got != pollIntervalLive {
		t.Errorf("com live no ar o intervalo = %v, quero %v", got, pollIntervalLive)
	}
	if got := pollInterval(false); got != pollIntervalIdle {
		t.Errorf("sem live o intervalo = %v, quero %v", got, pollIntervalIdle)
	}
}

// A cadência de live tem de ser sensivelmente menor, senão a mudança é
// decorativa. E não pode ser tão curta a ponto de virar martelo na Graph.
func TestCadenciaDeLiveEhMaisCurtaMasNaoAbusiva(t *testing.T) {
	if pollIntervalLive >= pollIntervalIdle {
		t.Fatalf("cadência de live (%v) não é mais curta que a ociosa (%v)", pollIntervalLive, pollIntervalIdle)
	}
	if pollIntervalIdle/pollIntervalLive < 2 {
		t.Errorf("acelerar de %v para %v mal muda a espera do comprador", pollIntervalIdle, pollIntervalLive)
	}
	if pollIntervalLive < 2*time.Second {
		t.Errorf("cadência de live em %v é curta demais — o limitador por integração já espaça as chamadas "+
			"em ~1s e isto viraria fila crescente", pollIntervalLive)
	}
}

// A pior espera possível é um tick inteiro: o comentário nasce logo depois de
// uma varredura e só é visto na seguinte.
func TestPiorEsperaDoCompradorComLiveNoAr(t *testing.T) {
	const aceitavel = 10 * time.Second
	if pollIntervalLive > aceitavel {
		t.Errorf("pior espera com live no ar = %v, acima do aceitável de %v para venda ao vivo",
			pollIntervalLive, aceitavel)
	}
}
