package erp

// Um ERP de mentira que se comporta como o de verdade.
//
// Cada regra abaixo foi medida contra a conta real em 26/08/2026, com o módulo
// de reserva ativo, e está anotada com o número que a produziu. O objetivo não é
// um duplo conveniente: é um duplo que ERRA do mesmo jeito que o original, para
// que um teste verde aqui signifique alguma coisa.
//
// As quatro leis que ele reproduz:
//
//  1. Criar pedido reserva sem tocar o físico.      saldo 5→5 · reservado 3→4
//  2. PUT /itens reajusta a reserva.                 1→2 un.: reservado 4→5
//  3. Lançar estoque converte reserva em baixa.      saldo 5→4 · reservado 4→3
//     e TRAVA a edição: PUT devolve "estoque lançado".
//  4. Estornar num pedido que só reservou INFLA.     reservado 5→7→9
//     (num pedido lançado, estornar desfaz a baixa e devolve a reserva)
//
// E a que fecha o ciclo: cancelar (situacao=2) devolve a reserva inteira,
// inclusive a inflação da lei 4 — reservado 9→3 numa chamada.

import (
	"context"
	"fmt"
	"sync"

	"livecart/apps/api/internal/integration/providers"
)

type produtoSimulado struct {
	saldo     int // físico
	reservado int
}

func (p produtoSimulado) disponivel() int { return p.saldo - p.reservado }

type pedidoSimulado struct {
	id    string
	itens map[string]int // grade: produto → quantidade
	// reservado é quanto ESTE pedido segura hoje, por produto. Separado da grade
	// de propósito: um estorno indevido infla a reserva sem mexer na grade, e é
	// essa diferença que o cancelamento precisa devolver. Foi medido: um pedido
	// de 2 unidades chegou a segurar 4 depois de um estorno, e cancelá-lo baixou
	// as 4.
	reservado      map[string]int
	situacao       int
	estoqueLancado bool
	marcadores     []string
}

// quantidadeTotal é quanto o pedido segura hoje, somando a grade.
func (p *pedidoSimulado) quantidadeTotal() int {
	total := 0
	for _, q := range p.itens {
		total += q
	}
	return total
}

// erpSimulado implementa providers.ERPProvider com a semântica medida.
type erpSimulado struct {
	providers.ERPProvider

	mu       sync.Mutex
	produtos map[string]*produtoSimulado
	pedidos  map[string]*pedidoSimulado
	proximo  int

	// Contadores — as asserções mais importantes são sobre o que NÃO foi
	// chamado, e para isso é preciso contar.
	criacoes    int
	puts        int
	estornos    int
	lancamentos int
	situacoes   int
	pagamentos  int

	// Falhas injetáveis.
	falharPut     error
	falharCriacao error
	// putsAteFalhar > 0 faz os N primeiros PUTs passarem e o seguinte falhar.
	putsAteFalhar int
	// antesDoPut roda antes de cada PUT — para o teste encenar um ERP lento e
	// estourar o prazo do chamador, ou mexer no carrinho no meio da chamada.
	antesDoPut func()
	// antesDaCriacao roda antes de cada POST /pedidos, pelo mesmo motivo.
	antesDaCriacao func()
}

func novoERPSimulado(saldos map[string]int) *erpSimulado {
	e := &erpSimulado{
		produtos: map[string]*produtoSimulado{},
		pedidos:  map[string]*pedidoSimulado{},
	}
	for id, s := range saldos {
		e.produtos[id] = &produtoSimulado{saldo: s}
	}
	return e
}

func (e *erpSimulado) estoque(produtoID string) produtoSimulado {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.produtos[produtoID]; ok {
		return *p
	}
	return produtoSimulado{}
}

func (e *erpSimulado) pedido(id string) *pedidoSimulado {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pedidos[id]
}

// aplicarGrade move a reserva do que o pedido segurava para o que passará a
// segurar. É a lei 2.
func (e *erpSimulado) aplicarGrade(p *pedidoSimulado, nova map[string]int) {
	for produto, antes := range p.reservado {
		if prod, ok := e.produtos[produto]; ok {
			prod.reservado -= antes
		}
	}
	p.reservado = map[string]int{}
	for produto, depois := range nova {
		if _, ok := e.produtos[produto]; !ok {
			e.produtos[produto] = &produtoSimulado{}
		}
		e.produtos[produto].reservado += depois
		p.reservado[produto] = depois
	}
	p.itens = nova
}

