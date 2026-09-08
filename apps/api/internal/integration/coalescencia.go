package integration

// Coalescência de trabalho repetido por chave.
//
// Existe por um laço de realimentação medido: cada pedido que o LiveCart cria
// mexe no `reservado` do produto, o ERP dispara um webhook de estoque por causa
// disso, e cada webhook faz o espelho reler o saldo do produto. Numa live de 15
// compradores no mesmo produto são 15 leituras onde UMA bastaria — e elas saem
// do mesmo teto de requisições que a live precisa para criar os pedidos. Foram
// 40 leituras estranguladas numa única simulação.
//
// A regra é a de sempre: enquanto uma execução para aquela chave está rodando,
// as que chegarem não enfileiram — marcam que ficou trabalho e voltam. Quem está
// rodando repete no fim se alguém marcou. Eventos que chegam durante uma
// repetição também precisam de uma leitura posterior.

import (
	"errors"
	"sync"
)

type coalescedor struct {
	mu       sync.Mutex
	rodando  map[string]bool
	pendente map[string]bool
}

func novoCoalescedor() *coalescedor {
	return &coalescedor{rodando: map[string]bool{}, pendente: map[string]bool{}}
}

// Fazer roda fn para a chave, coalescendo chamadas concorrentes.
//
// Devolve false quando a chamada foi absorvida por uma execução em curso — o
// chamador não deve tratar isso como falha: o trabalho dele será feito pela
// repetição de quem está rodando.
func (c *coalescedor) Fazer(chave string, fn func() error) (bool, error) {
	c.mu.Lock()
	if c.rodando[chave] {
		c.pendente[chave] = true
		c.mu.Unlock()
		return false, nil
	}
	c.rodando[chave] = true
	c.mu.Unlock()

	liberado := false
	defer func() {
		if liberado {
			return
		}
		// Também libera a chave se fn entrar em panic.
		c.mu.Lock()
		delete(c.rodando, chave)
		delete(c.pendente, chave)
		c.mu.Unlock()
	}()

	var resultado error
	for {
		resultado = errors.Join(resultado, fn())
		c.mu.Lock()
		repetir := c.pendente[chave]
		delete(c.pendente, chave)
		if !repetir {
			// Observar a ausência de trabalho e liberar a chave são atômicos:
			// um evento novo deve assumir a execução, nunca ser apagado pelo defer.
			delete(c.rodando, chave)
			liberado = true
		}
		c.mu.Unlock()
		if !repetir {
			return true, resultado
		}
	}
}
