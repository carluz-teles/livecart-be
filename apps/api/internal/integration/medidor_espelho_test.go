package integration

import (
	"sync"
	"testing"
	"time"
)

// O medidor existe para responder UMA pergunta: o que satura o teto do Tiny é
// a chuva (volume de webhooks) ou o eco (retentativas)? Se ele contar errado,
// a resposta vem errada e o conserto seguinte ataca o alvo errado.

// A virada de minuto publica o minuto ANTERIOR e zera o novo. Um medidor que
// vazasse contagem entre minutos inflaria o número que vai decidir a mudança.
func TestMedidorPublicaNaViradaEZeraOMinutoNovo(t *testing.T) {
	m := novoMedidorDoEspelho()

	// Minuto A: 3 recebidos, 2 lidos.
	m.janela = time.Now().Truncate(time.Minute)
	for i := 0; i < 3; i++ {
		if r := m.registrar("recebido", "p1"); r != nil {
			t.Fatal("publicou resumo sem virar o minuto")
		}
	}
	m.registrar("lido", "p1")
	m.registrar("lido", "p2")

	// Força a virada: a janela guardada fica no passado.
	m.janela = m.janela.Add(-time.Minute)
	resumo := m.registrar("recebido", "p3")
	if resumo == nil {
		t.Fatal("a virada não publicou o minuto anterior")
	}

	valores := map[string]int64{}
	for _, f := range resumo {
		valores[f.Key] = f.Integer
	}
	if valores["recebidos"] != 3 {
		t.Errorf("recebidos = %d, queria 3", valores["recebidos"])
	}
	if valores["lidos"] != 2 {
		t.Errorf("lidos = %d, queria 2", valores["lidos"])
	}
	if valores["produtos"] != 2 {
		t.Errorf("produtos = %d, queria 2 (p1 e p2)", valores["produtos"])
	}

	// E o minuto novo começou LIMPO, com só o evento que causou a virada.
	m.janela = m.janela.Add(-time.Minute)
	novo := m.registrar("recebido", "p4")
	valores = map[string]int64{}
	for _, f := range novo {
		valores[f.Key] = f.Integer
	}
	if valores["recebidos"] != 1 {
		t.Errorf("o minuto novo nasceu com recebidos = %d, queria 1 — está vazando "+
			"contagem entre janelas", valores["recebidos"])
	}
	if valores["lidos"] != 0 {
		t.Errorf("o minuto novo nasceu com lidos = %d, queria 0", valores["lidos"])
	}
}

// Minuto sem movimento não vira linha de log. Numa loja parada o medidor tem de
// ser invisível — senão ele vira o próprio ruído que veio medir.
func TestMinutoVazioNaoPublica(t *testing.T) {
	m := novoMedidorDoEspelho()
	m.janela = time.Now().Truncate(time.Minute).Add(-time.Minute)
	if r := m.registrar("coalescido", "p1"); r != nil {
		t.Error("publicou resumo de um minuto sem recebido, lido nem limitado")
	}
}

// Os quatro pontos instrumentados chamam de goroutines diferentes: o webhook
// chega numa, a releitura roda noutra, o 429 aflora numa terceira.
func TestMedidorAguentaConcorrencia(t *testing.T) {
	m := novoMedidorDoEspelho()
	m.janela = time.Now().Truncate(time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.registrar([]string{"recebido", "lido", "retentativa", "limitado"}[i%4], "p")
		}(i)
	}
	wg.Wait()

	m.mu.Lock()
	total := m.recebidos + m.lidos + m.retentativas + m.limitados
	m.mu.Unlock()
	if total != 60 {
		t.Errorf("contou %d de 60 — há corrida no medidor", total)
	}
}