func (e *erpSimulado) CreateOrder(_ context.Context, order providers.ERPOrder) (*providers.OrderResult, error) {
	e.mu.Lock()
	lento := e.antesDaCriacao
	e.mu.Unlock()
	if lento != nil {
		lento()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.falharCriacao != nil {
		return nil, e.falharCriacao
	}
	e.criacoes++
	e.proximo++
	id := fmt.Sprintf("ped-%d", e.proximo)
	p := &pedidoSimulado{id: id, itens: map[string]int{}, reservado: map[string]int{}, situacao: providers.SituacaoAberta}
	grade := map[string]int{}
	for _, it := range order.Items {
		grade[it.ProductID] += it.Quantity
	}
	e.aplicarGrade(p, grade)
	e.pedidos[id] = p
	return &providers.OrderResult{OrderID: id, Status: "created"}, nil
}

func (e *erpSimulado) UpdateOrderItems(ctx context.Context, orderID string, itens []providers.ERPOrderItem) error {
	e.mu.Lock()
	lento := e.antesDoPut
	e.mu.Unlock()
	if lento != nil {
		lento()
		if err := ctx.Err(); err != nil {
			e.mu.Lock()
			e.puts++
			e.mu.Unlock()
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.puts++
	if e.falharPut != nil {
		if e.putsAteFalhar > 0 {
			e.putsAteFalhar--
		} else {
			return e.falharPut
		}
	}
	p := e.pedidos[orderID]
	if p == nil {
		return fmt.Errorf("pedido %s não existe", orderID)
	}
	// Lei 3: pedido com estoque lançado recusa edição. É a ÚNICA forma de
	// descobrir que alguém lançou — o GET do pedido não conta.
	if p.estoqueLancado {
		return providers.ErrOrderStockLaunched
	}
	grade := map[string]int{}
	for _, it := range itens {
		grade[it.ProductID] += it.Quantity
	}
	e.aplicarGrade(p, grade)
	return nil
}

// LaunchOrderStock não está na interface ERPProvider — o LiveCart não lança
// estoque. Existe aqui para o teste poder simular o LOJISTA lançando pelo painel,
// que é o cenário que trava a edição.
func (e *erpSimulado) LaunchOrderStock(_ context.Context, orderID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lancamentos++
	p := e.pedidos[orderID]
	if p == nil || p.estoqueLancado {
		return nil
	}
	// Lei 3: a reserva vira baixa. O disponível NÃO se mexe.
	for produto, q := range p.reservado {
		if prod, ok := e.produtos[produto]; ok {
			prod.saldo -= q
			prod.reservado -= q
		}
		p.reservado[produto] = 0
	}
	p.estoqueLancado = true
	return nil
}

func (e *erpSimulado) ReverseOrderStock(_ context.Context, orderID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.estornos++
	p := e.pedidos[orderID]
	if p == nil {
		return fmt.Errorf("pedido %s não existe", orderID)
	}
	if p.estoqueLancado {
		// Desfaz a baixa e devolve a reserva. Este é o uso legítimo.
		for produto, q := range p.itens {
			if prod, ok := e.produtos[produto]; ok {
				prod.saldo += q
				prod.reservado += q
			}
			p.reservado[produto] += q
		}
		p.estoqueLancado = false
		return nil
	}
	// Lei 4: num pedido que só reservou, estornar INFLA a reserva. Sem teto,
	// a cada chamada. É o comportamento que proíbe estornar por precaução.
	for produto, q := range p.itens {
		if prod, ok := e.produtos[produto]; ok {
			prod.reservado += q
		}
		p.reservado[produto] += q
	}
	return nil
}

func (e *erpSimulado) SetOrderSituacao(_ context.Context, orderID string, situacao int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.situacoes++
	p := e.pedidos[orderID]
	if p == nil {
		return fmt.Errorf("pedido %s não existe", orderID)
	}
	p.situacao = situacao
	if situacao == providers.SituacaoCancelada {
		// Cancelar devolve TUDO que ESTE pedido segurava — inclusive a inflação
		// que um estorno indevido tenha criado. Medido: reservado 9 → 3, onde as
		// 3 pertenciam a outros pedidos.
		for produto, q := range p.reservado {
			if prod, ok := e.produtos[produto]; ok {
				prod.reservado -= q
			}
		}
		if p.estoqueLancado {
			for produto, q := range p.itens {
				if prod, ok := e.produtos[produto]; ok {
					prod.saldo += q
				}
			}
		}
		p.itens = map[string]int{}
		p.reservado = map[string]int{}
		p.estoqueLancado = false
	}
	return nil
}

func (e *erpSimulado) GetOrderSituacao(_ context.Context, orderID string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.pedidos[orderID]
	if p == nil {
		return 0, fmt.Errorf("pedido %s não existe", orderID)
	}
	return p.situacao, nil
}

func (e *erpSimulado) UpdateOrderPayment(_ context.Context, orderID string, _ *providers.ERPOrderPayment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pagamentos++
	if e.pedidos[orderID] == nil {
		return fmt.Errorf("pedido %s não existe", orderID)
	}
	return nil
}

func (e *erpSimulado) AddOrderMarker(_ context.Context, orderID, marker string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.pedidos[orderID]; p != nil {
		p.marcadores = append(p.marcadores, marker)
	}
	return nil
}

func (e *erpSimulado) FindOrderIDByMarker(_ context.Context, marker string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, p := range e.pedidos {
		for _, m := range p.marcadores {
			if m == marker {
				return id, nil
			}
		}
	}
	return "", nil
}

func (e *erpSimulado) GetProductStock(_ context.Context, produtoID string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.produtos[produtoID]
	if !ok {
		return 0, fmt.Errorf("produto %s sem controle de estoque", produtoID)
	}
	return p.disponivel(), nil
}
