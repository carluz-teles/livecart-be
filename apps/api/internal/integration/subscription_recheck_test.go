package integration

// A inscrição de webhook precisa ser REVERIFICADA, não garantida uma vez.
//
// O latch antigo era booleano: no primeiro sucesso a loja entrava num sync.Map e
// nunca mais era checada na vida do processo. O caminho de FALHA já soltava o
// registro para retentar; o de SUCESSO travava para sempre.
//
// Isso importa porque a Meta derruba a assinatura de um app cujas entregas
// falham seguidamente — precisamente o cenário que investigamos, com o Bot Fight
// Mode do Cloudflare desafiando as entregas. Num processo que fica semanas no
// ar, a única autocorreção que tínhamos para inscrição morta era "uma vez", que
// é o mesmo que nunca.
//
// O sintoma de uma inscrição morta é indistinguível do que perseguimos há dias:
// comentário existe, a live está no ar, e nenhum webhook chega.

import (
	"testing"
	"time"
)

func TestJanelaDeReverificacaoDaInscricao(t *testing.T) {
	// Curta o bastante para uma transmissão inteira não passar sem reverificar.
	// Uma live típica dura mais que isso, então uma inscrição que morrer no meio
	// é restabelecida ainda durante a mesma transmissão.
	if subscriptionRecheckEvery > 30*time.Minute {
		t.Errorf("reverificação a cada %v é frouxa demais — uma live inteira caberia na janela",
			subscriptionRecheckEvery)
	}
	// Longa o bastante para não virar chamada à Graph a cada sweep. O laço roda
	// a cada 20s; qualquer coisa perto disso seria marteladas na Meta.
	if subscriptionRecheckEvery < 5*time.Minute {
		t.Errorf("reverificação a cada %v martela a Graph — o sweep roda a cada 20s",
			subscriptionRecheckEvery)
	}
}

// O contrato do latch: dentro da janela pula, fora da janela reverifica. É a
// diferença entre o comportamento antigo e o novo, e é o que este teste trava.
func TestLatchDaInscricaoExpira(t *testing.T) {
	s := &Service{}
	const loja = "loja-1"

	// Nada registrado: tem de verificar.
	if _, ok := s.subscriptionEnsured.Load(loja); ok {
		t.Fatal("loja nova não devia ter registro")
	}

	// Registro recente: dentro da janela, pula.
	s.subscriptionEnsured.Store(loja, time.Now())
	v, _ := s.subscriptionEnsured.Load(loja)
	if last, _ := v.(time.Time); time.Since(last) >= subscriptionRecheckEvery {
		t.Error("registro recém-gravado não pode estar fora da janela")
	}

	// Registro velho: fora da janela, reverifica. Com o latch booleano antigo
	// este caso não existia — uma vez gravado, era para sempre.
	s.subscriptionEnsured.Store(loja, time.Now().Add(-subscriptionRecheckEvery-time.Minute))
	v, _ = s.subscriptionEnsured.Load(loja)
	last, _ := v.(time.Time)
	if time.Since(last) < subscriptionRecheckEvery {
		t.Error("registro antigo tem de cair fora da janela e disparar nova verificação")
	}
}
