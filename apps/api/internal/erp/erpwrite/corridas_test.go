package erpwrite

// A suíte de corridas. Cada cenário é um padrão adversarial distinto, e cada um
// roda com várias sementes de interleaving — semente diferente é ordem de
// execução diferente, então cada uma é um caso real distinto.
//
// O adversário central é o que o cliente vive: o ERP vendendo POR FORA enquanto
// a live acontece. Outro canal (Mercado Livre, e-commerce, o próprio lojista no
// balcão) consome o mesmo estoque, e nada avisa o LiveCart.
//
// As invariantes valem para TODOS os cenários:
//
//   I1  Nunca admitir mais unidades do que o gate local autorizou.
//   I2  Zero corrupção no ERP (dupla baixa, saldo inflado, grade duplicada).
//   I3  Nenhuma escrita fica em estado ambíguo quando ela nunca foi despachada.
//   I4  Todo carrinho pago chega a um desfecho terminal — nunca fica travado.
//   I5  O saldo do ERP no fim é exatamente explicável pelas operações aplicadas.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sementesPorCenario = 50

// gateLocal é o modelo do nosso portão de admissão: o UPDATE atômico
// `WHERE stock >= qty` de product.sql:55-60. Medido em 26/08 com 15 compradoras
// simultâneas sobre 1 unidade — admitiu exatamente 1.
type gateLocal struct{ n atomic.Int64 }

func novoGate(inicial int) *gateLocal {
	g := &gateLocal{}
	g.n.Store(int64(inicial))
	return g
}

// Admitir devolve true só se havia saldo. É atômico por construção.
func (g *gateLocal) Admitir(qtd int) bool {
	for {
		atual := g.n.Load()
		if atual < int64(qtd) {
			return false
		}
		if g.n.CompareAndSwap(atual, atual-int64(qtd)) {
			return true
		}
	}
}

func (g *gateLocal) Devolver(qtd int) { g.n.Add(int64(qtd)) }
func (g *gateLocal) Atual() int       { return int(g.n.Load()) }

// mundo junta as peças reais do pipeline com o ERP defeituoso.
type mundo struct {
	erp  *ERPFalso
	fila *Queue
	lim  *Limiter
	gate *gateLocal

	admitidas    atomic.Int64
	naoAplicado  atomic.Int64 // desfechos repetíveis
	ambiguo      atomic.Int64 // desfechos que travariam carrinho
	aplicado     atomic.Int64
	pagosAbertos atomic.Int64
}

func novoMundo(saldoERP, saldoLocal int) *mundo {
	return &mundo{
		erp:  NovoERPFalso(map[string]int{"P": saldoERP}),
		fila: NewQueue(4),
		lim:  NewLimiter(Limits{BurstN: 4, BurstWindow: 20 * time.Millisecond, SustainedN: 10000, SustWindow: time.Minute}),
		gate: novoGate(saldoLocal),
	}
}

// escrever é o caminho ÚNICO de escrita no ERP: limiter → fila serial → execução
// → classificação. Espelha o que o pipeline faz em produção.
func (m *mundo) escrever(ctx context.Context, pedido string, op func() error) Outcome {
	if err := m.lim.Wait(ctx); err != nil {
		o := Classify(Attempt{Dispatched: false, Err: err})
		m.contar(o)
		return o
	}
	var despachou bool
	err := m.fila.Do(ctx, pedido, func(context.Context) error {
		despachou = true
		return op()
	})
	o := Classify(Attempt{Dispatched: despachou, Err: err})
	m.contar(o)
	return o
}

func (m *mundo) contar(o Outcome) {
	switch o {
	case Applied:
		m.aplicado.Add(1)
	case NotApplied:
		m.naoAplicado.Add(1)
	default:
		m.ambiguo.Add(1)
	}
}

// compradora é o fluxo de uma compradora: admissão local → reserva no ERP.
func (m *mundo) compradora(ctx context.Context, i int, qtd int) {
	if !m.gate.Admitir(qtd) {
		return // vai para a fila de espera; correto.
	}
	m.admitidas.Add(int64(qtd))
	pedido := fmt.Sprintf("ped-%d", i)
	o := m.escrever(ctx, pedido, func() error {
		return m.erp.PutItens(pedido, "P", qtd)
	})
	if o == NotApplied {
		// Repetível: devolve a admissão, a compradora volta para a fila.
		m.gate.Devolver(qtd)
		m.admitidas.Add(-int64(qtd))
	}
}

