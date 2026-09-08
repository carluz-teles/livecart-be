package integration

// A coalescência tem uma propriedade e ela é fácil de errar: absorver chamadas
// é aceitável, PERDER a última não é. Quem chega enquanto alguém roda confia que
// a repetição vai enxergar o estado que ele acabou de gravar.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCoalescenciaPreservaEventoDuranteRepeticao(t *testing.T) {
	c := novoCoalescedor()
	entrou := make(chan int, 3)
	liberar := make(chan struct{})
	concluiu := make(chan error, 1)
	go func() {
		n := 0
		_, err := c.Fazer("pedido", func() error {
			n++
			entrou <- n
			if n < 3 {
				<-liberar
			}
			return nil
		})
		concluiu <- err
	}()
	defer close(liberar)
	for esperado := 1; esperado <= 2; esperado++ {
		if n := <-entrou; n != esperado {
			t.Fatalf("execução = %d, esperada %d", n, esperado)
		}
		rodou, err := c.Fazer("pedido", func() error {
			t.Error("evento concorrente deveria ser absorvido")
			return nil
		})
		if rodou || err != nil {
			t.Fatalf("evento concorrente: rodou=%v err=%v", rodou, err)
		}
		liberar <- struct{}{}
	}
	if err := <-concluiu; err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-entrou:
		if n != 3 {
			t.Fatalf("última execução = %d, esperada 3", n)
		}
	default:
		t.Fatal("evento recebido durante a repetição foi perdido")
	}
}

func TestCoalescenciaDrenaPendenciaMesmoAposErro(t *testing.T) {
	c := novoCoalescedor()
	entrou := make(chan struct{})
	liberar := make(chan struct{})
	concluiu := make(chan error, 1)
	var execucoes atomic.Int32
	go func() {
		_, err := c.Fazer("pedido", func() error {
			if execucoes.Add(1) == 1 {
				close(entrou)
				<-liberar
				return errFalhaDeTeste
			}
			return nil
		})
		concluiu <- err
	}()
	<-entrou
	rodou, err := c.Fazer("pedido", func() error { return nil })
	close(liberar)
	if rodou || err != nil {
		t.Fatalf("evento concorrente: rodou=%v err=%v", rodou, err)
	}
	if err := <-concluiu; !errors.Is(err, errFalhaDeTeste) {
		t.Fatalf("erro original não foi preservado: %v", err)
	}
	if n := execucoes.Load(); n != 2 {
		t.Fatalf("execuções = %d, esperadas 2", n)
	}
}

func TestCoalescenciaAbsorveARajadaMasNaoPerdeAUltima(t *testing.T) {
	c := novoCoalescedor()
	var execucoes atomic.Int32
	solto := make(chan struct{})

	// A primeira execução fica presa até o teste soltá-la; as outras chegam no
	// meio e devem ser absorvidas.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = c.Fazer("p1", func() error {
			execucoes.Add(1)
			<-solto
			return nil
		})
	}()

	// Espera a primeira entrar.
	for {
		if execucoes.Load() == 1 {
			break
		}
	}

	absorvidas := 0
	for i := 0; i < 20; i++ {
		rodou, err := c.Fazer("p1", func() error { execucoes.Add(1); return nil })
		if err != nil {
			t.Fatalf("chamada %d: %v", i, err)
		}
		if !rodou {
			absorvidas++
		}
	}
	if absorvidas != 20 {
		t.Errorf("absorvidas = %d, quero 20 — enfileirar cada uma é o custo que a "+
			"coalescência existe para evitar", absorvidas)
	}

	close(solto)
	wg.Wait()

	// Duas: a que estava rodando e UMA repetição pela rajada inteira.
	if n := execucoes.Load(); n != 2 {
		t.Errorf("execuções = %d, quero 2 (a original + uma repetição). Uma só "+
			"significaria perder tudo que chegou durante a primeira", n)
	}
}

func TestChavesDiferentesNaoSeBloqueiam(t *testing.T) {
	c := novoCoalescedor()
	var execucoes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.Fazer(string(rune('a'+i)), func() error { execucoes.Add(1); return nil })
		}(i)
	}
	wg.Wait()
	if n := execucoes.Load(); n != 8 {
		t.Errorf("execuções = %d, quero 8 — produtos diferentes não disputam", n)
	}
}

func TestErroNaoDeixaAChaveTravada(t *testing.T) {
	c := novoCoalescedor()
	if _, err := c.Fazer("p1", func() error { return errFalhaDeTeste }); err == nil {
		t.Fatal("esperava o erro de volta")
	}
	rodou, err := c.Fazer("p1", func() error { return nil })
	if !rodou || err != nil {
		t.Errorf("a chave ficou travada depois de um erro (rodou=%v err=%v)", rodou, err)
	}
}

var errFalhaDeTeste = errFalha("falha de teste")

type errFalha string

func (e errFalha) Error() string { return string(e) }
