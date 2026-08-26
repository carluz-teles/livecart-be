package erpwrite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFilaSerializaPorChave é a propriedade que impede a corrupção de grade
// medida em 26/08 (3 PUT /itens simultâneos deixaram o pedido com duas linhas).
func TestFilaSerializaPorChave(t *testing.T) {
	for _, n := range []int{2, 3, 5, 8, 13, 21} {
		t.Run(fmt.Sprintf("%d_concorrentes", n), func(t *testing.T) {
			q := NewQueue(16)
			var dentro atomic.Int32
			var maxDentro atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = q.Do(context.Background(), "pedido-1", func(context.Context) error {
						d := dentro.Add(1)
						for {
							m := maxDentro.Load()
							if d <= m || maxDentro.CompareAndSwap(m, d) {
								break
							}
						}
						time.Sleep(time.Millisecond)
						dentro.Add(-1)
						return nil
					})
				}()
			}
			wg.Wait()
			if got := maxDentro.Load(); got != 1 {
				t.Errorf("%d execuções simultâneas no MESMO pedido; a fila tem de serializar", got)
			}
		})
	}
}

// TestChavesDiferentesCorremEmParalelo — serializar por pedido não pode virar
// serializar tudo, senão a live inteira anda em fila única.
func TestChavesDiferentesCorremEmParalelo(t *testing.T) {
	q := NewQueue(4)
	var dentro atomic.Int32
	var maxDentro atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = q.Do(context.Background(), fmt.Sprintf("pedido-%d", i), func(context.Context) error {
				d := dentro.Add(1)
				for {
					m := maxDentro.Load()
					if d <= m || maxDentro.CompareAndSwap(m, d) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				dentro.Add(-1)
				return nil
			})
		}(i)
	}
	wg.Wait()
	if maxDentro.Load() < 2 {
		t.Errorf("pedidos distintos rodaram em série (max simultâneo=%d)", maxDentro.Load())
	}
}

// TestTetoGlobalRespeitado — o balde de rajada da API é 4/s; passar disso só
// gera 429.
func TestTetoGlobalRespeitado(t *testing.T) {
	for _, teto := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("teto_%d", teto), func(t *testing.T) {
			q := NewQueue(teto)
			var dentro atomic.Int32
			var maxDentro atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < teto*5; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_ = q.Do(context.Background(), fmt.Sprintf("p-%d", i), func(context.Context) error {
						d := dentro.Add(1)
						for {
							m := maxDentro.Load()
							if d <= m || maxDentro.CompareAndSwap(m, d) {
								break
							}
						}
						time.Sleep(2 * time.Millisecond)
						dentro.Add(-1)
						return nil
					})
				}(i)
			}
			wg.Wait()
			if got := int(maxDentro.Load()); got > teto {
				t.Errorf("%d execuções simultâneas com teto %d", got, teto)
			}
		})
	}
}

// TestCancelarEsperandoVezNaoDespacha — quem desiste na fila não executou nada,
// e precisa saber disso para poder repetir com segurança.
func TestCancelarEsperandoVezNaoDespacha(t *testing.T) {
	q := NewQueue(1)
	segurando := make(chan struct{})
	liberar := make(chan struct{})
	go func() {
		_ = q.Do(context.Background(), "k", func(context.Context) error {
			close(segurando)
			<-liberar
			return nil
		})
	}()
	<-segurando

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var executou atomic.Bool
	err := q.Do(ctx, "k", func(context.Context) error { executou.Store(true); return nil })
	close(liberar)

	if executou.Load() {
		t.Fatal("fn rodou apesar do contexto ter expirado na espera")
	}
	if !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("erro = %v; precisa ser ErrNotDispatched", err)
	}
	if got := Classify(Attempt{Dispatched: false, Err: err}); got != NotApplied {
		t.Errorf("classificou %s; desistir na fila é provadamente não aplicado", got)
	}
}

// TestMapaDeChavesNaoVaza — uma live cria um pedido por compradora.
func TestMapaDeChavesNaoVaza(t *testing.T) {
	q := NewQueue(8)
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = q.Do(context.Background(), fmt.Sprintf("pedido-%d", i), func(context.Context) error { return nil })
		}(i)
	}
	wg.Wait()
	if n := q.EmVoo(); n != 0 {
		t.Errorf("%d chaves ficaram no mapa depois de tudo terminar", n)
	}
}

// TestErroDeFnSobeIntacto — a fila não pode mascarar o desfecho real.
func TestErroDeFnSobeIntacto(t *testing.T) {
	q := NewQueue(2)
	meu := errors.New("400 do ERP")
	err := q.Do(context.Background(), "k", func(context.Context) error { return meu })
	if !errors.Is(err, meu) {
		t.Fatalf("erro = %v, esperava o erro de fn", err)
	}
	if errors.Is(err, ErrNotDispatched) {
		t.Error("erro de fn foi marcado como não-despachado; fn JÁ tinha começado")
	}
}
