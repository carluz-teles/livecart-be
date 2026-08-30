package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Fixo é um limitador PREDITIVO: espaça as requisições a uma taxa fixa sem
// depender de header nenhum do provedor.
//
// Existe porque o AdaptiveLimiter é inutilizável contra um provedor que não
// devolve cota. Medido contra a API do Bling em 29/08/2026 (23 headers numa
// resposta 200, nenhum de cota; nem X-RateLimit-*, nem Retry-After): sem
// `hasAPIData`, `AdaptiveLimiter.Allow` devolve `{Allowed:true, Remaining:-1}`
// incondicionalmente e PARA SEMPRE, porque `hasAPIData` só vira true em
// `UpdateFromHeaders`. Uma loja Bling naquele limitador sai sem freio.
//
// O teto do Bling é 3 req/s POR CONTA somando TODOS os apps do lojista — se ele
// tem e-commerce ou marketplace no mesmo Bling, eles comem da mesma cota e são
// invisíveis para nós. Por isso o padrão de uso é 2 req/s e não 3: errar para
// menos custa latência, errar para mais custa a venda.
type Fixo struct {
	mu        sync.Mutex
	intervalo time.Duration
	proxima   time.Time

	agora func() time.Time
}

// NovoFixo cria um limitador de `rps` requisições por segundo.
func NovoFixo(rps float64) *Fixo {
	if rps <= 0 {
		rps = 1
	}
	return &Fixo{
		intervalo: time.Duration(float64(time.Second) / rps),
		agora:     time.Now,
	}
}

// Allow reserva a próxima vaga e diz quanto falta para ela.
func (f *Fixo) Allow(context.Context) (*Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	agora := f.agora()
	if f.proxima.Before(agora) {
		f.proxima = agora
	}
	espera := f.proxima.Sub(agora)
	f.proxima = f.proxima.Add(f.intervalo)

	if espera <= 0 {
		return &Reservation{Allowed: true, Remaining: -1}, nil
	}
	return &Reservation{Allowed: false, RetryAfter: espera, Remaining: -1}, nil
}

// Wait bloqueia até a vaga.
//
// ⚠ A propriedade que NÃO PODE se perder: quando a espera não cabe no prazo do
// chamador, `Wait` devolve o erro do contexto SEM DORMIR. É a mesma lição do
// erpwrite.Limiter — uma escrita que ficou na fila até o prazo estourar nunca
// saiu da máquina, e tratá-la como "talvez tenha saído" foi o que produziu os
// 115 `unconfirmed` da live simulada. Quem chama precisa poder distinguir
// "não enviei" de "não sei", e dormir além do prazo apaga essa distinção.
func (f *Fixo) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// UMA reserva por Wait, e só uma. A primeira versão deste método chamava
	// Allow em laço: cada volta reservava uma vaga NOVA e empurrava a fila para
	// frente, então esperar acabava custando mais espera. Um teste de 3 chamadas
	// a 200 req/s levou 12,8 s em vez de 10 ms — o limitador estava competindo
	// consigo mesmo.
	res, err := f.Allow(ctx)
	if err != nil {
		return err
	}
	if res.Allowed {
		return nil
	}

	// A checagem que muda tudo: se a vaga não chega dentro do prazo, é melhor
	// devolver agora, provadamente sem ter enviado.
	if prazo, ok := ctx.Deadline(); ok {
		if restante := prazo.Sub(f.agora()); restante <= res.RetryAfter {
			f.devolverVaga()
			return context.DeadlineExceeded
		}
	}

	t := time.NewTimer(res.RetryAfter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		f.devolverVaga()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// devolverVaga desfaz a reserva de quem desistiu. Sem isso, cada desistência
// deixaria um buraco na grade e a taxa efetiva cairia abaixo do configurado —
// o freio ficaria mais apertado a cada timeout, exatamente quando a live mais
// precisa de vazão.
func (f *Fixo) devolverVaga() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxima = f.proxima.Add(-f.intervalo)
}

// UpdateFromHeaders existe para satisfazer RateLimiter e é NO-OP de propósito.
//
// O Bling não manda header de cota. Se um dia mandar, o lugar de reagir é aqui —
// mas até lá fingir que reagimos seria pior do que não reagir, porque esconderia
// que o freio é uma aposta calibrada e não uma reconciliação.
func (f *Fixo) UpdateFromHeaders(int, int) {}
