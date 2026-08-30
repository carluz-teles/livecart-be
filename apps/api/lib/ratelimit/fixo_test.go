package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// relógio injetável: medir espaçamento com time.Sleep real deixa o teste lento
// e instável.
type relogio struct{ t time.Time }

func (r *relogio) avancar(d time.Duration) { r.t = r.t.Add(d) }

func fixoComRelogio(rps float64) (*Fixo, *relogio) {
	r := &relogio{t: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)}
	f := NovoFixo(rps)
	f.agora = func() time.Time { return r.t }
	return f, r
}

// A primeira passa livre; as seguintes são espaçadas pelo intervalo.
func TestFixoEspacaAsRequisicoes(t *testing.T) {
	f, _ := fixoComRelogio(2) // 2 req/s → 500 ms entre vagas

	res, _ := f.Allow(context.Background())
	if !res.Allowed {
		t.Fatal("a primeira requisição devia passar direto")
	}
	res, _ = f.Allow(context.Background())
	if res.Allowed {
		t.Fatal("a segunda devia ser adiada")
	}
	if res.RetryAfter != 500*time.Millisecond {
		t.Errorf("RetryAfter = %s, queria 500ms", res.RetryAfter)
	}

	// A terceira acumula: 1 s depois do início.
	res, _ = f.Allow(context.Background())
	if res.RetryAfter != time.Second {
		t.Errorf("a terceira devia esperar 1s, esperou %s", res.RetryAfter)
	}
}

// Depois de um tempo ocioso, a taxa não "acumula crédito" para uma rajada — o
// teto do Bling é instantâneo, e devolver 30 vagas de uma vez porque ficamos
// parados 15 s seria estourar exatamente no pior momento.
func TestFixoNaoAcumulaCreditoDeOciosidade(t *testing.T) {
	f, r := fixoComRelogio(2)

	if res, _ := f.Allow(context.Background()); !res.Allowed {
		t.Fatal("primeira devia passar")
	}
	r.avancar(10 * time.Second)

	// Duas seguidas depois da ociosidade: a primeira passa, a segunda espera.
	if res, _ := f.Allow(context.Background()); !res.Allowed {
		t.Fatal("depois da ociosidade a próxima devia passar")
	}
	res, _ := f.Allow(context.Background())
	if res.Allowed {
		t.Error("a seguinte NÃO devia passar — ociosidade não vira crédito de rajada")
	}
}

// A propriedade que não pode se perder: quando a vaga não chega dentro do prazo
// do chamador, Wait devolve SEM DORMIR.
//
// É a lição dos 115 `unconfirmed`: uma escrita que ficou na fila até o prazo
// estourar NUNCA SAIU da máquina, e quem chama precisa poder afirmar isso em
// vez de arquivar como ambígua.
func TestFixoNaoDormeAlemDoPrazoDoChamador(t *testing.T) {
	f, _ := fixoComRelogio(0.5) // 1 vaga a cada 2 s

	// Consome a vaga livre.
	if res, _ := f.Allow(context.Background()); !res.Allowed {
		t.Fatal("primeira devia passar")
	}

	// Prazo de 100 ms; a próxima vaga só em 2 s.
	ctx, cancel := context.WithDeadline(context.Background(), f.agora().Add(100*time.Millisecond))
	defer cancel()

	inicio := time.Now()
	err := f.Wait(ctx)
	decorrido := time.Since(inicio)

	if err == nil {
		t.Fatal("Wait devia falhar: a vaga não cabe no prazo")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("erro = %v, queria DeadlineExceeded", err)
	}
	// Sem dormir: devolveu na hora, não depois de 2 s.
	if decorrido > 200*time.Millisecond {
		t.Errorf("Wait DORMIU %s antes de desistir — devia devolver imediatamente", decorrido)
	}
}

// Contexto já cancelado não pode consumir vaga nem dormir.
func TestFixoRespeitaContextoJaCancelado(t *testing.T) {
	f, _ := fixoComRelogio(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := f.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("erro = %v, queria Canceled", err)
	}
}

// Wait sem prazo espera a vaga e prossegue.
func TestFixoEsperaEProssegueQuandoCabe(t *testing.T) {
	f := NovoFixo(200) // 5 ms entre vagas — rápido o bastante para o teste

	inicio := time.Now()
	for i := 0; i < 3; i++ {
		if err := f.Wait(context.Background()); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	d := time.Since(inicio)
	if d < 8*time.Millisecond {
		t.Errorf("3 chamadas a 200/s levaram %s — o freio não segurou", d)
	}
	// Teto além do piso: a primeira versão do Wait reservava uma vaga NOVA a
	// cada volta do laço e as 3 chamadas levavam 12,8 SEGUNDOS. Sem este limite
	// o bug volta sem ninguém perceber, porque o piso continuaria passando.
	if d > 200*time.Millisecond {
		t.Errorf("3 chamadas a 200/s levaram %s — o Wait está reservando vaga a mais por volta", d)
	}
}

// Quem desiste devolve a vaga: sem isso cada timeout deixaria um buraco na
// grade e a taxa efetiva cairia abaixo da configurada, apertando o freio
// justamente quando a live precisa de vazão.
func TestFixoDevolveAVagaDeQuemDesiste(t *testing.T) {
	f, _ := fixoComRelogio(1)

	if err := f.Wait(context.Background()); err != nil { // consome a livre
		t.Fatal(err)
	}
	proximaAntes := f.proxima

	ctx, cancel := context.WithDeadline(context.Background(), f.agora().Add(time.Millisecond))
	defer cancel()
	if err := f.Wait(ctx); err == nil {
		t.Fatal("queria falha por prazo curto")
	}

	if !f.proxima.Equal(proximaAntes) {
		t.Errorf("a vaga não foi devolvida: proxima andou de %s para %s", proximaAntes, f.proxima)
	}
}

// UpdateFromHeaders é no-op e NÃO pode virar um caminho que "liga" o limitador:
// o Fixo tem de funcionar idêntico antes e depois, porque o Bling nunca manda
// header nenhum.
func TestFixoIgnoraHeadersSemMudarDeComportamento(t *testing.T) {
	f, _ := fixoComRelogio(2)

	f.UpdateFromHeaders(999, 1)
	if res, _ := f.Allow(context.Background()); !res.Allowed {
		t.Fatal("primeira devia passar")
	}
	f.UpdateFromHeaders(999, 1)
	if res, _ := f.Allow(context.Background()); res.Allowed {
		t.Error("header não pode afrouxar o freio — o Fixo é preditivo por decisão")
	}
}

// Contrato: o Fixo é um RateLimiter válido.
func TestFixoSatisfazAInterface(t *testing.T) {
	var _ RateLimiter = NovoFixo(1)
}
