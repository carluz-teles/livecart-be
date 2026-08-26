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
// rodando repete UMA vez no fim se alguém marcou. O resultado é no máximo duas
// execuções por rajada, e a última sempre enxerga o estado final.

import "sync"

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

	defer func() {
		c.mu.Lock()
		delete(c.rodando, chave)
		delete(c.pendente, chave)
		c.mu.Unlock()
	}()

	if err := fn(); err != nil {
		return true, err
	}

	// Alguém chegou enquanto rodávamos: uma repetição basta, porque ela lê o
	// estado final — e é o estado final que interessa.
	c.mu.Lock()
	repetir := c.pendente[chave]
	delete(c.pendente, chave)
	c.mu.Unlock()
	if repetir {
		return true, fn()
	}
	return true, nil
}