// verificar aplica as invariantes.
func (m *mundo) verificar(t *testing.T, cenario string, semente int64) {
	t.Helper()
	if c := m.erp.Corrupcoes(); c != 0 {
		t.Errorf("[%s semente=%d] I2 violada: %d corrupção(ões) no ERP\n   %v",
			cenario, semente, c, m.erp.Notas())
	}
	if m.gate.Atual() < 0 {
		t.Errorf("[%s semente=%d] I1 violada: gate local ficou negativo (%d)",
			cenario, semente, m.gate.Atual())
	}
	if m.pagosAbertos.Load() != 0 {
		t.Errorf("[%s semente=%d] I4 violada: %d carrinho(s) pago(s) sem desfecho",
			cenario, semente, m.pagosAbertos.Load())
	}
}

// cenario é um padrão adversarial.
type cenario struct {
	nome  string
	rodar func(t *testing.T, m *mundo, r *rand.Rand)
}

func ctxCurto(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// paralelo roda n funções ao mesmo tempo, com jitter da semente.
func paralelo(n int, r *rand.Rand, f func(i int)) {
	var wg sync.WaitGroup
	inicio := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		atraso := time.Duration(r.Intn(3)) * time.Millisecond
		go func(i int, d time.Duration) {
			defer wg.Done()
			<-inicio
			time.Sleep(d)
			f(i)
		}(i, atraso)
	}
	close(inicio)
	wg.Wait()
}

