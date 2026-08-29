package erpwrite

// ERPFalso reproduz o Tiny COM OS DEFEITOS QUE FORAM MEDIDOS na API real em
// 26/08/2026, e não um ERP idealizado. Um simulador educado não prova nada: o
// objetivo aqui é que o nosso pipeline sobreviva ao ERP que existe.
//
// O que foi medido e está modelado:
//
//   1. Saída manual (POST /estoque tipo S) NÃO valida saldo. Quatro simultâneas
//      sobre saldo 1 foram todas aceitas e o saldo terminou em -3.
//   2. lancar-estoque do PEDIDO valida saldo (400 "saldo insuficiente"), mas a
//      guarda "Estoque já lançado." é check-then-act: 3 simultâneos deram
//      204/400/204 e baixaram DUAS vezes um pedido de 1 item.
//   3. estornar-estoque concorrente INFLA: 3 simultâneos devolveram +2.
//   4. PUT /itens concorrente NÃO é last-write-wins: 3 simultâneos deixaram a
//      grade com duas linhas.
//
// Qualquer uma dessas patologias que dispare durante um teste é registrada como
// corrupção. A suíte exige corrupção ZERO — é assim que se prova que a
// serialização do nosso lado cobre a ausência de atomicidade do lado deles.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ERPFalso struct {
	mu sync.Mutex

	saldo   map[string]int  // produto -> saldo físico
	lancado map[string]bool // pedido -> estoque lançado?
	itens   map[string]int  // pedido -> quantidade na grade (1 linha só, se sã)
	linhas  map[string]int  // pedido -> nº de linhas na grade

	// Detector de concorrência POR PEDIDO. Modela a falta de atomicidade real.
	emUso map[string]int

	corrupcoes atomic.Int64
	notas      []string
	notasMu    sync.Mutex

	// Contadores para as asserções.
	saidasAplicadas   atomic.Int64
	lancamentosOK     atomic.Int64
	estornosAplicados atomic.Int64
	vendasExternas    atomic.Int64
}

func NovoERPFalso(saldoInicial map[string]int) *ERPFalso {
	s := make(map[string]int, len(saldoInicial))
	for k, v := range saldoInicial {
		s[k] = v
	}
	return &ERPFalso{
		saldo: s, lancado: map[string]bool{}, itens: map[string]int{},
		linhas: map[string]int{}, emUso: map[string]int{},
	}
}

func (e *ERPFalso) anota(f string, a ...any) {
	e.notasMu.Lock()
	e.notas = append(e.notas, fmt.Sprintf(f, a...))
	e.notasMu.Unlock()
}

func (e *ERPFalso) Corrupcoes() int64 { return e.corrupcoes.Load() }

func (e *ERPFalso) Notas() []string {
	e.notasMu.Lock()
	defer e.notasMu.Unlock()
	return append([]string(nil), e.notas...)
}

// entra/sai detectam duas operações simultâneas no MESMO pedido — a condição em
// que o ERP real perde a corrida.
func (e *ERPFalso) entra(pedido, op string) {
	e.mu.Lock()
	e.emUso[pedido]++
	n := e.emUso[pedido]
	e.mu.Unlock()
	if n > 1 {
		e.corrupcoes.Add(1)
		e.anota("CORRUPÇÃO: %d operações simultâneas no pedido %s (op=%s)", n, pedido, op)
	}
}

func (e *ERPFalso) sai(pedido string) {
	e.mu.Lock()
	e.emUso[pedido]--
	e.mu.Unlock()
}

// Saldo lê o saldo físico.
func (e *ERPFalso) Saldo(produto string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saldo[produto]
}

// SaidaManual é o fluxo ANTIGO: não valida nada. Modelado exatamente como medido.
func (e *ERPFalso) SaidaManual(produto string, qtd int) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saldo[produto] -= qtd // sem piso: o real foi para -3
	e.saidasAplicadas.Add(1)
	if e.saldo[produto] < 0 {
		e.anota("saldo de %s ficou NEGATIVO (%d) — saída manual não valida", produto, e.saldo[produto])
	}
	return e.saldo[produto], nil
}

// VendaExterna é outro canal (Mercado Livre, e-commerce, o próprio lojista)
// consumindo estoque no Tiny enquanto a live roda. É o adversário do teste.
func (e *ERPFalso) VendaExterna(produto string, qtd int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saldo[produto] -= qtd
	e.vendasExternas.Add(1)
}

// ReporEstoque é o balanço.
func (e *ERPFalso) ReporEstoque(produto string, qtd int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saldo[produto] = qtd
}

// PutItens substitui a grade. Sob concorrência o ERP real duplica linha.
func (e *ERPFalso) PutItens(pedido, produto string, qtd int) error {
	e.entra(pedido, "PUT /itens")
	defer e.sai(pedido)
	time.Sleep(time.Duration(qtd%3) * time.Millisecond) // janela para a corrida aparecer

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lancado[pedido] {
		return fmt.Errorf("400 pedido.motivosBloqueio[0]: estoque lançado")
	}
	e.itens[pedido] = qtd
	e.linhas[pedido] = 1
	return nil
}

// LancarEstoque valida saldo (como o real) e tem a guarda NÃO atômica.
func (e *ERPFalso) LancarEstoque(pedido, produto string) error {
	e.entra(pedido, "lancar-estoque")
	defer e.sai(pedido)

	e.mu.Lock()
	jaLancado := e.lancado[pedido]
	qtd := e.itens[pedido]
	saldo := e.saldo[produto]
	e.mu.Unlock()

	if jaLancado {
		return fmt.Errorf("400 Estoque já lançado.")
	}
	// A JANELA: o real checa, respira, e só então grava. É aqui que dois
	// lançamentos simultâneos passam pela guarda.
	time.Sleep(time.Millisecond)

	if saldo < qtd {
		return fmt.Errorf("400 Não é possível integrar o estoque deste pedido pois o saldo em estoque de um ou mais produtos é insuficiente.")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lancado[pedido] {
		// Outro passou pela guarda no meio: no ERP real isso baixa DUAS vezes.
		e.saldo[produto] -= qtd
		e.corrupcoes.Add(1)
		e.anota("DUPLA BAIXA no pedido %s (produto %s)", pedido, produto)
		return nil
	}
	e.lancado[pedido] = true
	e.saldo[produto] -= qtd
	e.lancamentosOK.Add(1)
	return nil
}

// EstornarEstoque devolve. Concorrente, o real INFLA.
func (e *ERPFalso) EstornarEstoque(pedido, produto string) error {
	e.entra(pedido, "estornar-estoque")
	defer e.sai(pedido)

	e.mu.Lock()
	estavaLancado := e.lancado[pedido]
	qtd := e.itens[pedido]
	e.mu.Unlock()

	time.Sleep(time.Millisecond) // a janela

	e.mu.Lock()
	defer e.mu.Unlock()
	if !estavaLancado {
		return nil // no-op idempotente, como o real (204)
	}
	if !e.lancado[pedido] {
		// Já estornado por outro: o real devolve DE NOVO.
		e.saldo[produto] += qtd
		e.corrupcoes.Add(1)
		e.anota("SALDO INFLADO no pedido %s (produto %s)", pedido, produto)
		return nil
	}
	e.lancado[pedido] = false
	e.saldo[produto] += qtd
	e.estornosAplicados.Add(1)
	return nil
}

// Lancado diz se o pedido está com estoque baixado.
func (e *ERPFalso) Lancado(pedido string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lancado[pedido]
}

// Linhas é o nº de linhas da grade — mais de 1 é grade corrompida.
func (e *ERPFalso) Linhas(pedido string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.linhas[pedido]
}
