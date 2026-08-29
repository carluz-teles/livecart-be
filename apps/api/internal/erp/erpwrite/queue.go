package erpwrite

import (
	"context"
	"sync"
)

// Queue serializa as escritas de um MESMO pedido e limita a concorrência global.
//
// Não é preferência de estilo — é consequência de três corridas medidas contra a
// API real em 26/08/2026, todas num único pedido:
//
//	3× lancar-estoque simultâneos   → 204 / 400 "Estoque já lançado." / 204
//	                                  e o saldo caiu 2 num pedido de 1 item.
//	3× estornar-estoque simultâneos → três 204, e o saldo INFLOU em 2.
//	3× PUT /itens simultâneos       → três 204, e a grade final ficou com DUAS
//	                                  linhas. Não é last-write-wins.
//
// A guarda "Estoque já lançado." do ERP é check-then-act e perde a corrida. Como
// não dá para consertar o lado deles, a serialização tem de ser nossa.
//
// O teto global existe porque o balde de rajada da API é de 4 requisições por
// segundo: passar disso só produz 429.
type Queue struct {
	mu    sync.Mutex
	locks map[string]*chaveEmUso
	sem   chan struct{}
}

type chaveEmUso struct {
	ch       chan struct{} // canal de capacidade 1 = mutex cancelável
	refCount int
}

// NewQueue cria a fila. concorrenciaGlobal <= 0 usa o balde de rajada medido.
func NewQueue(concorrenciaGlobal int) *Queue {
	if concorrenciaGlobal <= 0 {
		concorrenciaGlobal = DefaultLimits().BurstN
	}
	return &Queue{
		locks: make(map[string]*chaveEmUso),
		sem:   make(chan struct{}, concorrenciaGlobal),
	}
}

// Do executa fn com exclusividade sobre `key`, respeitando o teto global.
//
// Se o contexto expirar ENQUANTO ESPERA a vez, devolve ErrNotDispatched: nada
// foi executado, e o chamador precisa saber disso para poder repetir com
// segurança. É a mesma lição do limiter, aplicada à espera pela chave.
func (q *Queue) Do(ctx context.Context, key string, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return wrapNotDispatched(err)
	}

	lock := q.adquirir(key)
	defer q.liberar(key)

	select {
	case lock.ch <- struct{}{}:
		defer func() { <-lock.ch }()
	case <-ctx.Done():
		return wrapNotDispatched(ctx.Err())
	}

	select {
	case q.sem <- struct{}{}:
		defer func() { <-q.sem }()
	case <-ctx.Done():
		return wrapNotDispatched(ctx.Err())
	}

	// A partir daqui a execução COMEÇOU. Um erro de fn não é mais
	// "não despachado" por definição — quem classifica é o Classify, com o
	// que fn souber sobre ter enviado ou não.
	return fn(ctx)
}

func (q *Queue) adquirir(key string) *chaveEmUso {
	q.mu.Lock()
	defer q.mu.Unlock()
	l, ok := q.locks[key]
	if !ok {
		l = &chaveEmUso{ch: make(chan struct{}, 1)}
		q.locks[key] = l
	}
	l.refCount++
	return l
}

// liberar remove a chave quando ninguém mais a espera, para o mapa não crescer
// com um pedido por live.
func (q *Queue) liberar(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	l, ok := q.locks[key]
	if !ok {
		return
	}
	l.refCount--
	if l.refCount <= 0 {
		delete(q.locks, key)
	}
}

// EmVoo diz quantas chaves estão ocupadas. Só para observabilidade e teste.
func (q *Queue) EmVoo() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.locks)
}
