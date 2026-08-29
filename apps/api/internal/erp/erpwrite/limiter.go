package erpwrite

import (
	"context"
	"sync"
	"time"
)

// Limits são os tetos medidos da API v3 do Tiny em 25–26/08/2026.
//
// São DOIS baldes independentes, e o 429 diz qual estourou pelo cabeçalho
// X-Ratelimit-Limit: 4 numa janela de 1 s, 30 numa janela de 60 s. A doc oficial
// acrescenta o que a medição não mostrava — o teto é POR CONTA (todas as lojas
// de um mesmo Tiny dividem) e depende do plano contratado.
type Limits struct {
	BurstN      int
	BurstWindow time.Duration
	SustainedN  int
	SustWindow  time.Duration
}

// DefaultLimits é o que foi medido. Deliberadamente conservador nos dois baldes:
// errar para menos custa latência, errar para mais custa a venda.
func DefaultLimits() Limits {
	return Limits{
		BurstN: 4, BurstWindow: time.Second,
		SustainedN: 30, SustWindow: time.Minute,
	}
}

// Limiter é um par de janelas deslizantes. A propriedade que importa não é a
// precisão do algoritmo — é `Wait` NUNCA dormir além do prazo do chamador.
//
// Foi exatamente isso que produziu os 115 `unconfirmed` da live simulada: as
// chamadas esperaram a fila até o prazo de 90 s estourar e foram arquivadas como
// ambíguas, quando na verdade nenhuma delas havia saído. Quando a espera não
// cabe no prazo, este limiter devolve ErrNotDispatched IMEDIATAMENTE — e o
// classificador transforma isso em NotApplied, que é repetível.
type Limiter struct {
	mu     sync.Mutex
	limits Limits
	burst  []time.Time
	sust   []time.Time
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

func NewLimiter(l Limits) *Limiter {
	return &Limiter{limits: l, now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait bloqueia até haver espaço nos dois baldes.
//
// Devolve ErrNotDispatched — e não o erro do contexto — quando a espera não cabe
// no prazo restante. A distinção é o coração do pacote: o chamador precisa saber
// que NADA foi enviado.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return wrapNotDispatched(err)
		}

		espera := l.reserveOrWait()
		if espera == 0 {
			return nil
		}

		// A checagem que muda tudo: se a janela não rola dentro do prazo, não
		// adianta dormir — é melhor devolver agora, provadamente não enviado.
		if prazo, ok := ctx.Deadline(); ok {
			if restante := prazo.Sub(l.now()); restante <= espera {
				return wrapNotDispatched(errDeadlineTooShort{espera: espera, restante: restante})
			}
		}
		if err := l.sleep(ctx, espera); err != nil {
			return wrapNotDispatched(err)
		}
	}
}

// reserveOrWait consome uma vaga quando há, ou diz quanto falta para a próxima.
func (l *Limiter) reserveOrWait() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	agora := l.now()
	l.burst = purge(l.burst, agora.Add(-l.limits.BurstWindow))
	l.sust = purge(l.sust, agora.Add(-l.limits.SustWindow))

	var espera time.Duration
	if len(l.burst) >= l.limits.BurstN {
		if d := l.burst[0].Add(l.limits.BurstWindow).Sub(agora); d > espera {
			espera = d
		}
	}
	if len(l.sust) >= l.limits.SustainedN {
		if d := l.sust[0].Add(l.limits.SustWindow).Sub(agora); d > espera {
			espera = d
		}
	}
	if espera > 0 {
		return espera
	}

	l.burst = append(l.burst, agora)
	l.sust = append(l.sust, agora)
	return 0
}

func purge(ts []time.Time, corte time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(corte) {
		i++
	}
	return ts[i:]
}

// Observe realinha o balde sustentado com o que o ERP disse.
//
// Os cabeçalhos NÃO vêm sempre: em 40 escritas sequenciais medidas em 26/08 eles
// vieram ausentes, e na rajada vieram preenchidos. Por isso o limiter funciona
// sozinho e só se ajusta quando há dado — nunca depende de o header existir.
func (l *Limiter) Observe(remaining int, reset time.Duration) {
	if remaining > 0 || reset <= 0 {
		return
	}
	// Remaining zero: o balde acabou. Empurra a janela para o reset anunciado.
	l.mu.Lock()
	defer l.mu.Unlock()
	limite := l.now().Add(reset).Add(-l.limits.SustWindow)
	l.sust = l.sust[:0]
	for i := 0; i < l.limits.SustainedN; i++ {
		l.sust = append(l.sust, limite)
	}
}

type errDeadlineTooShort struct{ espera, restante time.Duration }

func (e errDeadlineTooShort) Error() string {
	return "a janela de rate limit (" + e.espera.String() + ") não cabe no prazo restante (" + e.restante.String() + ")"
}

func wrapNotDispatched(cause error) error { return notDispatched{cause} }

type notDispatched struct{ cause error }

func (n notDispatched) Error() string { return ErrNotDispatched.Error() + ": " + n.cause.Error() }
func (n notDispatched) Unwrap() error { return n.cause }
func (n notDispatched) Is(target error) bool {
	return target == ErrNotDispatched
}
