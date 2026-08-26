package erpwrite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// relogioFalso torna o limiter determinístico: sem ele o teste vira medição de
// wall-clock e passa a falhar em máquina carregada.
type relogioFalso struct {
	mu  sync.Mutex
	t   time.Time
	dor []time.Duration // durações que o Wait pediu para dormir
}

func novoRelogio() *relogioFalso { return &relogioFalso{t: time.Unix(1700000000, 0)} }

func (r *relogioFalso) agora() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t
}

func (r *relogioFalso) avanca(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.t = r.t.Add(d)
}

// dormir avança o relógio em vez de bloquear de verdade.
func (r *relogioFalso) dormir(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.dor = append(r.dor, d)
	r.t = r.t.Add(d)
	r.mu.Unlock()
	return nil
}

func limiterFalso(l Limits) (*Limiter, *relogioFalso) {
	r := novoRelogio()
	lim := NewLimiter(l)
	lim.now = r.agora
	lim.sleep = r.dormir
	return lim, r
}

// TestBaldeDeRajada — 4 por segundo, e a quinta espera a janela rolar.
func TestBaldeDeRajada(t *testing.T) {
	lim, rel := limiterFalso(DefaultLimits())
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := lim.Wait(ctx); err != nil {
			t.Fatalf("requisição %d na rajada deveria passar: %v", i+1, err)
		}
	}
	antes := rel.agora()
	if err := lim.Wait(ctx); err != nil {
		t.Fatalf("5ª requisição: %v", err)
	}
	if esperou := rel.agora().Sub(antes); esperou < time.Second {
		t.Errorf("a 5ª esperou %s; o balde de rajada é 4 por segundo", esperou)
	}
}

// TestBaldeSustentado — 30 por minuto.
func TestBaldeSustentado(t *testing.T) {
	lim, rel := limiterFalso(DefaultLimits())
	ctx := context.Background()
	inicio := rel.agora()
	for i := 0; i < 30; i++ {
		if err := lim.Wait(ctx); err != nil {
			t.Fatalf("requisição %d: %v", i+1, err)
		}
		rel.avanca(300 * time.Millisecond) // abaixo do teto de rajada
	}
	if err := lim.Wait(ctx); err != nil {
		t.Fatalf("31ª: %v", err)
	}
	if total := rel.agora().Sub(inicio); total < time.Minute {
		t.Errorf("31 requisições couberam em %s; o balde sustentado é 30 por minuto", total)
	}
}

// TestPrazoCurtoDevolveNotDispatched é A propriedade do pacote.
//
// Na live simulada de 26/08, 115 reservas esperaram a fila até o prazo de 90 s
// estourar e foram arquivadas como ambíguas. Nenhuma havia saído. Aqui isso vira
// ErrNotDispatched, que o Classify transforma em NotApplied — repetível.
func TestPrazoCurtoDevolveNotDispatched(t *testing.T) {
	lim, rel := limiterFalso(DefaultLimits())
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := lim.Wait(ctx); err != nil {
			t.Fatalf("preenchendo a rajada: %v", err)
		}
	}
	// Prazo de 100 ms, mas a janela só rola em 1 s.
	curto, cancel := context.WithDeadline(context.Background(), rel.agora().Add(100*time.Millisecond))
	defer cancel()

	err := lim.Wait(curto)
	if err == nil {
		t.Fatal("Wait deveria recusar: a janela não cabe no prazo")
	}
	if !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("erro = %v; precisa ser ErrNotDispatched para o movimento virar repetível", err)
	}
	if got := Classify(Attempt{Dispatched: false, Err: err}); got != NotApplied {
		t.Errorf("classificou como %s; tinha de ser NotApplied", got)
	}
	if n := len(rel.dor); n != 0 {
		t.Errorf("dormiu %d vez(es) mesmo sem caber no prazo — é isso que produz o unconfirmed", n)
	}
}

// TestContextoJaCanceladoNaoDespacha.
func TestContextoJaCanceladoNaoDespacha(t *testing.T) {
	lim, _ := limiterFalso(DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := lim.Wait(ctx)
	if !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("erro = %v, esperava ErrNotDispatched", err)
	}
}

// TestSemPrazoEsperaAJanela — sem deadline não há o que estourar.
func TestSemPrazoEsperaAJanela(t *testing.T) {
	lim, rel := limiterFalso(Limits{BurstN: 2, BurstWindow: time.Second, SustainedN: 100, SustWindow: time.Minute})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_ = lim.Wait(ctx)
	}
	if err := lim.Wait(ctx); err != nil {
		t.Fatalf("sem prazo o Wait deve esperar, não recusar: %v", err)
	}
	if len(rel.dor) == 0 {
		t.Error("esperava ter dormido para aguardar a janela")
	}
}

// TestObserveRespeitaResetAnunciado — quando o header VEM, ele manda.
func TestObserveRespeitaResetAnunciado(t *testing.T) {
	lim, rel := limiterFalso(DefaultLimits())
	lim.Observe(0, 30*time.Second)
	curto, cancel := context.WithDeadline(context.Background(), rel.agora().Add(5*time.Second))
	defer cancel()
	if err := lim.Wait(curto); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("após Observe(remaining=0, reset=30s) com prazo de 5s, erro = %v", err)
	}
}

// TestObserveIgnoraDadoInutil — os headers não vêm sempre (medido em 26/08),
// então o limiter não pode se desregular quando eles faltam.
func TestObserveIgnoraDadoInutil(t *testing.T) {
	casos := []struct {
		rem   int
		reset time.Duration
	}{{5, 0}, {10, 20 * time.Second}, {0, 0}, {-1, 0}, {1, -5 * time.Second}}
	for i, c := range casos {
		t.Run(fmt.Sprintf("caso_%d", i), func(t *testing.T) {
			lim, _ := limiterFalso(DefaultLimits())
			lim.Observe(c.rem, c.reset)
			if err := lim.Wait(context.Background()); err != nil {
				t.Errorf("Observe(%d,%v) travou o limiter: %v", c.rem, c.reset, err)
			}
		})
	}
}

// TestLimiterSobConcorrencia — nunca ultrapassa o teto, com 50 goroutines.
func TestLimiterSobConcorrencia(t *testing.T) {
	lim := NewLimiter(Limits{BurstN: 4, BurstWindow: 50 * time.Millisecond,
		SustainedN: 1000, SustWindow: time.Minute})
	var mu sync.Mutex
	var marcas []time.Time
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lim.Wait(context.Background()); err != nil {
				return
			}
			mu.Lock()
			marcas = append(marcas, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(marcas) != 50 {
		t.Fatalf("passaram %d de 50", len(marcas))
	}
	// Em qualquer janela de 50ms não pode haver mais que 4.
	for i := range marcas {
		n := 0
		for j := range marcas {
			if d := marcas[j].Sub(marcas[i]); d >= 0 && d < 50*time.Millisecond {
				n++
			}
		}
		if n > 4 {
			t.Fatalf("janela com %d requisições; o teto de rajada é 4", n)
		}
	}
}