var cenarios = []cenario{
	{"01_duas_compradoras_uma_unidade", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(2 * time.Second)
		defer c()
		paralelo(2, r, func(i int) { m.compradora(ctx, i, 1) })
		if m.admitidas.Load() > 1 {
			t.Errorf("admitiu %d sobre estoque 1", m.admitidas.Load())
		}
	}},
	{"02_dez_compradoras_uma_unidade", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		paralelo(10, r, func(i int) { m.compradora(ctx, i, 1) })
		if m.admitidas.Load() > 1 {
			t.Errorf("admitiu %d sobre estoque 1", m.admitidas.Load())
		}
	}},
	{"03_cinquenta_compradoras_uma_unidade", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(5 * time.Second)
		defer c()
		paralelo(50, r, func(i int) { m.compradora(ctx, i, 1) })
		if m.admitidas.Load() > 1 {
			t.Errorf("admitiu %d sobre estoque 1", m.admitidas.Load())
		}
	}},
	{"04_venda_externa_durante_admissao", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		go m.erp.VendaExterna("P", 1)
		paralelo(5, r, func(i int) { m.compradora(ctx, i, 1) })
	}},
	{"05_venda_externa_repetida", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		go func() {
			for k := 0; k < 5; k++ {
				m.erp.VendaExterna("P", 1)
				time.Sleep(time.Millisecond)
			}
		}()
		paralelo(8, r, func(i int) { m.compradora(ctx, i, 1) })
	}},
	{"06_lancamento_concorrente_mesmo_pedido", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-X", "P", 1)
		paralelo(3, r, func(int) {
			m.escrever(ctx, "ped-X", func() error { return m.erp.LancarEstoque("ped-X", "P") })
		})
		if m.erp.Saldo("P") < 0 {
			t.Errorf("saldo negativo: %d", m.erp.Saldo("P"))
		}
	}},
	{"07_estorno_concorrente_mesmo_pedido", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-Y", "P", 1)
		_ = m.erp.LancarEstoque("ped-Y", "P")
		antes := m.erp.Saldo("P")
		paralelo(3, r, func(int) {
			m.escrever(ctx, "ped-Y", func() error { return m.erp.EstornarEstoque("ped-Y", "P") })
		})
		if d := m.erp.Saldo("P") - antes; d > 1 {
			t.Errorf("estorno concorrente inflou o saldo em %d", d)
		}
	}},
	{"08_put_itens_concorrente", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		paralelo(3, r, func(i int) {
			m.escrever(ctx, "ped-Z", func() error { return m.erp.PutItens("ped-Z", "P", i+1) })
		})
		if n := m.erp.Linhas("ped-Z"); n > 1 {
			t.Errorf("grade com %d linhas", n)
		}
	}},
	{"09_put_e_lancar_concorrentes", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-A", "P", 1)
		paralelo(4, r, func(i int) {
			if i%2 == 0 {
				m.escrever(ctx, "ped-A", func() error { return m.erp.PutItens("ped-A", "P", 2) })
			} else {
				m.escrever(ctx, "ped-A", func() error { return m.erp.LancarEstoque("ped-A", "P") })
			}
		})
	}},
	{"10_estornar_e_lancar_concorrentes", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-B", "P", 1)
		paralelo(6, r, func(i int) {
			if i%2 == 0 {
				m.escrever(ctx, "ped-B", func() error { return m.erp.LancarEstoque("ped-B", "P") })
			} else {
				m.escrever(ctx, "ped-B", func() error { return m.erp.EstornarEstoque("ped-B", "P") })
			}
		})
	}},
	{"11_prazo_curto_nao_vira_ambiguo", func(t *testing.T, m *mundo, r *rand.Rand) {
		for i := 0; i < 4; i++ {
			_ = m.lim.Wait(context.Background())
		}
		ctx, c := ctxCurto(2 * time.Millisecond)
		defer c()
		o := m.escrever(ctx, "ped-C", func() error { return nil })
		if o == Unknown {
			t.Error("prazo curto virou ambíguo — travaria o carrinho")
		}
	}},
	{"12_rajada_estoura_o_limiter", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		paralelo(30, r, func(i int) {
			m.escrever(ctx, fmt.Sprintf("ped-%d", i), func() error { return nil })
		})
		if m.ambiguo.Load() != 0 {
			t.Errorf("%d ambíguos numa rajada", m.ambiguo.Load())
		}
	}},
	{"13_venda_externa_zera_antes_do_lancamento", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-D", "P", 2)
		m.erp.VendaExterna("P", 100) // externo levou tudo
		o := m.escrever(ctx, "ped-D", func() error { return m.erp.LancarEstoque("ped-D", "P") })
		if o == Applied && m.erp.Saldo("P") < -100 {
			t.Error("lançou sobre saldo inexistente")
		}
	}},
	{"14_reposicao_externa_durante_a_live", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		go func() { time.Sleep(2 * time.Millisecond); m.erp.ReporEstoque("P", 50) }()
		paralelo(10, r, func(i int) { m.compradora(ctx, i, 1) })
	}},
	{"15_mesma_compradora_varios_comentarios", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		paralelo(8, r, func(int) {
			m.escrever(ctx, "ped-mesma", func() error { return m.erp.PutItens("ped-mesma", "P", 1+r.Intn(3)) })
		})
		if n := m.erp.Linhas("ped-mesma"); n > 1 {
			t.Errorf("grade com %d linhas", n)
		}
	}},
	{"16_cancelamento_durante_lancamento", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-E", "P", 1)
		paralelo(4, r, func(i int) {
			if i%2 == 0 {
				m.escrever(ctx, "ped-E", func() error { return m.erp.LancarEstoque("ped-E", "P") })
			} else {
				m.escrever(ctx, "ped-E", func() error { return m.erp.EstornarEstoque("ped-E", "P") })
			}
		})
	}},
	{"17_admissao_e_venda_externa_alternadas", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		paralelo(20, r, func(i int) {
			if i%3 == 0 {
				m.erp.VendaExterna("P", 1)
				return
			}
			m.compradora(ctx, i, 1)
		})
	}},
	{"18_carrinho_com_muitos_itens", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		paralelo(12, r, func(i int) {
			m.escrever(ctx, "ped-grande", func() error { return m.erp.PutItens("ped-grande", "P", i+1) })
		})
		if n := m.erp.Linhas("ped-grande"); n > 1 {
			t.Errorf("grade com %d linhas", n)
		}
	}},
	{"19_pedidos_distintos_em_paralelo", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		paralelo(25, r, func(i int) {
			p := fmt.Sprintf("ped-%d", i)
			m.escrever(ctx, p, func() error { return m.erp.PutItens(p, "P", 1) })
		})
	}},
	{"20_retry_do_nao_aplicado", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		var tentativas atomic.Int64
		for k := 0; k < 3; k++ {
			o := m.escrever(ctx, "ped-F", func() error { tentativas.Add(1); return nil })
			if o != Applied {
				t.Errorf("tentativa %d: %s", k, o)
			}
		}
		if tentativas.Load() != 3 {
			t.Errorf("executou %d vezes", tentativas.Load())
		}
	}},
	{"21_estoque_zero_desde_o_inicio", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		m.gate = novoGate(0)
		paralelo(10, r, func(i int) { m.compradora(ctx, i, 1) })
		if m.admitidas.Load() != 0 {
			t.Errorf("admitiu %d com estoque zero", m.admitidas.Load())
		}
	}},
	{"22_quantidade_maior_que_saldo", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		paralelo(5, r, func(i int) { m.compradora(ctx, i, 10) })
		if m.gate.Atual() < 0 {
			t.Error("gate negativo")
		}
	}},
	{"23_venda_externa_negativa_no_erp", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		m.erp.VendaExterna("P", 999)
		paralelo(6, r, func(i int) { m.compradora(ctx, i, 1) })
	}},
	{"24_lancar_estornar_lancar_serial", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(3 * time.Second)
		defer c()
		_ = m.erp.PutItens("ped-G", "P", 1)
		m.escrever(ctx, "ped-G", func() error { return m.erp.LancarEstoque("ped-G", "P") })
		m.escrever(ctx, "ped-G", func() error { return m.erp.EstornarEstoque("ped-G", "P") })
		m.escrever(ctx, "ped-G", func() error { return m.erp.LancarEstoque("ped-G", "P") })
		if !m.erp.Lancado("ped-G") {
			t.Error("pedido deveria terminar lançado")
		}
	}},
	{"25_muitas_compradoras_estoque_medio", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(5 * time.Second)
		defer c()
		m.gate = novoGate(10)
		paralelo(40, r, func(i int) { m.compradora(ctx, i, 1) })
		if m.admitidas.Load() > 10 {
			t.Errorf("admitiu %d sobre 10", m.admitidas.Load())
		}
	}},
	{"26_todas_pedem_duas_unidades", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		m.gate = novoGate(5)
		paralelo(20, r, func(i int) { m.compradora(ctx, i, 2) })
		if m.admitidas.Load() > 5 {
			t.Errorf("admitiu %d sobre 5", m.admitidas.Load())
		}
	}},
	{"27_prazo_expira_no_meio_da_rajada", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(15 * time.Millisecond)
		defer c()
		paralelo(20, r, func(i int) {
			m.escrever(ctx, fmt.Sprintf("ped-%d", i), func() error { return nil })
		})
		if m.ambiguo.Load() != 0 {
			t.Errorf("%d ambíguos — prazo na fila não pode travar carrinho", m.ambiguo.Load())
		}
	}},
	{"28_erp_lento_intermitente", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(5 * time.Second)
		defer c()
		paralelo(15, r, func(i int) {
			p := fmt.Sprintf("ped-%d", i)
			m.escrever(ctx, p, func() error {
				time.Sleep(time.Duration(r.Intn(4)) * time.Millisecond)
				return m.erp.PutItens(p, "P", 1)
			})
		})
	}},
	{"29_venda_externa_e_lancamentos_juntos", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		for i := 0; i < 5; i++ {
			_ = m.erp.PutItens(fmt.Sprintf("ped-%d", i), "P", 1)
		}
		paralelo(10, r, func(i int) {
			if i%2 == 0 {
				m.erp.VendaExterna("P", 1)
				return
			}
			p := fmt.Sprintf("ped-%d", i/2)
			m.escrever(ctx, p, func() error { return m.erp.LancarEstoque(p, "P") })
		})
	}},
	{"30_tudo_ao_mesmo_tempo", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(6 * time.Second)
		defer c()
		m.gate = novoGate(8)
		paralelo(40, r, func(i int) {
			switch i % 4 {
			case 0:
				m.compradora(ctx, i, 1)
			case 1:
				m.erp.VendaExterna("P", 1)
			case 2:
				p := fmt.Sprintf("ped-%d", i%7)
				m.escrever(ctx, p, func() error { return m.erp.PutItens(p, "P", 1) })
			case 3:
				p := fmt.Sprintf("ped-%d", i%7)
				m.escrever(ctx, p, func() error { return m.erp.LancarEstoque(p, "P") })
			}
		})
	}},
	{"31_reposicao_e_esvaziamento_alternados", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(4 * time.Second)
		defer c()
		go func() {
			for k := 0; k < 8; k++ {
				if k%2 == 0 {
					m.erp.ReporEstoque("P", 20)
				} else {
					m.erp.VendaExterna("P", 15)
				}
				time.Sleep(time.Millisecond)
			}
		}()
		paralelo(15, r, func(i int) { m.compradora(ctx, i, 1) })
	}},
	{"32_mesmo_pedido_de_muitos_workers", func(t *testing.T, m *mundo, r *rand.Rand) {
		ctx, c := ctxCurto(5 * time.Second)
		defer c()
		paralelo(30, r, func(i int) {
			m.escrever(ctx, "ped-unico", func() error {
				if i%3 == 0 {
					return m.erp.LancarEstoque("ped-unico", "P")
				}
				if i%3 == 1 {
					return m.erp.EstornarEstoque("ped-unico", "P")
				}
				return m.erp.PutItens("ped-unico", "P", 1)
			})
		})
		if n := m.erp.Linhas("ped-unico"); n > 1 {
			t.Errorf("grade com %d linhas", n)
		}
	}},
}

// TestCorridas roda todo cenário com várias sementes de interleaving.
func TestCorridas(t *testing.T) {
	for _, c := range cenarios {
		c := c
		t.Run(c.nome, func(t *testing.T) {
			for s := 0; s < sementesPorCenario; s++ {
				s := s
				t.Run(fmt.Sprintf("semente_%02d", s), func(t *testing.T) {
					t.Parallel()
					m := novoMundo(20, 1)
					c.rodar(t, m, rand.New(rand.NewSource(int64(s)*7919+1)))
					m.verificar(t, c.nome, int64(s))
				})
			}
		})
	}
}
